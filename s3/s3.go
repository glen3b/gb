package s3

import (
	"bytes"
	"encoding/hex"
	"io"
	"log"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/leijurv/gb/storage_base"
	"github.com/leijurv/gb/utils"
)

// you should change this to wherever your bucket is
const AWSRegion = "us-east-1"

var AWSSession = session.Must(session.NewSession(&aws.Config{Region: aws.String(AWSRegion)}))

// DANGER DANGER DANGER
// EVEN THOUGH the default and RECOMMENDED part size is 5MB, you SHOULD NOT use that
// B E C A U S E when you transition to Glacier Deep Archive, they will REPACK and RECALCULATE the ETag with a chunk size of 16777216
// seriously, try it. upload a file of >16mb to s3 standard, then transition to deep archive. notice the etag changes
// im mad
const s3PartSize = 1 << 24 // this is 16777216

type S3 struct {
	StorageID []byte
	Bucket    string
	RootPath  string
}

type s3Result struct {
	result *s3manager.UploadOutput
	err    error
}

type s3Upload struct {
	calc   *ETagCalculator
	writer *io.PipeWriter
	result chan s3Result
	path   string
	blobID []byte
	s3     *S3
}

func (remote *S3) GetID() []byte {
	return remote.StorageID
}

func (remote *S3) niceRootPath() string {
	path := remote.RootPath
	if path != "" && !strings.HasSuffix(path, "/") {
		path += "/"
	}
	return path
}

func formatPath(blobID []byte) string {
	if len(blobID) != 32 {
		panic(len(blobID))
	}
	h := hex.EncodeToString(blobID)
	return h[:2] + "/" + h[2:4] + "/" + h
}

func makeUploader() *s3manager.Uploader {
	return s3manager.NewUploader(AWSSession, func(u *s3manager.Uploader) {
		u.PartSize = s3PartSize
	})
}

func (remote *S3) BeginBlobUpload(blobID []byte) storage_base.StorageUpload {
	path := remote.niceRootPath() + formatPath(blobID)
	log.Println("Path is", path)
	pipeR, pipeW := io.Pipe()
	resultCh := make(chan s3Result)
	go func() {
		defer pipeR.Close()
		result, err := makeUploader().Upload(&s3manager.UploadInput{
			Bucket: aws.String(remote.Bucket),
			Key:    aws.String(path),
			Body:   pipeR,
		})
		if err != nil {
			log.Println("s3 error", err)
			pipeR.CloseWithError(err)
		}
		resultCh <- s3Result{result, err}
	}()
	return &s3Upload{
		calc:   CreateETagCalculator(),
		writer: pipeW,
		result: resultCh,
		path:   path,
		s3:     remote,
	}
}

func (remote *S3) UploadDatabaseBackup(encryptedDatabase []byte, name string) {
	path := remote.niceRootPath() + name
	result, err := makeUploader().Upload(&s3manager.UploadInput{
		Bucket: aws.String(remote.Bucket),
		Key:    aws.String(path),
		Body:   bytes.NewReader(encryptedDatabase),
	})
	if err != nil {
		panic(err)
	}
	calc := CreateETagCalculator()
	calc.Writer.Write(encryptedDatabase)
	calc.Writer.Close()
	etag := <-calc.Result
	realEtag, realSize := fetchETagAndSize(remote.Bucket, path)
	if realSize != int64(len(encryptedDatabase)) {
		panic("upload length failed")
	}
	if realEtag != etag {
		panic("upload hash failed")
	}
	log.Println("Database backed up to S3. Location:", result.Location)
}

func (remote *S3) Metadata(path string) (string, int64) {
	return fetchETagAndSize(remote.Bucket, path)
}

func (remote *S3) DownloadSection(path string, offset int64, length int64) io.ReadCloser {
	if length == 0 {
		// a range of length 0 is invalid! we get a 400 instead of an empty 200!
		return &utils.EmptyReadCloser{}
	}
	log.Println("S3 key is", path)
	rangeStr := utils.FormatHTTPRange(offset, length)
	log.Println("S3 range is", rangeStr)
	result, err := s3.New(AWSSession).GetObject(&s3.GetObjectInput{
		Bucket: aws.String(remote.Bucket),
		Key:    aws.String(path),
		Range:  aws.String(rangeStr),
	})
	if err != nil {
		panic(err)
	}
	return result.Body
}

func (remote *S3) ListBlobs() []storage_base.UploadedBlob {
	log.Println("Listing blobs in S3")
	files := make([]storage_base.UploadedBlob, 0)
	err := s3.New(AWSSession).ListObjectsPages(&s3.ListObjectsInput{
		Bucket: aws.String(remote.Bucket),
		Prefix: aws.String(remote.niceRootPath()),
	},
		func(page *s3.ListObjectsOutput, lastPage bool) bool {
			for _, obj := range page.Contents {
				if strings.Contains(*obj.Key, "db-backup-") {
					continue // this is not a blob
				}
				etag := *obj.ETag
				etag = etag[1 : len(etag)-1] // aws puts double quotes around the etag lol
				files = append(files, storage_base.UploadedBlob{
					Path:     *obj.Key,
					Checksum: etag,
					Size:     *obj.Size,
				})
			}
			if !lastPage {
				log.Println("Fetched page from S3. Have", len(files), "blobs so far")
			}
			return true
		})
	if err != nil {
		panic(err)
	}
	log.Println("Listed", len(files), "blobs in S3")
	return files
}

func (remote *S3) String() string {
	return "S3 bucket " + remote.Bucket + " at path " + remote.RootPath
}

func (up *s3Upload) Writer() io.Writer {
	return io.MultiWriter(up.calc.Writer, up.writer)
}

func (up *s3Upload) End() storage_base.UploadedBlob {
	up.writer.Close()
	up.calc.Writer.Close()
	result := <-up.result
	if result.err != nil {
		panic(result.err)
	}
	log.Println("Upload output", result.result.Location)
	etag := <-up.calc.Result
	log.Println("Expecting etag", etag)
	realEtag, realSize := fetchETagAndSize(up.s3.Bucket, up.path)
	log.Println("Real etag was", realEtag)
	if etag != realEtag {
		panic("aws broke the etag lmao")
	}
	return storage_base.UploadedBlob{
		Path:     up.path,
		Checksum: etag,
		Size:     realSize,
	}
}

func fetchETagAndSize(bucket string, path string) (string, int64) {
	result, err := s3.New(AWSSession).HeadObject(&s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(path),
	})
	if err != nil {
		panic(err)
	}
	etag := *result.ETag
	etag = etag[1 : len(etag)-1] // aws puts double quotes around the etag lol
	return etag, *result.ContentLength
}
