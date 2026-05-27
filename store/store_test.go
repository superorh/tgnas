package store

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aahl/tgnas/internal/testutil"
	"github.com/aahl/tgnas/metadata"
	"github.com/aahl/tgnas/telegram"
)

func TestStoreHeadBucketUsesStartupConfiguredMetadata(t *testing.T) {
	objectStore, _ := newReadyTestObjectStore(t, map[string]string{"photos": "-100"})
	if err := objectStore.HeadBucket(context.Background(), "photos"); err != nil {
		t.Fatalf("HeadBucket returned error: %v", err)
	}
	if err := objectStore.HeadBucket(context.Background(), "unknown"); err != ErrNoSuchBucket {
		t.Fatalf("err = %v, want ErrNoSuchBucket", err)
	}
}

func TestStoreHeadBucketIgnoresBucketsAddedAfterConstruction(t *testing.T) {
	ctx := context.Background()
	meta, err := metadata.OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: "photos", ChatID: "-100", CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
		t.Fatalf("UpsertBucket returned error: %v", err)
	}
	store := mustNewObjectStore(t, meta, testutil.NewFakeTelegram(), Options{Upload: DefaultUploadConfig()})
	if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: "late", ChatID: "-200", CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
		t.Fatalf("UpsertBucket late returned error: %v", err)
	}

	if err := store.HeadBucket(ctx, "late"); err != ErrNoSuchBucket {
		t.Fatalf("err = %v, want ErrNoSuchBucket", err)
	}
}

func TestStoreHeadBucketRejectsDisabledStartupBucket(t *testing.T) {
	ctx := context.Background()
	meta, err := metadata.OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: "disabled", ChatID: "-300", CreatedAt: time.Now().UTC(), Enabled: false}); err != nil {
		t.Fatalf("UpsertBucket returned error: %v", err)
	}

	store := mustNewObjectStore(t, meta, testutil.NewFakeTelegram(), Options{Upload: DefaultUploadConfig()})
	if err := store.HeadBucket(ctx, "disabled"); err != ErrNoSuchBucket {
		t.Fatalf("err = %v, want ErrNoSuchBucket", err)
	}
}

func TestStorePutHeadDeleteAndList(t *testing.T) {
	ctx := context.Background()
	objectStore, fake := newReadyTestObjectStore(t, map[string]string{"photos": "-100"})
	result, err := objectStore.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: "hello.txt", ContentType: "text/plain", Size: 5, Body: strings.NewReader("hello")})
	if err != nil {
		t.Fatalf("PutObject returned error: %v", err)
	}
	if result.ETag != "5d41402abc4b2a76b9719d911017c592" {
		t.Fatalf("etag = %q", result.ETag)
	}
	if len(fake.Uploads) != 1 || fake.Uploads[0].ChatID != "-100" || fake.Uploads[0].Type != telegram.TypeDocument {
		t.Fatalf("uploads = %+v", fake.Uploads)
	}
	if fake.Uploads[0].Caption != "photos:hello.txt:1:1:" {
		t.Fatalf("caption = %q", fake.Uploads[0].Caption)
	}
	head, err := objectStore.HeadObject(ctx, "photos", "hello.txt")
	if err != nil || head.SHA256 == "" || head.Size != 5 {
		t.Fatalf("head = %+v err = %v", head, err)
	}
	listed, err := objectStore.ListObjects(ctx, ListObjectsInput{Bucket: "photos", Limit: 10})
	if err != nil || len(listed.Objects) != 1 || listed.Objects[0].Key != "hello.txt" {
		t.Fatalf("listed = %+v err = %v", listed, err)
	}
	if err := objectStore.DeleteObject(ctx, "photos", "hello.txt"); err != nil {
		t.Fatalf("DeleteObject returned error: %v", err)
	}
	_, err = objectStore.HeadObject(ctx, "photos", "hello.txt")
	if err != ErrNoSuchKey {
		t.Fatalf("err = %v, want ErrNoSuchKey", err)
	}
}

func TestStorePutZeroByteObjectStoresMetadataWithoutTelegramUpload(t *testing.T) {
	ctx := context.Background()
	meta, err := metadata.OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: "photos", ChatID: "-100", CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
		t.Fatalf("UpsertBucket returned error: %v", err)
	}
	fake := testutil.NewFakeTelegram()
	objectStore := mustNewObjectStore(t, meta, fake, Options{Upload: DefaultUploadConfig()})

	result, err := objectStore.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: "empty.txt", ContentType: "text/plain", Size: 0, Body: strings.NewReader("")})
	if err != nil {
		t.Fatalf("PutObject returned error: %v", err)
	}
	if result.ETag != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Fatalf("etag = %q", result.ETag)
	}
	if len(fake.Uploads) != 0 {
		t.Fatalf("uploads = %+v, want none", fake.Uploads)
	}
	object, chunks, err := meta.GetObject(ctx, "photos", "empty.txt")
	if err != nil {
		t.Fatalf("GetObject returned error: %v", err)
	}
	if object.Size != 0 || object.ChunkCount != 0 || object.ContentType != "text/plain" {
		t.Fatalf("object = %+v", object)
	}
	if len(chunks) != 0 {
		t.Fatalf("chunks = %+v, want none", chunks)
	}
}

func TestStoreChunkedPutAndFullGet(t *testing.T) {
	ctx := context.Background()
	objectStore, fake := newReadyTestObjectStoreWithUploadConfig(t, map[string]string{"backups": "-200"}, UploadConfig{Strategy: "document", EnableChunking: true, MaxFileSize: 50, ChunkSize: 3, TypeLimits: map[string]int64{"document": 3}, PutBufferSize: 2})
	_, err := objectStore.PutObject(ctx, PutObjectInput{Bucket: "backups", Key: "big.bin", ContentType: "application/octet-stream", Size: 8, Body: strings.NewReader("abcdefgh")})
	if err != nil {
		t.Fatalf("PutObject returned error: %v", err)
	}
	if len(fake.Uploads) != 3 {
		t.Fatalf("uploads = %d", len(fake.Uploads))
	}
	reader, head, err := objectStore.GetObject(ctx, GetObjectInput{Bucket: "backups", Key: "big.bin"})
	if err != nil {
		t.Fatalf("GetObject returned error: %v", err)
	}
	defer reader.Close()
	data, _ := io.ReadAll(reader)
	if string(data) != "abcdefgh" || head.Size != 8 {
		t.Fatalf("data = %q head = %+v", string(data), head)
	}
}

func TestStoreRangeGetSingleFile(t *testing.T) {
	ctx := context.Background()
	objectStore, _ := newReadyTestObjectStore(t, map[string]string{"photos": "-100"})
	_, _ = objectStore.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: "letters.txt", ContentType: "text/plain", Size: 8, Body: strings.NewReader("abcdefgh")})
	byteRange := ByteRange{Start: 2, End: 4}
	reader, _, err := objectStore.GetObject(ctx, GetObjectInput{Bucket: "photos", Key: "letters.txt", Range: &byteRange})
	if err != nil {
		t.Fatalf("GetObject returned error: %v", err)
	}
	defer reader.Close()
	data, _ := io.ReadAll(reader)
	if string(data) != "cde" {
		t.Fatalf("data = %q", string(data))
	}
}

func TestStoreMissingContentLength(t *testing.T) {
	objectStore, _ := newReadyTestObjectStore(t, map[string]string{"photos": "-100"})
	_, err := objectStore.PutObject(context.Background(), PutObjectInput{Bucket: "photos", Key: "x", Size: -1, Body: strings.NewReader("x")})
	if err != ErrMissingContentLength {
		t.Fatalf("err = %v, want ErrMissingContentLength", err)
	}
}

func TestStoreRangeGetChunkedDownloadsOnlyOverlappingChunks(t *testing.T) {
	ctx := context.Background()
	objectStore, fake := newReadyTestObjectStoreWithUploadConfig(t, map[string]string{"backups": "-200"}, UploadConfig{Strategy: "document", EnableChunking: true, MaxFileSize: 50, ChunkSize: 3, TypeLimits: map[string]int64{"document": 3}})
	_, err := objectStore.PutObject(ctx, PutObjectInput{Bucket: "backups", Key: "big.bin", ContentType: "application/octet-stream", Size: 9, Body: strings.NewReader("abcdefghi")})
	if err != nil {
		t.Fatalf("PutObject returned error: %v", err)
	}
	byteRange := ByteRange{Start: 3, End: 5}
	reader, _, err := objectStore.GetObject(ctx, GetObjectInput{Bucket: "backups", Key: "big.bin", Range: &byteRange})
	if err != nil {
		t.Fatalf("GetObject returned error: %v", err)
	}
	defer reader.Close()
	data, _ := io.ReadAll(reader)
	if string(data) != "def" {
		t.Fatalf("data = %q", string(data))
	}
	if len(fake.Downloads) != 1 || fake.Downloads[0] != "file-2" {
		t.Fatalf("downloads = %+v", fake.Downloads)
	}
}

func TestStoreMissingBucketAndKeyErrors(t *testing.T) {
	ctx := context.Background()
	objectStore, _ := newReadyTestObjectStore(t, map[string]string{"photos": "-100"})
	if _, err := objectStore.PutObject(ctx, PutObjectInput{Bucket: "missing", Key: "x", Size: 1, Body: strings.NewReader("x")}); err != ErrNoSuchBucket {
		t.Fatalf("put err = %v, want ErrNoSuchBucket", err)
	}
	if _, _, err := objectStore.GetObject(ctx, GetObjectInput{Bucket: "photos", Key: "missing"}); err != ErrNoSuchKey {
		t.Fatalf("get err = %v, want ErrNoSuchKey", err)
	}
}

func TestStoreDeleteObjectMissingKeyReturnsNil(t *testing.T) {
	ctx := context.Background()
	meta, err := metadata.OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: "photos", ChatID: "-100", CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
		t.Fatalf("UpsertBucket returned error: %v", err)
	}

	store := mustNewObjectStore(t, deleteMissingReturnsNotFoundStore{Store: meta}, testutil.NewFakeTelegram(), Options{Upload: DefaultUploadConfig()})
	if err := store.DeleteObject(ctx, "photos", "missing"); err != nil {
		t.Fatalf("DeleteObject returned error: %v", err)
	}
}

func TestStoreLogsOrphanUploadWhenMetadataCommitFails(t *testing.T) {
	ctx := context.Background()
	meta, err := metadata.OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: "photos", ChatID: "-100", CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
		t.Fatalf("UpsertBucket returned error: %v", err)
	}
	fake := testutil.NewFakeTelegram()
	var logs bytes.Buffer
	store := mustNewObjectStore(t, failingMetadataStore{Store: meta, putErr: errors.New("metadata commit failed: secret_key=123 bot_token=456 detail=retryable")}, fake, Options{Upload: DefaultUploadConfig(), Logger: log.New(&logs, "", 0)})
	_, err = store.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: "hello.txt", ContentType: "text/plain", Size: 5, Body: strings.NewReader("hello")})
	if err == nil {
		t.Fatal("expected PutObject error")
	}
	output := logs.String()
	if !strings.Contains(output, "orphan_upload") || !strings.Contains(output, `bucket="photos"`) || !strings.Contains(output, `key="hello.txt"`) {
		t.Fatalf("log output = %q", output)
	}
	if !strings.Contains(output, `event=metadata_put_object bucket="photos" key="hello.txt" chunk_count=1`) || !strings.Contains(output, `result=error`) {
		t.Fatalf("metadata failure log output = %q", output)
	}
	if !strings.Contains(output, "metadata commit failed") || !strings.Contains(output, "detail=retryable") {
		t.Fatalf("log lost useful diagnostics: %q", output)
	}
	if strings.Contains(output, "456") || strings.Contains(output, "123") {
		t.Fatalf("log leaked secret material: %q", output)
	}
	if !strings.Contains(output, "secret_key=[REDACTED]") || !strings.Contains(output, "bot_token=[REDACTED]") {
		t.Fatalf("log did not redact assignments: %q", output)
	}
}

func TestStoreLogsOrphanUploadWhenChunkedUploadFailsAfterEarlierChunks(t *testing.T) {
	ctx := context.Background()
	meta, err := metadata.OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: "backups", ChatID: "-200", CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
		t.Fatalf("UpsertBucket returned error: %v", err)
	}
	fake := testutil.NewFakeTelegram()
	uploadErr := errors.New("chunk upload failed: secret_key=123 bot_token=456 detail=retryable")
	var uploadCalls atomic.Int32
	fake.UploadFunc = func(ctx context.Context, request telegram.UploadRequest) (telegram.UploadedFile, error) {
		_, err := io.ReadAll(request.Reader)
		if err != nil {
			return telegram.UploadedFile{}, err
		}
		call := uploadCalls.Add(1)
		if call == 1 {
			return telegram.UploadedFile{Type: request.Type, FileID: "chunk-file-1", FileUniqueID: "chunk-file-1-u", MessageID: 101, FileSize: 3, MIMEType: request.MIMEType}, nil
		}
		return telegram.UploadedFile{}, uploadErr
	}
	var logs bytes.Buffer
	store := mustNewObjectStore(t, meta, fake, Options{Upload: UploadConfig{Strategy: "document", EnableChunking: true, MaxFileSize: 50, ChunkSize: 3, TypeLimits: map[string]int64{"document": 3}}, Logger: log.New(&logs, "", 0)})

	_, err = store.PutObject(ctx, PutObjectInput{Bucket: "backups", Key: "big.bin", ContentType: "application/octet-stream", Size: 6, Body: strings.NewReader("abcdef")})
	if !errors.Is(err, uploadErr) {
		t.Fatalf("PutObject err = %v, want %v", err, uploadErr)
	}
	output := logs.String()
	if !strings.Contains(output, `event=put_object_decision bucket="backups" key="big.bin" size=6`) || !strings.Contains(output, `chunked=true`) || !strings.Contains(output, `chunk_size=3`) || !strings.Contains(output, `chunk_count=2`) {
		t.Fatalf("decision log output = %q", output)
	}
	if !strings.Contains(output, "orphan_upload") || !strings.Contains(output, `bucket="backups"`) || !strings.Contains(output, `key="big.bin"`) {
		t.Fatalf("log output = %q", output)
	}
	if !strings.Contains(output, "file_id=chun...le-1 message_id=101") {
		t.Fatalf("log missing redacted uploaded chunk details: %q", output)
	}
	if strings.Contains(output, "456") || strings.Contains(output, "123") {
		t.Fatalf("log leaked secret material: %q", output)
	}
	if !strings.Contains(output, "secret_key=[REDACTED]") || !strings.Contains(output, "bot_token=[REDACTED]") {
		t.Fatalf("log did not redact assignments: %q", output)
	}
}

func TestStoreTypedUploadUsesTelegramReturnedFileSize(t *testing.T) {
	ctx := context.Background()
	meta, err := metadata.OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: "photos", ChatID: "-100", CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
		t.Fatalf("UpsertBucket returned error: %v", err)
	}

	fake := testutil.NewFakeTelegram()
	fake.UploadFunc = func(ctx context.Context, request telegram.UploadRequest) (telegram.UploadedFile, error) {
		if request.Type != telegram.TypePhoto {
			return telegram.UploadedFile{}, fmt.Errorf("upload type = %q, want %q", request.Type, telegram.TypePhoto)
		}
		data, err := io.ReadAll(request.Reader)
		if err != nil {
			return telegram.UploadedFile{}, err
		}
		if string(data) != "hello" {
			return telegram.UploadedFile{}, fmt.Errorf("upload payload = %q, want %q", string(data), "hello")
		}
		return telegram.UploadedFile{Type: request.Type, FileID: "photo-file-1", FileUniqueID: "photo-file-1-u", MessageID: 101, FileSize: 3, MIMEType: request.MIMEType}, nil
	}

	store := mustNewObjectStore(t, meta, fake, Options{Upload: UploadConfig{Strategy: "auto", EnableChunking: true, MaxFileSize: 50, ChunkSize: 3, TypeLimits: map[string]int64{"photo": 10, "document": 50}}})
	if _, err := store.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: "hello.jpg", ContentType: "image/jpeg", Size: 5, Body: strings.NewReader("hello")}); err != nil {
		t.Fatalf("PutObject returned error: %v", err)
	}

	object, err := meta.HeadObject(ctx, "photos", "hello.jpg")
	if err != nil {
		t.Fatalf("HeadObject returned error: %v", err)
	}
	if object.Size != 3 {
		t.Fatalf("object size = %d, want %d", object.Size, 3)
	}
	if object.UploadStrategy != "typed" || object.TelegramType != telegram.TypePhoto {
		t.Fatalf("object = %+v", object)
	}
}

func TestStoreTypedUploadETagNotPlainMD5(t *testing.T) {
	ctx := context.Background()
	meta, err := metadata.OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: "photos", ChatID: "-100", CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
		t.Fatalf("UpsertBucket returned error: %v", err)
	}

	fake := testutil.NewFakeTelegram()
	fake.UploadFunc = func(ctx context.Context, request telegram.UploadRequest) (telegram.UploadedFile, error) {
		_, _ = io.ReadAll(request.Reader)
		return telegram.UploadedFile{Type: request.Type, FileID: "photo-f1", FileUniqueID: "photo-f1-u", MessageID: 201, FileSize: 3}, nil
	}

	st := mustNewObjectStore(t, meta, fake, Options{Upload: UploadConfig{Strategy: "auto", EnableChunking: true, MaxFileSize: 50, ChunkSize: 3, TypeLimits: map[string]int64{"photo": 10, "document": 50}}})
	result, err := st.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: "typed.jpg", ContentType: "image/jpeg", Size: 5, Body: strings.NewReader("hello")})
	if err != nil {
		t.Fatalf("PutObject returned error: %v", err)
	}

	if result.ETag == hex.EncodeToString(md5sum([]byte("hello"))) {
		t.Fatalf("typed upload ETag should not be plain MD5 of input bytes: %s", result.ETag)
	}
	if !strings.HasSuffix(result.ETag, "-typed") {
		t.Fatalf("typed upload ETag should have -typed suffix: %s", result.ETag)
	}

	object, err := meta.HeadObject(ctx, "photos", "typed.jpg")
	if err != nil {
		t.Fatalf("HeadObject returned error: %v", err)
	}
	if object.ETag != result.ETag {
		t.Fatalf("stored ETag %q != returned ETag %q", object.ETag, result.ETag)
	}
}

func md5sum(data []byte) []byte {
	h := md5.New()
	h.Write(data)
	return h.Sum(nil)
}

func TestStoreListObjectsDelimiterPagination(t *testing.T) {
	ctx := context.Background()
	objectStore, _ := newReadyTestObjectStore(t, map[string]string{"photos": "-100"})
	for _, key := range []string{"a.txt", "dir1/a.txt", "dir1/b.txt", "dir2/c.txt"} {
		_, err := objectStore.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: key, ContentType: "text/plain", Size: int64(len(key)), Body: strings.NewReader(key)})
		if err != nil {
			t.Fatalf("PutObject(%s) returned error: %v", key, err)
		}
	}
	listed, err := objectStore.ListObjects(ctx, ListObjectsInput{Bucket: "photos", Delimiter: "/", Limit: 2})
	if err != nil {
		t.Fatalf("ListObjects returned error: %v", err)
	}
	if len(listed.Objects) != 1 || listed.Objects[0].Key != "a.txt" {
		t.Fatalf("objects = %+v", listed.Objects)
	}
	if len(listed.CommonPrefixes) != 1 || listed.CommonPrefixes[0] != "dir1/" {
		t.Fatalf("common prefixes = %+v", listed.CommonPrefixes)
	}
	if !listed.IsTruncated || listed.NextContinuationAfter != "dir1/b.txt" {
		t.Fatalf("result = %+v", listed)
	}

	continued, err := objectStore.ListObjects(ctx, ListObjectsInput{Bucket: "photos", Delimiter: "/", Limit: 2, AfterKey: listed.NextContinuationAfter})
	if err != nil {
		t.Fatalf("continued ListObjects returned error: %v", err)
	}
	if len(continued.CommonPrefixes) != 1 || continued.CommonPrefixes[0] != "dir2/" {
		t.Fatalf("continued common prefixes = %+v", continued.CommonPrefixes)
	}
	if len(continued.Objects) != 0 {
		t.Fatalf("continued objects = %+v", continued.Objects)
	}
	if continued.IsTruncated {
		t.Fatalf("continued result = %+v", continued)
	}
}

func TestStoreListObjectsDelimiterCountsObjectsAndUniquePrefixesPerPage(t *testing.T) {
	ctx := context.Background()
	objectStore, _ := newReadyTestObjectStore(t, map[string]string{"photos": "-100"})
	for _, key := range []string{"a.txt", "dir1/a.txt", "dir1/b.txt", "dir2/c.txt"} {
		_, err := objectStore.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: key, ContentType: "text/plain", Size: int64(len(key)), Body: strings.NewReader(key)})
		if err != nil {
			t.Fatalf("PutObject(%s) returned error: %v", key, err)
		}
	}

	listed, err := objectStore.ListObjects(ctx, ListObjectsInput{Bucket: "photos", Delimiter: "/", Limit: 3})
	if err != nil {
		t.Fatalf("ListObjects returned error: %v", err)
	}
	if len(listed.Objects) != 1 || listed.Objects[0].Key != "a.txt" {
		t.Fatalf("objects = %+v", listed.Objects)
	}
	if len(listed.CommonPrefixes) != 2 || listed.CommonPrefixes[0] != "dir1/" || listed.CommonPrefixes[1] != "dir2/" {
		t.Fatalf("common prefixes = %+v", listed.CommonPrefixes)
	}
	if listed.NextContinuationAfter != "dir2/c.txt" {
		t.Fatalf("NextContinuationAfter = %q, want dir2/c.txt", listed.NextContinuationAfter)
	}
}

func TestStoreMaxUploadsSerializationWhenSetToOne(t *testing.T) {
	ctx := context.Background()
	meta, err := metadata.OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: "photos", ChatID: "-100", CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
		t.Fatalf("UpsertBucket returned error: %v", err)
	}
	fake := testutil.NewFakeTelegram()
	started := make(chan string, 2)
	release := make(chan struct{})
	var concurrent atomic.Int32
	var maxConcurrent atomic.Int32
	fake.UploadFunc = func(ctx context.Context, request telegram.UploadRequest) (telegram.UploadedFile, error) {
		name := request.Filename
		current := concurrent.Add(1)
		defer concurrent.Add(-1)
		if current > maxConcurrent.Load() {
			maxConcurrent.Store(current)
		}
		started <- name
		<-release
		_, err := io.ReadAll(request.Reader)
		if err != nil {
			return telegram.UploadedFile{}, err
		}
		return telegram.UploadedFile{Type: request.Type, FileID: name, FileUniqueID: name + "-u", MessageID: 1, FileSize: 1, MIMEType: request.MIMEType}, nil
	}
	caption, _ := telegram.ParseCaptionTemplate("")
	objectStore := mustNewObjectStore(t, meta, fake, Options{Upload: DefaultUploadConfig(), Caption: caption, MaxUploads: 1})

	firstDone := make(chan error, 1)
	go func() {
		_, err := objectStore.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: "one.txt", ContentType: "text/plain", Size: 1, Body: strings.NewReader("x")})
		firstDone <- err
	}()
	if first := <-started; first != "one.txt" {
		t.Fatalf("first upload = %q, want one.txt", first)
	}

	secondCtx, cancelSecond := context.WithCancel(ctx)
	defer cancelSecond()
	secondDone := make(chan error, 1)
	go func() {
		_, err := objectStore.PutObject(secondCtx, PutObjectInput{Bucket: "photos", Key: "two.txt", ContentType: "text/plain", Size: 1, Body: strings.NewReader("x")})
		secondDone <- err
	}()
	cancelSecond()
	if err := <-secondDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("second PutObject err = %v, want context.Canceled", err)
	}
	select {
	case second := <-started:
		t.Fatalf("second upload started before first released: %s", second)
	default:
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first PutObject returned error: %v", err)
	}
	if maxConcurrent.Load() != 1 {
		t.Fatalf("max concurrent uploads = %d", maxConcurrent.Load())
	}
}

func TestStoreMaxTelegramCallsSerializationWhenSetToOne(t *testing.T) {
	ctx := context.Background()
	meta, err := metadata.OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: "photos", ChatID: "-100", CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
		t.Fatalf("UpsertBucket returned error: %v", err)
	}
	fake := testutil.NewFakeTelegram()
	started := make(chan string, 2)
	release := make(chan struct{})
	var uploadCalls atomic.Int32
	fake.UploadFunc = func(ctx context.Context, request telegram.UploadRequest) (telegram.UploadedFile, error) {
		name := request.Filename
		started <- name
		uploadCalls.Add(1)
		<-release
		_, err := io.ReadAll(request.Reader)
		if err != nil {
			return telegram.UploadedFile{}, err
		}
		return telegram.UploadedFile{Type: request.Type, FileID: name, FileUniqueID: name + "-u", MessageID: 1, FileSize: 1, MIMEType: request.MIMEType}, nil
	}
	caption, _ := telegram.ParseCaptionTemplate("")
	objectStore := mustNewObjectStore(t, meta, fake, Options{Upload: DefaultUploadConfig(), Caption: caption, MaxTelegramCalls: 1})

	firstDone := make(chan error, 1)
	go func() {
		_, err := objectStore.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: "one.txt", ContentType: "text/plain", Size: 1, Body: strings.NewReader("x")})
		firstDone <- err
	}()
	if first := <-started; first != "one.txt" {
		t.Fatalf("first upload = %q, want one.txt", first)
	}

	secondCtx, cancelSecond := context.WithCancel(ctx)
	defer cancelSecond()
	secondDone := make(chan error, 1)
	go func() {
		_, err := objectStore.PutObject(secondCtx, PutObjectInput{Bucket: "photos", Key: "two.txt", ContentType: "text/plain", Size: 1, Body: strings.NewReader("x")})
		secondDone <- err
	}()
	cancelSecond()
	if err := <-secondDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("second PutObject err = %v, want context.Canceled", err)
	}
	select {
	case second := <-started:
		t.Fatalf("second telegram upload started before first released: %s", second)
	default:
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first PutObject returned error: %v", err)
	}
	if uploadCalls.Load() != 1 {
		t.Fatalf("telegram upload calls = %d, want 1", uploadCalls.Load())
	}
}

func TestStoreMaxDownloadsSerializationWhenSetToOne(t *testing.T) {
	ctx := context.Background()
	objectStore, fake := newReadyTestObjectStore(t, map[string]string{"photos": "-100"})
	_, err := objectStore.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: "hello.txt", ContentType: "text/plain", Size: 5, Body: strings.NewReader("hello")})
	if err != nil {
		t.Fatalf("PutObject returned error: %v", err)
	}
	started := make(chan string, 2)
	release := make(chan struct{})
	var concurrent atomic.Int32
	var maxConcurrent atomic.Int32
	fake.DownloadFunc = func(ctx context.Context, fileID string) (io.ReadCloser, error) {
		current := concurrent.Add(1)
		defer concurrent.Add(-1)
		if current > maxConcurrent.Load() {
			maxConcurrent.Store(current)
		}
		started <- fileID
		<-release
		return io.NopCloser(strings.NewReader(fake.Files[fileID])), nil
	}
	caption, _ := telegram.ParseCaptionTemplate("")
	meta, err := metadata.OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: "photos", ChatID: "-100", CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
		t.Fatalf("UpsertBucket returned error: %v", err)
	}
	if err := meta.PutObject(ctx, metadata.Object{Bucket: "photos", Key: "hello.txt", Size: 5, ContentType: "text/plain", ETag: "etag", SHA256: "sha", LastModified: time.Now().UTC(), ChunkCount: 1, TelegramType: "document", UploadStrategy: "document"}, []metadata.Chunk{{Bucket: "photos", Key: "hello.txt", PartNumber: 1, Offset: 0, Size: 5, TelegramType: "document", TelegramFileID: "file-1", TelegramMessageID: 1, TelegramFileUniqueID: "u1", SHA256: "sha"}}); err != nil {
		t.Fatalf("PutObject metadata returned error: %v", err)
	}
	serializedStore := mustNewObjectStore(t, meta, fake, Options{Upload: DefaultUploadConfig(), Caption: caption, MaxDownloads: 1})

	firstDone := make(chan error, 1)
	go func() {
		reader, _, err := serializedStore.GetObject(ctx, GetObjectInput{Bucket: "photos", Key: "hello.txt"})
		if err != nil {
			firstDone <- err
			return
		}
		defer reader.Close()
		_, err = io.ReadAll(reader)
		firstDone <- err
	}()
	if first := <-started; first != "file-1" {
		t.Fatalf("first download = %q, want file-1", first)
	}

	secondCtx, cancelSecond := context.WithCancel(ctx)
	defer cancelSecond()
	secondDone := make(chan error, 1)
	go func() {
		_, _, err := serializedStore.GetObject(secondCtx, GetObjectInput{Bucket: "photos", Key: "hello.txt"})
		secondDone <- err
	}()
	cancelSecond()
	if err := <-secondDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("second GetObject err = %v, want context.Canceled", err)
	}
	select {
	case second := <-started:
		t.Fatalf("second download started before first released: %s", second)
	default:
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first GetObject returned error: %v", err)
	}
	if maxConcurrent.Load() != 1 {
		t.Fatalf("max concurrent downloads = %d", maxConcurrent.Load())
	}
}

func TestStoreLogsPutObjectDecisionForSingleUpload(t *testing.T) {
	ctx := context.Background()
	meta, err := metadata.OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: "photos", ChatID: "-100", CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
		t.Fatalf("UpsertBucket returned error: %v", err)
	}
	var logs bytes.Buffer
	store := mustNewObjectStore(t, meta, testutil.NewFakeTelegram(), Options{Upload: DefaultUploadConfig(), Logger: log.New(&logs, "", 0)})

	_, err = store.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: "hello.txt", ContentType: "text/plain", Size: 5, Body: strings.NewReader("hello")})
	if err != nil {
		t.Fatalf("PutObject returned error: %v", err)
	}
	output := logs.String()
	if !strings.Contains(output, `event=put_object_decision bucket="photos" key="hello.txt" size=5`) || !strings.Contains(output, `telegram_type="document"`) || !strings.Contains(output, `strategy="document"`) || !strings.Contains(output, `chunked=false`) || !strings.Contains(output, `chunk_size=0`) || !strings.Contains(output, `chunk_count=1`) {
		t.Fatalf("decision log output = %q", output)
	}
}

func TestStorePutSingleReturnsPromptlyWhenUploadFailsWithoutReading(t *testing.T) {
	ctx := context.Background()
	meta, err := metadata.OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: "photos", ChatID: "-100", CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
		t.Fatalf("UpsertBucket returned error: %v", err)
	}

	uploadErr := errors.New("upload failed early")
	fake := testutil.NewFakeTelegram()
	fake.UploadFunc = func(ctx context.Context, request telegram.UploadRequest) (telegram.UploadedFile, error) {
		return telegram.UploadedFile{}, uploadErr
	}
	store := mustNewObjectStore(t, meta, fake, Options{Upload: DefaultUploadConfig()})

	done := make(chan error, 1)
	go func() {
		_, err := store.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: "hello.txt", ContentType: "text/plain", Size: 5, Body: strings.NewReader("hello")})
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, uploadErr) {
			t.Fatalf("PutObject err = %v, want %v", err, uploadErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PutObject did not return after upload failed without reading")
	}
}

func TestStoreGetObjectCloseReleasesDownloadSlot(t *testing.T) {
	ctx := context.Background()
	objectStore, _ := newReadyTestObjectStore(t, map[string]string{"photos": "-100"})
	payload := strings.Repeat("abcdefgh", 1024)
	_, err := objectStore.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: "hello.txt", ContentType: "text/plain", Size: int64(len(payload)), Body: strings.NewReader(payload)})
	if err != nil {
		t.Fatalf("PutObject returned error: %v", err)
	}

	store := mustNewObjectStore(t, objectStore.meta, objectStore.tg, Options{Upload: DefaultUploadConfig(), MaxDownloads: 1})
	reader, _, err := store.GetObject(ctx, GetObjectInput{Bucket: "photos", Key: "hello.txt"})
	if err != nil {
		t.Fatalf("first GetObject returned error: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("first reader.Close returned error: %v", err)
	}

	secondDone := make(chan error, 1)
	go func() {
		reader, _, err := store.GetObject(ctx, GetObjectInput{Bucket: "photos", Key: "hello.txt"})
		if err != nil {
			secondDone <- err
			return
		}
		defer reader.Close()
		_, err = io.Copy(io.Discard, reader)
		secondDone <- err
	}()

	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("second GetObject returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second GetObject stayed blocked after first reader closed early")
	}
}

func TestStoreReturnsStartupBucketSnapshotFailure(t *testing.T) {
	store, err := NewObjectStore(listBucketsFailStore{err: errors.New("startup secret_key=123 unavailable")}, testutil.NewFakeTelegram(), Options{Upload: DefaultUploadConfig()})
	if err == nil {
		t.Fatal("expected NewObjectStore error")
	}
	if store != nil {
		t.Fatalf("store = %+v, want nil", store)
	}
	message := err.Error()
	if !strings.Contains(message, "startup bucket snapshot") || !strings.Contains(message, "unavailable") {
		t.Fatalf("error = %q", message)
	}
	if strings.Contains(message, "123") {
		t.Fatalf("error leaked secret material: %q", message)
	}
}

func TestSanitizeLogErrorKeepsGenericSecretDiagnostics(t *testing.T) {
	message := sanitizeLogError(errors.New("write failed: secret cache missing during retry"))
	if message != "write failed: secret cache missing during retry" {
		t.Fatalf("sanitizeLogError = %q", message)
	}
}

func TestStoreCompleteMultipartUploadValidatesParts(t *testing.T) {
	ctx := context.Background()
	objectStore, _ := newReadyTestObjectStore(t, map[string]string{"photos": "-100"})
	created, err := objectStore.CreateMultipartUpload(ctx, CreateMultipartUploadInput{Bucket: "photos", Key: "big.bin", ContentType: "application/octet-stream"})
	if err != nil {
		t.Fatalf("CreateMultipartUpload returned error: %v", err)
	}
	part, err := objectStore.UploadPart(ctx, UploadPartInput{Bucket: "photos", Key: "big.bin", UploadID: created.UploadID, PartNumber: 2, Size: 3, Body: strings.NewReader("abc")})
	if err != nil {
		t.Fatalf("UploadPart returned error: %v", err)
	}

	_, err = objectStore.CompleteMultipartUpload(ctx, CompleteMultipartUploadInput{Bucket: "photos", Key: "big.bin", UploadID: created.UploadID, Parts: []CompletedPart{{PartNumber: 2, ETag: part.ETag}, {PartNumber: 1, ETag: part.ETag}}})
	if err != ErrInvalidPartOrder {
		t.Fatalf("order err = %v, want ErrInvalidPartOrder", err)
	}

	_, err = objectStore.CompleteMultipartUpload(ctx, CompleteMultipartUploadInput{Bucket: "photos", Key: "big.bin", UploadID: created.UploadID, Parts: []CompletedPart{{PartNumber: 1, ETag: part.ETag}}})
	if err != ErrInvalidPart {
		t.Fatalf("missing err = %v, want ErrInvalidPart", err)
	}

	_, err = objectStore.CompleteMultipartUpload(ctx, CompleteMultipartUploadInput{Bucket: "photos", Key: "big.bin", UploadID: created.UploadID, Parts: []CompletedPart{{PartNumber: 2, ETag: "bad"}}})
	if err != ErrInvalidPart {
		t.Fatalf("etag err = %v, want ErrInvalidPart", err)
	}
}

func TestStoreMultipartRangeGetAcrossVariableChunks(t *testing.T) {
	ctx := context.Background()
	objectStore, fake := newReadyTestObjectStoreWithUploadConfig(t, map[string]string{"photos": "-100"}, UploadConfig{Strategy: "document", EnableChunking: true, MaxFileSize: 50, ChunkSize: 3, TypeLimits: map[string]int64{"document": 3}, PutBufferSize: 2})
	created, err := objectStore.CreateMultipartUpload(ctx, CreateMultipartUploadInput{Bucket: "photos", Key: "big.bin", ContentType: "application/octet-stream"})
	if err != nil {
		t.Fatalf("CreateMultipartUpload returned error: %v", err)
	}
	first, _ := objectStore.UploadPart(ctx, UploadPartInput{Bucket: "photos", Key: "big.bin", UploadID: created.UploadID, PartNumber: 1, Size: 5, Body: strings.NewReader("abcde")})
	second, _ := objectStore.UploadPart(ctx, UploadPartInput{Bucket: "photos", Key: "big.bin", UploadID: created.UploadID, PartNumber: 2, Size: 4, Body: strings.NewReader("fghi")})
	_, err = objectStore.CompleteMultipartUpload(ctx, CompleteMultipartUploadInput{Bucket: "photos", Key: "big.bin", UploadID: created.UploadID, Parts: []CompletedPart{{PartNumber: 1, ETag: first.ETag}, {PartNumber: 2, ETag: second.ETag}}})
	if err != nil {
		t.Fatalf("CompleteMultipartUpload returned error: %v", err)
	}

	byteRange := ByteRange{Start: 2, End: 6}
	reader, _, err := objectStore.GetObject(ctx, GetObjectInput{Bucket: "photos", Key: "big.bin", Range: &byteRange})
	if err != nil {
		t.Fatalf("GetObject range returned error: %v", err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll returned error: %v", err)
	}
	if string(data) != "cdefg" {
		t.Fatalf("range data = %q", string(data))
	}
	if len(fake.Downloads) == 0 {
		t.Fatalf("downloads = %+v", fake.Downloads)
	}
}

func TestStorePutObjectStillUsesWholeObjectMD5AndSHA256(t *testing.T) {
	ctx := context.Background()
	objectStore, _ := newReadyTestObjectStore(t, map[string]string{"photos": "-100"})
	result, err := objectStore.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: "hello.txt", ContentType: "text/plain", Size: 5, Body: strings.NewReader("hello")})
	if err != nil {
		t.Fatalf("PutObject returned error: %v", err)
	}
	if result.ETag != "5d41402abc4b2a76b9719d911017c592" {
		t.Fatalf("etag = %q", result.ETag)
	}
	head, err := objectStore.HeadObject(ctx, "photos", "hello.txt")
	if err != nil {
		t.Fatalf("HeadObject returned error: %v", err)
	}
	if head.SHA256 != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("sha256 = %q", head.SHA256)
	}
}

func TestStoreAbortMultipartUploadRemovesSession(t *testing.T) {
	ctx := context.Background()
	objectStore, _ := newReadyTestObjectStore(t, map[string]string{"photos": "-100"})
	created, err := objectStore.CreateMultipartUpload(ctx, CreateMultipartUploadInput{Bucket: "photos", Key: "big.bin", ContentType: "application/octet-stream"})
	if err != nil {
		t.Fatalf("CreateMultipartUpload returned error: %v", err)
	}
	if err := objectStore.AbortMultipartUpload(ctx, AbortMultipartUploadInput{Bucket: "photos", Key: "big.bin", UploadID: created.UploadID}); err != nil {
		t.Fatalf("AbortMultipartUpload returned error: %v", err)
	}
	_, err = objectStore.UploadPart(ctx, UploadPartInput{Bucket: "photos", Key: "big.bin", UploadID: created.UploadID, PartNumber: 1, Size: 3, Body: strings.NewReader("abc")})
	if err != ErrNoSuchUpload {
		t.Fatalf("UploadPart err = %v, want ErrNoSuchUpload", err)
	}
}

func TestStoreCreateMultipartUploadEvictsOldestSession(t *testing.T) {
	ctx := context.Background()
	objectStore, _ := newReadyTestObjectStore(t, map[string]string{"photos": "-100"})

	var first string
	for i := 0; i < maxMultipartUploads+1; i++ {
		created, err := objectStore.CreateMultipartUpload(ctx, CreateMultipartUploadInput{Bucket: "photos", Key: fmt.Sprintf("object-%d.bin", i), ContentType: "application/octet-stream"})
		if err != nil {
			t.Fatalf("CreateMultipartUpload(%d) returned error: %v", i, err)
		}
		if i == 0 {
			first = created.UploadID
		}
	}

	_, err := objectStore.UploadPart(ctx, UploadPartInput{Bucket: "photos", Key: "object-0.bin", UploadID: first, PartNumber: 1, Size: 3, Body: strings.NewReader("abc")})
	if err != ErrNoSuchUpload {
		t.Fatalf("UploadPart err = %v, want ErrNoSuchUpload", err)
	}
}

func TestStoreCompleteMultipartUploadCommitsChunksAndMultipartETag(t *testing.T) {
	ctx := context.Background()
	objectStore, fake := newReadyTestObjectStoreWithUploadConfig(t, map[string]string{"photos": "-100"}, UploadConfig{Strategy: "document", EnableChunking: true, MaxFileSize: 50, ChunkSize: 3, TypeLimits: map[string]int64{"document": 3}, PutBufferSize: 2})
	created, err := objectStore.CreateMultipartUpload(ctx, CreateMultipartUploadInput{Bucket: "photos", Key: "big.bin", ContentType: "application/octet-stream"})
	if err != nil {
		t.Fatalf("CreateMultipartUpload returned error: %v", err)
	}
	first, err := objectStore.UploadPart(ctx, UploadPartInput{Bucket: "photos", Key: "big.bin", UploadID: created.UploadID, PartNumber: 1, Size: 5, Body: strings.NewReader("abcde")})
	if err != nil {
		t.Fatalf("UploadPart first returned error: %v", err)
	}
	second, err := objectStore.UploadPart(ctx, UploadPartInput{Bucket: "photos", Key: "big.bin", UploadID: created.UploadID, PartNumber: 2, Size: 4, Body: strings.NewReader("fghi")})
	if err != nil {
		t.Fatalf("UploadPart second returned error: %v", err)
	}

	completed, err := objectStore.CompleteMultipartUpload(ctx, CompleteMultipartUploadInput{Bucket: "photos", Key: "big.bin", UploadID: created.UploadID, Parts: []CompletedPart{{PartNumber: 1, ETag: first.ETag}, {PartNumber: 2, ETag: second.ETag}}})
	if err != nil {
		t.Fatalf("CompleteMultipartUpload returned error: %v", err)
	}
	if completed.ETag != "1c4bb33d6bb358e9305bd0e3f40b1552-2" {
		t.Fatalf("complete etag = %q", completed.ETag)
	}

	reader, info, err := objectStore.GetObject(ctx, GetObjectInput{Bucket: "photos", Key: "big.bin"})
	if err != nil {
		t.Fatalf("GetObject returned error: %v", err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll returned error: %v", err)
	}
	if string(data) != "abcdefghi" {
		t.Fatalf("data = %q", string(data))
	}
	if info.Size != 9 || info.ETag != completed.ETag || info.SHA256 != "" {
		t.Fatalf("info = %+v", info)
	}
	if len(fake.Uploads) != 4 {
		t.Fatalf("uploads = %d, want 4", len(fake.Uploads))
	}
}

func TestStoreUploadPartSplitsIntoTelegramChunks(t *testing.T) {
	ctx := context.Background()
	objectStore, fake := newReadyTestObjectStoreWithUploadConfig(t, map[string]string{"photos": "-100"}, UploadConfig{Strategy: "document", EnableChunking: true, MaxFileSize: 50, ChunkSize: 3, TypeLimits: map[string]int64{"document": 3}, PutBufferSize: 2})
	created, err := objectStore.CreateMultipartUpload(ctx, CreateMultipartUploadInput{Bucket: "photos", Key: "big.bin", ContentType: "application/octet-stream"})
	if err != nil {
		t.Fatalf("CreateMultipartUpload returned error: %v", err)
	}

	result, err := objectStore.UploadPart(ctx, UploadPartInput{Bucket: "photos", Key: "big.bin", UploadID: created.UploadID, PartNumber: 1, Size: 8, Body: strings.NewReader("abcdefgh")})
	if err != nil {
		t.Fatalf("UploadPart returned error: %v", err)
	}
	if result.ETag != "e8dc4081b13434b45189a720b77b6818" {
		t.Fatalf("etag = %q", result.ETag)
	}
	if len(fake.Uploads) != 3 {
		t.Fatalf("uploads = %d, want 3", len(fake.Uploads))
	}
	if fake.Files["file-1"] != "abc" || fake.Files["file-2"] != "def" || fake.Files["file-3"] != "gh" {
		t.Fatalf("files = %+v", fake.Files)
	}
}

func TestStoreCreateMultipartUploadMissingBucket(t *testing.T) {
	ctx := context.Background()
	objectStore, _ := newReadyTestObjectStore(t, map[string]string{"photos": "-100"})

	_, err := objectStore.CreateMultipartUpload(ctx, CreateMultipartUploadInput{Bucket: "missing", Key: "big.bin", ContentType: "application/octet-stream"})
	if err != ErrNoSuchBucket {
		t.Fatalf("err = %v, want ErrNoSuchBucket", err)
	}
}

func TestStoreCreateMultipartUploadReturnsDistinctUploadIDs(t *testing.T) {
	ctx := context.Background()
	objectStore, _ := newReadyTestObjectStore(t, map[string]string{"photos": "-100"})

	first, err := objectStore.CreateMultipartUpload(ctx, CreateMultipartUploadInput{Bucket: "photos", Key: "big.bin", ContentType: "application/octet-stream"})
	if err != nil {
		t.Fatalf("first CreateMultipartUpload returned error: %v", err)
	}
	second, err := objectStore.CreateMultipartUpload(ctx, CreateMultipartUploadInput{Bucket: "photos", Key: "big.bin", ContentType: "application/octet-stream"})
	if err != nil {
		t.Fatalf("second CreateMultipartUpload returned error: %v", err)
	}
	if first.UploadID == "" || second.UploadID == "" || first.UploadID == second.UploadID {
		t.Fatalf("upload ids = %q %q", first.UploadID, second.UploadID)
	}
}

func TestMultipartAPITypesCompile(t *testing.T) {
	create := CreateMultipartUploadInput{Bucket: "photos", Key: "big.bin", ContentType: "application/octet-stream"}
	if create.Bucket != "photos" || create.Key != "big.bin" || create.ContentType == "" {
		t.Fatalf("create input = %+v", create)
	}

	upload := UploadPartInput{Bucket: "photos", Key: "big.bin", UploadID: "upload-1", PartNumber: 1, Size: 3, Body: strings.NewReader("abc")}
	if upload.UploadID == "" || upload.PartNumber != 1 || upload.Size != 3 || upload.Body == nil {
		t.Fatalf("upload input = %+v", upload)
	}

	complete := CompleteMultipartUploadInput{Bucket: "photos", Key: "big.bin", UploadID: "upload-1", Parts: []CompletedPart{{PartNumber: 1, ETag: "etag"}}}
	if len(complete.Parts) != 1 || complete.Parts[0].PartNumber != 1 {
		t.Fatalf("complete input = %+v", complete)
	}

	abort := AbortMultipartUploadInput{Bucket: "photos", Key: "big.bin", UploadID: "upload-1"}
	if abort.UploadID != "upload-1" {
		t.Fatalf("abort input = %+v", abort)
	}

	if !errors.Is(ErrNoSuchUpload, ErrNoSuchUpload) || !errors.Is(ErrInvalidPart, ErrInvalidPart) || !errors.Is(ErrInvalidPartOrder, ErrInvalidPartOrder) || !errors.Is(ErrInvalidArgument, ErrInvalidArgument) {
		t.Fatal("multipart errors must be defined")
	}
}

func newReadyTestObjectStore(t *testing.T, buckets map[string]string) (*ObjectStore, *testutil.FakeTelegram) {
	t.Helper()
	return newReadyTestObjectStoreWithUploadConfig(t, buckets, DefaultUploadConfig())
}

func newReadyTestObjectStoreWithUploadConfig(t *testing.T, buckets map[string]string, upload UploadConfig) (*ObjectStore, *testutil.FakeTelegram) {
	t.Helper()
	ctx := context.Background()
	meta, err := metadata.OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	for name, chatID := range buckets {
		if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: name, ChatID: chatID, CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
			t.Fatalf("UpsertBucket(%s) returned error: %v", name, err)
		}
	}
	fake := testutil.NewFakeTelegram()
	caption, err := telegram.ParseCaptionTemplate("{bucket}:{key}:{part}:{parts}:{chunk}")
	if err != nil {
		t.Fatalf("ParseCaptionTemplate returned error: %v", err)
	}
	return mustNewObjectStore(t, meta, fake, Options{Upload: upload, Caption: caption}), fake
}

func mustNewObjectStore(t *testing.T, meta metadata.Store, tg telegram.Client, options Options) *ObjectStore {
	t.Helper()
	store, err := NewObjectStore(meta, tg, options)
	if err != nil {
		t.Fatalf("NewObjectStore returned error: %v", err)
	}
	return store
}

type failingMetadataStore struct {
	metadata.Store
	putErr error
}

func (s failingMetadataStore) PutObject(ctx context.Context, object metadata.Object, chunks []metadata.Chunk) error {
	return s.putErr
}

type deleteMissingReturnsNotFoundStore struct {
	metadata.Store
}

type listBucketsFailStore struct {
	err error
}

func (s listBucketsFailStore) ListBuckets(ctx context.Context) ([]metadata.Bucket, error) {
	return nil, s.err
}

func (s listBucketsFailStore) UpsertBucket(ctx context.Context, bucket metadata.Bucket) error {
	return errors.New("not implemented")
}

func (s listBucketsFailStore) GetBucket(ctx context.Context, name string) (metadata.Bucket, error) {
	return metadata.Bucket{}, errors.New("not implemented")
}

func (s listBucketsFailStore) HeadObject(ctx context.Context, bucket, key string) (metadata.Object, error) {
	return metadata.Object{}, errors.New("not implemented")
}

func (s listBucketsFailStore) GetObject(ctx context.Context, bucket, key string) (metadata.Object, []metadata.Chunk, error) {
	return metadata.Object{}, nil, errors.New("not implemented")
}

func (s listBucketsFailStore) PutObject(ctx context.Context, object metadata.Object, chunks []metadata.Chunk) error {
	return errors.New("not implemented")
}

func (s listBucketsFailStore) DeleteObject(ctx context.Context, bucket, key string) error {
	return errors.New("not implemented")
}

func (s listBucketsFailStore) ListObjects(ctx context.Context, query metadata.ListQuery) ([]metadata.Object, error) {
	return nil, errors.New("not implemented")
}

func (s listBucketsFailStore) CopyObject(ctx context.Context, bucket, srcKey, dstKey string, options metadata.CopyOptions) (metadata.CopyResult, error) {
	return metadata.CopyResult{}, errors.New("not implemented")
}

func (s listBucketsFailStore) MoveObject(ctx context.Context, bucket, srcKey, dstKey string, options metadata.MoveOptions) (metadata.MoveResult, error) {
	return metadata.MoveResult{}, errors.New("not implemented")
}

func (s listBucketsFailStore) CopyPrefix(ctx context.Context, bucket, srcPrefix, dstPrefix string, options metadata.CopyOptions) (metadata.CopyResult, error) {
	return metadata.CopyResult{}, errors.New("not implemented")
}

func (s listBucketsFailStore) MovePrefix(ctx context.Context, bucket, srcPrefix, dstPrefix string, options metadata.MoveOptions) (metadata.MoveResult, error) {
	return metadata.MoveResult{}, errors.New("not implemented")
}

func (s listBucketsFailStore) DeletePrefix(ctx context.Context, bucket, prefix string) error {
	return errors.New("not implemented")
}

func (s listBucketsFailStore) DeleteBucket(ctx context.Context, bucket string) error {
	return errors.New("not implemented")
}

func (s listBucketsFailStore) ListAllObjects(ctx context.Context, bucket, prefix string) ([]metadata.Object, error) {
	return nil, errors.New("not implemented")
}

func (s listBucketsFailStore) CountObjects(ctx context.Context, bucket, prefix string) (int, error) {
	return 0, errors.New("not implemented")
}

func (s listBucketsFailStore) DisableBucketsExcept(ctx context.Context, keepNames []string) error {
	return errors.New("not implemented")
}

func (s listBucketsFailStore) CountBucketRenameRows(ctx context.Context, oldName string) (metadata.BucketRename, error) {
	return metadata.BucketRename{}, errors.New("not implemented")
}

func (s listBucketsFailStore) RenameBucket(ctx context.Context, oldName, newName string) (metadata.BucketRename, error) {
	return metadata.BucketRename{}, errors.New("not implemented")
}

func (s listBucketsFailStore) Close() error {
	return nil
}

func (s deleteMissingReturnsNotFoundStore) DeleteObject(ctx context.Context, bucket, key string) error {
	if _, err := s.Store.HeadObject(ctx, bucket, key); err != nil {
		if errors.Is(err, metadata.ErrNotFound) {
			return metadata.ErrNotFound
		}
		return err
	}
	return s.Store.DeleteObject(ctx, bucket, key)
}
