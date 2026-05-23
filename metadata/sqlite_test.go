package metadata

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestSQLiteBucketsObjectsAndChunks(t *testing.T) {
	store := openTestSQLiteStore(t)
	defer store.Close()

	bucket := Bucket{
		Name:      "photos",
		ChatID:    "-100123",
		CreatedAt: time.Unix(10, 0),
		Enabled:   true,
	}
	if err := store.UpsertBucket(t.Context(), bucket); err != nil {
		t.Fatalf("UpsertBucket returned error: %v", err)
	}

	gotBucket, err := store.GetBucket(t.Context(), "photos")
	if err != nil {
		t.Fatalf("GetBucket returned error: %v", err)
	}
	if gotBucket != bucket {
		t.Fatalf("bucket = %+v, want %+v", gotBucket, bucket)
	}

	object := Object{
		Bucket:         "photos",
		Key:            "b/cat.jpg",
		Size:           11,
		ContentType:    "image/jpeg",
		ETag:           "5eb63bbbe01eeed093cb22bb8f5acdc3",
		SHA256:         "sha",
		LastModified:   time.Unix(20, 0),
		ChunkCount:     2,
		TelegramType:   "document",
		UploadStrategy: "chunked_document",
	}
	chunks := []Chunk{
		{
			Bucket:               "photos",
			Key:                  "b/cat.jpg",
			PartNumber:           1,
			Offset:               0,
			Size:                 5,
			TelegramType:         "document",
			TelegramFileID:       "file-1",
			TelegramMessageID:    101,
			TelegramFileUniqueID: "u1",
			SHA256:               "c1",
		},
		{
			Bucket:               "photos",
			Key:                  "b/cat.jpg",
			PartNumber:           2,
			Offset:               5,
			Size:                 6,
			TelegramType:         "document",
			TelegramFileID:       "file-2",
			TelegramMessageID:    102,
			TelegramFileUniqueID: "u2",
			SHA256:               "c2",
		},
	}
	if err := store.PutObject(t.Context(), object, chunks); err != nil {
		t.Fatalf("PutObject returned error: %v", err)
	}

	gotObject, gotChunks, err := store.GetObject(t.Context(), "photos", "b/cat.jpg")
	if err != nil {
		t.Fatalf("GetObject returned error: %v", err)
	}
	if !reflect.DeepEqual(gotObject, object) {
		t.Fatalf("object = %+v, want %+v", gotObject, object)
	}
	if !reflect.DeepEqual(gotChunks, chunks) {
		t.Fatalf("chunks = %+v, want %+v", gotChunks, chunks)
	}
}

func TestSQLitePutObjectReplacesChunks(t *testing.T) {
	store := openTestSQLiteStore(t)
	defer store.Close()

	object := Object{
		Bucket:         "photos",
		Key:            "b/cat.jpg",
		Size:           11,
		ContentType:    "image/jpeg",
		ETag:           "etag",
		SHA256:         "sha",
		LastModified:   time.Unix(20, 0),
		ChunkCount:     2,
		TelegramType:   "document",
		UploadStrategy: "chunked_document",
	}
	if err := store.PutObject(t.Context(), object, []Chunk{
		{
			Bucket:               "photos",
			Key:                  "b/cat.jpg",
			PartNumber:           1,
			Offset:               0,
			Size:                 5,
			TelegramType:         "document",
			TelegramFileID:       "file-1",
			TelegramMessageID:    101,
			TelegramFileUniqueID: "u1",
			SHA256:               "c1",
		},
		{
			Bucket:               "photos",
			Key:                  "b/cat.jpg",
			PartNumber:           2,
			Offset:               5,
			Size:                 6,
			TelegramType:         "document",
			TelegramFileID:       "file-2",
			TelegramMessageID:    102,
			TelegramFileUniqueID: "u2",
			SHA256:               "c2",
		},
	}); err != nil {
		t.Fatalf("initial PutObject returned error: %v", err)
	}

	replacement := object
	replacement.ChunkCount = 1
	if err := store.PutObject(t.Context(), replacement, []Chunk{
		{
			Bucket:               "photos",
			Key:                  "b/cat.jpg",
			PartNumber:           1,
			Offset:               0,
			Size:                 11,
			TelegramType:         "document",
			TelegramFileID:       "replacement",
			TelegramMessageID:    201,
			TelegramFileUniqueID: "u3",
			SHA256:               "c3",
		},
	}); err != nil {
		t.Fatalf("replacement PutObject returned error: %v", err)
	}

	gotObject, gotChunks, err := store.GetObject(t.Context(), "photos", "b/cat.jpg")
	if err != nil {
		t.Fatalf("GetObject returned error: %v", err)
	}
	if gotObject.ChunkCount != 1 {
		t.Fatalf("chunk count = %d", gotObject.ChunkCount)
	}
	if len(gotChunks) != 1 {
		t.Fatalf("len(chunks) = %d", len(gotChunks))
	}
	if gotChunks[0].TelegramFileID != "replacement" {
		t.Fatalf("file id = %q", gotChunks[0].TelegramFileID)
	}
}

func TestSQLitePutObjectReplacementRollbackOnChunkInsertFailure(t *testing.T) {
	store := openTestSQLiteStore(t)
	defer store.Close()

	originalObject := Object{
		Bucket:         "photos",
		Key:            "b/cat.jpg",
		Size:           5,
		ContentType:    "image/jpeg",
		ETag:           "original-etag",
		SHA256:         "original-sha",
		LastModified:   time.Unix(21, 0),
		ChunkCount:     1,
		TelegramType:   "document",
		UploadStrategy: "document",
	}
	originalChunks := []Chunk{
		{
			Bucket:               "photos",
			Key:                  "b/cat.jpg",
			PartNumber:           1,
			Offset:               0,
			Size:                 5,
			TelegramType:         "document",
			TelegramFileID:       "original-file",
			TelegramMessageID:    101,
			TelegramFileUniqueID: "original-unique",
			SHA256:               "original-chunk-sha",
		},
	}
	if err := store.PutObject(t.Context(), originalObject, originalChunks); err != nil {
		t.Fatalf("initial PutObject returned error: %v", err)
	}

	replacementObject := Object{
		Bucket:         "photos",
		Key:            "b/cat.jpg",
		Size:           10,
		ContentType:    "image/jpeg",
		ETag:           "replacement-etag",
		SHA256:         "replacement-sha",
		LastModified:   time.Unix(22, 0),
		ChunkCount:     2,
		TelegramType:   "document",
		UploadStrategy: "chunked_document",
	}
	replacementChunks := []Chunk{
		{
			Bucket:               "photos",
			Key:                  "b/cat.jpg",
			PartNumber:           1,
			Offset:               0,
			Size:                 5,
			TelegramType:         "document",
			TelegramFileID:       "replacement-file-1",
			TelegramMessageID:    201,
			TelegramFileUniqueID: "replacement-unique-1",
			SHA256:               "replacement-chunk-sha-1",
		},
		{
			Bucket:               "photos",
			Key:                  "b/cat.jpg",
			PartNumber:           1,
			Offset:               5,
			Size:                 5,
			TelegramType:         "document",
			TelegramFileID:       "replacement-file-2",
			TelegramMessageID:    202,
			TelegramFileUniqueID: "replacement-unique-2",
			SHA256:               "replacement-chunk-sha-2",
		},
	}

	if err := store.PutObject(t.Context(), replacementObject, replacementChunks); err == nil {
		t.Fatal("replacement PutObject returned nil error, want primary key violation")
	}

	gotObject, gotChunks, err := store.GetObject(t.Context(), "photos", "b/cat.jpg")
	if err != nil {
		t.Fatalf("GetObject returned error: %v", err)
	}
	if !reflect.DeepEqual(gotObject, originalObject) {
		t.Fatalf("object = %+v, want %+v", gotObject, originalObject)
	}
	if !reflect.DeepEqual(gotChunks, originalChunks) {
		t.Fatalf("chunks = %+v, want %+v", gotChunks, originalChunks)
	}
}

func TestSQLiteListObjectsOrdersAndPaginates(t *testing.T) {
	store := openTestSQLiteStore(t)
	defer store.Close()

	objects := []Object{
		{
			Bucket:         "photos",
			Key:            "a/one.txt",
			Size:           1,
			ContentType:    "text/plain",
			ETag:           "etag-1",
			SHA256:         "sha-1",
			LastModified:   time.Unix(1, 0),
			ChunkCount:     1,
			TelegramType:   "document",
			UploadStrategy: "document",
		},
		{
			Bucket:         "photos",
			Key:            "a/two.txt",
			Size:           2,
			ContentType:    "text/plain",
			ETag:           "etag-2",
			SHA256:         "sha-2",
			LastModified:   time.Unix(2, 0),
			ChunkCount:     1,
			TelegramType:   "document",
			UploadStrategy: "document",
		},
		{
			Bucket:         "photos",
			Key:            "a/zero.txt",
			Size:           0,
			ContentType:    "text/plain",
			ETag:           "etag-0",
			SHA256:         "sha-0",
			LastModified:   time.Unix(0, 0),
			ChunkCount:     1,
			TelegramType:   "document",
			UploadStrategy: "document",
		},
		{
			Bucket:         "photos",
			Key:            "b/three.txt",
			Size:           3,
			ContentType:    "text/plain",
			ETag:           "etag-3",
			SHA256:         "sha-3",
			LastModified:   time.Unix(3, 0),
			ChunkCount:     1,
			TelegramType:   "document",
			UploadStrategy: "document",
		},
	}
	for _, object := range objects {
		if err := store.PutObject(t.Context(), object, []Chunk{singleChunk(object.Bucket, object.Key)}); err != nil {
			t.Fatalf("PutObject(%q) returned error: %v", object.Key, err)
		}
	}

	ordered, err := store.ListObjects(t.Context(), ListQuery{Bucket: "photos", Prefix: "a/", Limit: 10})
	if err != nil {
		t.Fatalf("ListObjects ordered returned error: %v", err)
	}
	wantOrdered := []string{"a/one.txt", "a/two.txt", "a/zero.txt"}
	var gotOrdered []string
	for _, object := range ordered {
		gotOrdered = append(gotOrdered, object.Key)
	}
	if !reflect.DeepEqual(gotOrdered, wantOrdered) {
		t.Fatalf("ordered keys = %v, want %v", gotOrdered, wantOrdered)
	}

	limited, err := store.ListObjects(t.Context(), ListQuery{Bucket: "photos", Prefix: "a/", Limit: 2})
	if err != nil {
		t.Fatalf("ListObjects limited returned error: %v", err)
	}
	wantLimited := []string{"a/one.txt", "a/two.txt"}
	var gotLimited []string
	for _, object := range limited {
		gotLimited = append(gotLimited, object.Key)
	}
	if !reflect.DeepEqual(gotLimited, wantLimited) {
		t.Fatalf("limited keys = %v, want %v", gotLimited, wantLimited)
	}

	afterKey, err := store.ListObjects(t.Context(), ListQuery{Bucket: "photos", Prefix: "a/", AfterKey: "a/one.txt", Limit: 10})
	if err != nil {
		t.Fatalf("ListObjects after key returned error: %v", err)
	}
	wantAfterKey := []string{"a/two.txt", "a/zero.txt"}
	var gotAfterKey []string
	for _, object := range afterKey {
		gotAfterKey = append(gotAfterKey, object.Key)
	}
	if !reflect.DeepEqual(gotAfterKey, wantAfterKey) {
		t.Fatalf("after key keys = %v, want %v", gotAfterKey, wantAfterKey)
	}
}

func TestSQLiteListObjectsTreatsLikeWildcardsLiterally(t *testing.T) {
	store := openTestSQLiteStore(t)
	defer store.Close()

	objects := []Object{
		newTestObject("photos", "literal%/match.txt", 8),
		newTestObject("photos", "literal_/match.txt", 9),
		newTestObject("photos", "literalx/match.txt", 10),
		newTestObject("photos", "literaly/match.txt", 11),
	}
	for _, object := range objects {
		if err := store.PutObject(t.Context(), object, []Chunk{singleChunk(object.Bucket, object.Key)}); err != nil {
			t.Fatalf("PutObject(%q) returned error: %v", object.Key, err)
		}
	}

	percentObjects, err := store.ListObjects(t.Context(), ListQuery{Bucket: "photos", Prefix: "literal%/", Limit: 10})
	if err != nil {
		t.Fatalf("ListObjects with percent prefix returned error: %v", err)
	}
	assertObjectKeys(t, percentObjects, []string{"literal%/match.txt"})

	underscoreObjects, err := store.ListObjects(t.Context(), ListQuery{Bucket: "photos", Prefix: "literal_/", Limit: 10})
	if err != nil {
		t.Fatalf("ListObjects with underscore prefix returned error: %v", err)
	}
	assertObjectKeys(t, underscoreObjects, []string{"literal_/match.txt"})
}

func TestSQLiteDeleteAndMissingObject(t *testing.T) {
	store := openTestSQLiteStore(t)
	defer store.Close()

	if err := store.DeleteObject(t.Context(), "photos", "missing"); err != nil {
		t.Fatalf("DeleteObject returned error: %v", err)
	}

	_, _, err := store.GetObject(t.Context(), "photos", "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetObject error = %v, want ErrNotFound", err)
	}

	object := Object{
		Bucket:         "photos",
		Key:            "gone.txt",
		Size:           1,
		ContentType:    "text/plain",
		ETag:           "etag",
		SHA256:         "sha",
		LastModified:   time.Unix(4, 0),
		ChunkCount:     1,
		TelegramType:   "document",
		UploadStrategy: "document",
	}
	if err := store.PutObject(t.Context(), object, []Chunk{singleChunk("photos", "gone.txt")}); err != nil {
		t.Fatalf("PutObject returned error: %v", err)
	}
	if err := store.DeleteObject(t.Context(), "photos", "gone.txt"); err != nil {
		t.Fatalf("DeleteObject returned error: %v", err)
	}
	_, _, err = store.GetObject(t.Context(), "photos", "gone.txt")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetObject after delete error = %v, want ErrNotFound", err)
	}

	_, err = store.HeadObject(t.Context(), "photos", "gone.txt")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("HeadObject after delete error = %v, want ErrNotFound", err)
	}
}

func TestSQLiteListBucketsSkipsDisabled(t *testing.T) {
	store := openTestSQLiteStore(t)
	defer store.Close()

	if err := store.UpsertBucket(t.Context(), Bucket{Name: "beta", ChatID: "-2", CreatedAt: time.Unix(2, 0), Enabled: true}); err != nil {
		t.Fatalf("UpsertBucket beta returned error: %v", err)
	}
	if err := store.UpsertBucket(t.Context(), Bucket{Name: "alpha", ChatID: "-1", CreatedAt: time.Unix(1, 0), Enabled: false}); err != nil {
		t.Fatalf("UpsertBucket alpha returned error: %v", err)
	}

	got, err := store.ListBuckets(t.Context())
	if err != nil {
		t.Fatalf("ListBuckets returned error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(buckets) = %d", len(got))
	}
	if got[0].Name != "beta" {
		t.Fatalf("bucket name = %q", got[0].Name)
	}
}

func openTestSQLiteStore(t *testing.T) *SQLiteStore {
	t.Helper()

	path := filepath.Join(t.TempDir(), "metadata.sqlite")
	store, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	return store
}

func newTestObject(bucket, key string, unixTime int64) Object {
	return Object{
		Bucket:         bucket,
		Key:            key,
		Size:           1,
		ContentType:    "text/plain",
		ETag:           key + "-etag",
		SHA256:         key + "-sha",
		LastModified:   time.Unix(unixTime, 0),
		ChunkCount:     1,
		TelegramType:   "document",
		UploadStrategy: "document",
	}
}

func assertObjectKeys(t *testing.T, objects []Object, want []string) {
	t.Helper()

	got := make([]string, 0, len(objects))
	for _, object := range objects {
		got = append(got, object.Key)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
}

func singleChunk(bucket, key string) Chunk {
	return Chunk{
		Bucket:               bucket,
		Key:                  key,
		PartNumber:           1,
		Offset:               0,
		Size:                 1,
		TelegramType:         "document",
		TelegramFileID:       key + "-file",
		TelegramMessageID:    1,
		TelegramFileUniqueID: key + "-unique",
		SHA256:               key + "-sha",
	}
}
