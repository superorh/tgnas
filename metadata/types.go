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

type CopyOptions struct {
	Overwrite bool
}

type MoveOptions struct {
	Overwrite bool
}

type CopyResult struct {
	Created bool
}

type MoveResult struct {
	Created bool
}

type BucketRename struct {
	Buckets int
	Objects int
	Chunks  int
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
	CopyObject(ctx context.Context, bucket, srcKey, dstKey string, options CopyOptions) (CopyResult, error)
	MoveObject(ctx context.Context, bucket, srcKey, dstKey string, options MoveOptions) (MoveResult, error)
	CopyPrefix(ctx context.Context, bucket, srcPrefix, dstPrefix string, options CopyOptions) (CopyResult, error)
	MovePrefix(ctx context.Context, bucket, srcPrefix, dstPrefix string, options MoveOptions) (MoveResult, error)
	DeletePrefix(ctx context.Context, bucket, prefix string) error
	DeleteBucket(ctx context.Context, bucket string) error
	ListAllObjects(ctx context.Context, bucket, prefix string) ([]Object, error)
	CountObjects(ctx context.Context, bucket, prefix string) (int, error)
	DisableBucketsExcept(ctx context.Context, keepNames []string) error
	CountBucketRenameRows(ctx context.Context, oldName string) (BucketRename, error)
	RenameBucket(ctx context.Context, oldName, newName string) (BucketRename, error)
}
