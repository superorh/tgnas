package metadata

import (
	"context"
	"errors"
	"time"
)

var ErrNotFound = errors.New("metadata not found")

type Bucket struct {
	Name      string
	ChatID    string
	CreatedAt time.Time
	Enabled   bool
}

type Object struct {
	Bucket         string
	Key            string
	Size           int64
	ContentType    string
	ETag           string
	SHA256         string
	LastModified   time.Time
	ChunkCount     int
	TelegramType   string
	UploadStrategy string
}

type Chunk struct {
	Bucket               string
	Key                  string
	PartNumber           int
	Offset               int64
	Size                 int64
	TelegramType         string
	TelegramFileID       string
	TelegramMessageID    int64
	TelegramFileUniqueID string
	SHA256               string
}

type ListQuery struct {
	Bucket   string
	Prefix   string
	AfterKey string
	Limit    int
}

type Store interface {
	Close() error
	UpsertBucket(ctx context.Context, bucket Bucket) error
	GetBucket(ctx context.Context, name string) (Bucket, error)
	ListBuckets(ctx context.Context) ([]Bucket, error)
	PutObject(ctx context.Context, object Object, chunks []Chunk) error
	GetObject(ctx context.Context, bucket, key string) (Object, []Chunk, error)
	HeadObject(ctx context.Context, bucket, key string) (Object, error)
	ListObjects(ctx context.Context, query ListQuery) ([]Object, error)
	DeleteObject(ctx context.Context, bucket, key string) error
}
