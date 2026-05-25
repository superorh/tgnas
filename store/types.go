package store

import (
	"errors"
	"io"
	"log"
	"time"

	"github.com/aahl/tgnas/telegram"
)

var (
	ErrNotImplemented       = errors.New("not implemented")
	ErrEntityTooLarge       = errors.New("entity too large")
	ErrNoSuchBucket         = errors.New("no such bucket")
	ErrNoSuchKey            = errors.New("no such key")
	ErrMissingContentLength = errors.New("missing content length")
	ErrInvalidRange         = errors.New("invalid range")
	ErrNoSuchUpload         = errors.New("no such upload")
	ErrInvalidPart          = errors.New("invalid part")
	ErrInvalidPartOrder     = errors.New("invalid part order")
	ErrInvalidArgument      = errors.New("invalid argument")
)

type PutObjectInput struct {
	Bucket      string
	Key         string
	ContentType string
	Size        int64
	Body        io.Reader
}

type PutObjectResult struct {
	ETag string
}

type CreateMultipartUploadInput struct {
	Bucket      string
	Key         string
	ContentType string
}

type CreateMultipartUploadResult struct {
	UploadID string
}

type UploadPartInput struct {
	Bucket     string
	Key        string
	UploadID   string
	PartNumber int
	Size       int64
	Body       io.Reader
}

type UploadPartResult struct {
	ETag string
}

type CompletedPart struct {
	PartNumber int
	ETag       string
}

type CompleteMultipartUploadInput struct {
	Bucket   string
	Key      string
	UploadID string
	Parts    []CompletedPart
}

type CompleteMultipartUploadResult struct {
	ETag string
}

type AbortMultipartUploadInput struct {
	Bucket   string
	Key      string
	UploadID string
}

type ObjectInfo struct {
	Bucket       string
	Key          string
	Size         int64
	ContentType  string
	ETag         string
	SHA256       string
	LastModified time.Time
}

type GetObjectInput struct {
	Bucket string
	Key    string
	Range  *ByteRange
}

type ListObjectsInput struct {
	Bucket    string
	Prefix    string
	Delimiter string
	AfterKey  string
	Limit     int
}

type ListObjectsResult struct {
	Objects               []ObjectInfo
	CommonPrefixes        []string
	NextContinuationAfter string
	IsTruncated           bool
}

type UploadConfig struct {
	Strategy       string
	EnableChunking bool
	MaxFileSize    int64
	ChunkSize      int64
	TypeLimits     map[string]int64
	PutBufferSize  int
}

func DefaultUploadConfig() UploadConfig {
	return UploadConfig{
		Strategy:       "document",
		EnableChunking: true,
		MaxFileSize:    50 * 1024 * 1024,
		ChunkSize:      20 * 1024 * 1024,
		TypeLimits: map[string]int64{
			"photo":     10 * 1024 * 1024,
			"video":     20 * 1024 * 1024,
			"audio":     20 * 1024 * 1024,
			"animation": 20 * 1024 * 1024,
			"document":  20 * 1024 * 1024,
		},
		PutBufferSize: 1024 * 1024,
	}
}

type Options struct {
	Upload           UploadConfig
	Caption          *telegram.CaptionTemplate
	MaxUploads       int
	MaxDownloads     int
	MaxTelegramCalls int
	Logger           *log.Logger
}
