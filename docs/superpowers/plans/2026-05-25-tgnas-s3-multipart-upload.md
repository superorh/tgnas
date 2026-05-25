# TgNAS S3 Multipart Upload Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the minimal S3 multipart upload lifecycle: CreateMultipartUpload, UploadPart, CompleteMultipartUpload, and AbortMultipartUpload.

**Architecture:** Multipart upload state lives in `store.ObjectStore` memory until completion. `UploadPart` uploads request bytes directly to Telegram in internal chunks and records metadata in the upload session. `CompleteMultipartUpload` validates the client part list, expands staged Telegram chunk metadata into final `object_chunks`, and commits the final `objects` row without re-uploading object data.

**Tech Stack:** Go, `net/http`, XML encoding/decoding, SQLite metadata store, Telegram upload client, AWS SigV4 tests.

---

## File Structure

- Modify `store/types.go`
  - Add multipart input/result types and store-level multipart errors.
- Modify `store/store.go`
  - Add in-memory multipart session state to `ObjectStore`.
  - Initialize multipart state in `NewObjectStore`.
  - Implement create/upload/complete/abort methods.
  - Add helper methods for internal Telegram chunk upload, final metadata commit, multipart ETag, and oldest-session eviction.
- Modify `internal/s3api/errors.go`
  - Map store multipart errors to S3 XML errors.
- Modify `internal/s3api/xml.go`
  - Add XML request/response structs for multipart create and complete.
- Modify `internal/s3api/server.go`
  - Extend the S3 `ObjectStore` interface with multipart methods.
  - Route object-level multipart requests.
  - Add handler methods for create/upload/complete/abort.
- Modify `internal/s3api/server_test.go`
  - Add end-to-end signed S3 API tests for the multipart lifecycle and routing behavior.
  - Update any fake `ObjectStore` test implementation to satisfy the new interface.
- Modify `store/store_test.go`
  - Add focused store tests for chunk splitting, metadata commit, multipart ETag, abort, eviction, and ordinary PutObject regression.

## Implementation Notes

Use TDD. For each task, write the test first, run it and confirm the expected failure, then implement only the code needed to pass that test.

Do not add durable multipart tables. Do not add `ListParts`, `ListMultipartUploads`, multipart copy, presigned multipart writes, or Telegram remote deletion.

Use these method/type names consistently:

```go
const maxMultipartUploads = 1000

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
```

Use these errors in `store/types.go`:

```go
ErrNoSuchUpload      = errors.New("no such upload")
ErrInvalidPart       = errors.New("invalid part")
ErrInvalidPartOrder  = errors.New("invalid part order")
ErrInvalidArgument   = errors.New("invalid argument")
```

Internal multipart structs can live in `store/store.go` near `uploadRecord`:

```go
type multipartUpload struct {
	bucket      string
	key         string
	contentType string
	createdAt   time.Time
	parts       map[int]multipartPart
}

type multipartPart struct {
	number   int
	etag     string
	md5Bytes []byte
	size     int64
	chunks   []multipartChunk
}

type multipartChunk struct {
	size                 int64
	telegramType         string
	telegramFileID       string
	telegramMessageID    int64
	telegramFileUniqueID string
	sha256               string
}
```

Add these fields to `store.ObjectStore`:

```go
multipartMu      sync.Mutex
multipartUploads map[string]*multipartUpload
```

When `CompleteMultipartUpload` commits metadata, set `metadata.Object.SHA256` to an empty string for multipart objects.

## Task 1: Add store multipart types and error mapping

**Files:**
- Modify: `store/types.go`
- Modify: `internal/s3api/errors.go`

- [ ] **Step 1: Add failing store compile test for new multipart API types**

Append this test to `store/store_test.go`:

```go
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
```

Ensure `store/store_test.go` imports already include `errors` and `strings`; both are already used in the file.

- [ ] **Step 2: Run the focused test and verify it fails**

Run:

```bash
go test ./store -run TestMultipartAPITypesCompile
```

Expected: compile failure for undefined multipart types and errors.

- [ ] **Step 3: Add types and errors to `store/types.go`**

Edit the error var block to include:

```go
	ErrNoSuchUpload      = errors.New("no such upload")
	ErrInvalidPart       = errors.New("invalid part")
	ErrInvalidPartOrder  = errors.New("invalid part order")
	ErrInvalidArgument   = errors.New("invalid argument")
```

Add the multipart types after `PutObjectResult`:

```go
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
```

- [ ] **Step 4: Run the focused store test and verify it passes**

Run:

```bash
go test ./store -run TestMultipartAPITypesCompile
```

Expected: PASS.

- [ ] **Step 5: Add failing S3 error mapping test**

Append this test to `internal/s3api/errors_test.go`, creating the file if it does not exist:

```go
package s3api

import (
	"testing"

	"github.com/aahl/tgnas/store"
)

func TestMapMultipartErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		code   string
		status int
	}{
		{name: "no such upload", err: store.ErrNoSuchUpload, code: "NoSuchUpload", status: 404},
		{name: "invalid part", err: store.ErrInvalidPart, code: "InvalidPart", status: 400},
		{name: "invalid part order", err: store.ErrInvalidPartOrder, code: "InvalidPartOrder", status: 400},
		{name: "invalid argument", err: store.ErrInvalidArgument, code: "InvalidArgument", status: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := MapError(tc.err)
			if got.Code != tc.code || got.Status != tc.status {
				t.Fatalf("MapError(%v) = %+v", tc.err, got)
			}
		})
	}
}
```

If `internal/s3api/errors_test.go` already exists after checkout, append the test without duplicating the package/import block.

- [ ] **Step 6: Run the S3 error test and verify it fails**

Run:

```bash
go test ./internal/s3api -run TestMapMultipartErrors
```

Expected: failure because multipart errors currently map to `InternalError`.

- [ ] **Step 7: Implement multipart error mapping**

Add S3 error values to `internal/s3api/errors.go`:

```go
	ErrNoSuchUpload      = S3Error{Code: "NoSuchUpload", Message: "The specified multipart upload does not exist.", Status: http.StatusNotFound}
	ErrInvalidPart       = S3Error{Code: "InvalidPart", Message: "One or more of the specified parts could not be found or did not match the supplied ETag.", Status: http.StatusBadRequest}
	ErrInvalidPartOrder  = S3Error{Code: "InvalidPartOrder", Message: "The list of parts was not in ascending order.", Status: http.StatusBadRequest}
```

Update `MapError` before the generic invalid argument string checks:

```go
	case errors.Is(err, store.ErrNoSuchUpload):
		return ErrNoSuchUpload
	case errors.Is(err, store.ErrInvalidPart):
		return ErrInvalidPart
	case errors.Is(err, store.ErrInvalidPartOrder):
		return ErrInvalidPartOrder
	case errors.Is(err, store.ErrInvalidArgument):
		return ErrInvalidArgument
```

- [ ] **Step 8: Run focused tests**

Run:

```bash
go test ./store -run TestMultipartAPITypesCompile
go test ./internal/s3api -run TestMapMultipartErrors
```

Expected: both PASS.

- [ ] **Step 9: Commit Task 1**

```bash
git add store/types.go store/store_test.go internal/s3api/errors.go internal/s3api/errors_test.go
git commit -m "feat: add multipart upload API types"
```

## Task 2: Add XML structs and S3 handler routing tests

**Files:**
- Modify: `internal/s3api/xml.go`
- Modify: `internal/s3api/server.go`
- Modify: `internal/s3api/server_test.go`

- [ ] **Step 1: Add failing XML compile test**

Append to `internal/s3api/server_test.go`:

```go
func TestMultipartXMLTypesCompile(t *testing.T) {
	created := InitiateMultipartUploadResult{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/", Bucket: "photos", Key: "big.bin", UploadID: "upload-1"}
	data, err := xml.Marshal(created)
	if err != nil {
		t.Fatalf("Marshal create result returned error: %v", err)
	}
	if !strings.Contains(string(data), "InitiateMultipartUploadResult") || !strings.Contains(string(data), "<UploadId>upload-1</UploadId>") {
		t.Fatalf("create xml = %s", data)
	}

	var complete CompleteMultipartUploadRequest
	if err := xml.Unmarshal([]byte(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"abc"</ETag></Part></CompleteMultipartUpload>`), &complete); err != nil {
		t.Fatalf("Unmarshal complete returned error: %v", err)
	}
	if len(complete.Parts) != 1 || complete.Parts[0].PartNumber != 1 || complete.Parts[0].ETag != "\"abc\"" {
		t.Fatalf("complete request = %+v", complete)
	}

	completed := CompleteMultipartUploadResult{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/", Bucket: "photos", Key: "big.bin", ETag: "\"etag-2\""}
	data, err = xml.Marshal(completed)
	if err != nil {
		t.Fatalf("Marshal complete result returned error: %v", err)
	}
	if !strings.Contains(string(data), "CompleteMultipartUploadResult") || !strings.Contains(string(data), "<ETag>&#34;etag-2&#34;</ETag>") {
		t.Fatalf("complete xml = %s", data)
	}
}
```

`server_test.go` already imports `encoding/xml` and `strings`.

- [ ] **Step 2: Run XML test and verify it fails**

Run:

```bash
go test ./internal/s3api -run TestMultipartXMLTypesCompile
```

Expected: compile failure for undefined XML types.

- [ ] **Step 3: Add XML structs to `internal/s3api/xml.go`**

Append:

```go
type InitiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr,omitempty"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

type CompleteMultipartUploadRequest struct {
	XMLName xml.Name                `xml:"CompleteMultipartUpload"`
	Parts   []CompletePartXML       `xml:"Part"`
}

type CompletePartXML struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

type CompleteMultipartUploadResult struct {
	XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
	Xmlns   string   `xml:"xmlns,attr,omitempty"`
	Bucket  string   `xml:"Bucket"`
	Key     string   `xml:"Key"`
	ETag    string   `xml:"ETag"`
}
```

- [ ] **Step 4: Run XML test and verify it passes**

Run:

```bash
go test ./internal/s3api -run TestMultipartXMLTypesCompile
```

Expected: PASS.

- [ ] **Step 5: Add failing create multipart handler test**

Append to `internal/s3api/server_test.go`:

```go
func TestCreateMultipartUploadReturnsUploadID(t *testing.T) {
	server := newSignedTestServer(t)

	create := signedRecorderRequest(t, http.MethodPost, "/photos/big.bin?uploads", "", map[string]string{"Content-Type": "application/octet-stream"})
	server.ServeHTTP(create.recorder, create.request)

	if create.recorder.Code != http.StatusOK {
		t.Fatalf("create status = %d body = %s", create.recorder.Code, create.recorder.Body.String())
	}
	if !strings.Contains(create.recorder.Body.String(), "<InitiateMultipartUploadResult") || !strings.Contains(create.recorder.Body.String(), "<Bucket>photos</Bucket>") || !strings.Contains(create.recorder.Body.String(), "<Key>big.bin</Key>") || !strings.Contains(create.recorder.Body.String(), "<UploadId>") {
		t.Fatalf("create body = %s", create.recorder.Body.String())
	}
}
```

- [ ] **Step 6: Run create handler test and verify it fails**

Run:

```bash
go test ./internal/s3api -run TestCreateMultipartUploadReturnsUploadID
```

Expected: failure with HTTP 404 or compile failure for missing multipart methods.

- [ ] **Step 7: Extend S3 ObjectStore interface and route multipart requests**

In `internal/s3api/server.go`, add to `ObjectStore`:

```go
	CreateMultipartUpload(ctx context.Context, input store.CreateMultipartUploadInput) (store.CreateMultipartUploadResult, error)
	UploadPart(ctx context.Context, input store.UploadPartInput) (store.UploadPartResult, error)
	CompleteMultipartUpload(ctx context.Context, input store.CompleteMultipartUploadInput) (store.CompleteMultipartUploadResult, error)
	AbortMultipartUpload(ctx context.Context, input store.AbortMultipartUploadInput) error
```

Update `handleObject` to dispatch multipart routes before ordinary object routes:

```go
func (s *Server) handleObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	query := r.URL.Query()
	switch r.Method {
	case http.MethodPost:
		if _, ok := query["uploads"]; ok {
			s.createMultipartUpload(w, r, bucket, key)
			return
		}
		if query.Get("uploadId") != "" {
			s.completeMultipartUpload(w, r, bucket, key)
			return
		}
		http.NotFound(w, r)
	case http.MethodPut:
		if query.Get("uploadId") != "" || query.Get("partNumber") != "" {
			s.uploadPart(w, r, bucket, key)
			return
		}
		s.putObject(w, r, bucket, key)
	case http.MethodGet:
		s.getObject(w, r, bucket, key)
	case http.MethodHead:
		s.headObject(w, r, bucket, key)
	case http.MethodDelete:
		if query.Get("uploadId") != "" {
			s.abortMultipartUpload(w, r, bucket, key)
			return
		}
		if err := s.store.DeleteObject(r.Context(), bucket, key); err != nil {
			WriteErrorResponse(w, r, MapError(err), r.URL.Path, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}
```

Add minimal handler methods to `internal/s3api/server.go`:

```go
func (s *Server) createMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) {
	result, err := s.store.CreateMultipartUpload(r.Context(), store.CreateMultipartUploadInput{Bucket: bucket, Key: key, ContentType: r.Header.Get("Content-Type")})
	if err != nil {
		WriteErrorResponse(w, r, MapError(err), r.URL.Path, "")
		return
	}
	writeXML(w, http.StatusOK, InitiateMultipartUploadResult{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/", Bucket: bucket, Key: key, UploadID: result.UploadID})
}

func (s *Server) uploadPart(w http.ResponseWriter, r *http.Request, bucket, key string) {
	WriteErrorResponse(w, r, ErrNotImplemented, r.URL.Path, "")
}

func (s *Server) completeMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) {
	WriteErrorResponse(w, r, ErrNotImplemented, r.URL.Path, "")
}

func (s *Server) abortMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) {
	WriteErrorResponse(w, r, ErrNotImplemented, r.URL.Path, "")
}
```

Update `errorPutObjectStore` in `internal/s3api/server_test.go` to implement the four new methods, returning `store.ErrNotImplemented` or nil for abort:

```go
func (s errorPutObjectStore) CreateMultipartUpload(context.Context, store.CreateMultipartUploadInput) (store.CreateMultipartUploadResult, error) {
	return store.CreateMultipartUploadResult{}, store.ErrNotImplemented
}

func (s errorPutObjectStore) UploadPart(context.Context, store.UploadPartInput) (store.UploadPartResult, error) {
	return store.UploadPartResult{}, store.ErrNotImplemented
}

func (s errorPutObjectStore) CompleteMultipartUpload(context.Context, store.CompleteMultipartUploadInput) (store.CompleteMultipartUploadResult, error) {
	return store.CompleteMultipartUploadResult{}, store.ErrNotImplemented
}

func (s errorPutObjectStore) AbortMultipartUpload(context.Context, store.AbortMultipartUploadInput) error {
	return store.ErrNotImplemented
}
```

- [ ] **Step 8: Add temporary store stub methods so create handler can compile**

Add temporary methods to `store/store.go`; later tasks will replace them with real behavior:

```go
func (s *ObjectStore) CreateMultipartUpload(ctx context.Context, input CreateMultipartUploadInput) (CreateMultipartUploadResult, error) {
	if err := s.HeadBucket(ctx, input.Bucket); err != nil {
		return CreateMultipartUploadResult{}, err
	}
	return CreateMultipartUploadResult{}, ErrNotImplemented
}

func (s *ObjectStore) UploadPart(ctx context.Context, input UploadPartInput) (UploadPartResult, error) {
	return UploadPartResult{}, ErrNotImplemented
}

func (s *ObjectStore) CompleteMultipartUpload(ctx context.Context, input CompleteMultipartUploadInput) (CompleteMultipartUploadResult, error) {
	return CompleteMultipartUploadResult{}, ErrNotImplemented
}

func (s *ObjectStore) AbortMultipartUpload(ctx context.Context, input AbortMultipartUploadInput) error {
	return ErrNotImplemented
}
```

- [ ] **Step 9: Run create handler test and verify it still fails for NotImplemented**

Run:

```bash
go test ./internal/s3api -run TestCreateMultipartUploadReturnsUploadID
```

Expected: failure with HTTP 501 because store create still returns `ErrNotImplemented`.

- [ ] **Step 10: Do not commit Task 2 yet**

Do not create a commit at this point. Task 2 intentionally leaves CreateMultipartUpload returning `NotImplemented`; commit the XML/routing work together with the passing CreateMultipartUpload store behavior in Task 3.

## Task 3: Implement CreateMultipartUpload store behavior

**Files:**
- Modify: `store/store.go`
- Modify: `store/store_test.go`
- Modify: `internal/s3api/server_test.go`

- [ ] **Step 1: Add failing store create test**

Append to `store/store_test.go`:

```go
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
```

- [ ] **Step 2: Run store create test and verify it fails**

Run:

```bash
go test ./store -run TestStoreCreateMultipartUploadReturnsDistinctUploadIDs
```

Expected: failure because create returns `ErrNotImplemented`.

- [ ] **Step 3: Add multipart state and ID generation**

In `store/store.go`, add imports:

```go
	"crypto/rand"
	"sync"
```

Add fields to `ObjectStore`:

```go
	multipartMu      sync.Mutex
	multipartUploads map[string]*multipartUpload
```

Initialize in `NewObjectStore`:

```go
		multipartUploads: map[string]*multipartUpload{},
```

Add internal structs near `uploadRecord`:

```go
const maxMultipartUploads = 1000

type multipartUpload struct {
	bucket      string
	key         string
	contentType string
	createdAt   time.Time
	parts       map[int]multipartPart
}

type multipartPart struct {
	number   int
	etag     string
	md5Bytes []byte
	size     int64
	chunks   []multipartChunk
}

type multipartChunk struct {
	size                 int64
	telegramType         string
	telegramFileID       string
	telegramMessageID    int64
	telegramFileUniqueID string
	sha256               string
}
```

Add helper:

```go
func newUploadID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
```

Replace `CreateMultipartUpload` stub:

```go
func (s *ObjectStore) CreateMultipartUpload(ctx context.Context, input CreateMultipartUploadInput) (CreateMultipartUploadResult, error) {
	if err := s.HeadBucket(ctx, input.Bucket); err != nil {
		return CreateMultipartUploadResult{}, err
	}
	uploadID, err := newUploadID()
	if err != nil {
		return CreateMultipartUploadResult{}, err
	}

	s.multipartMu.Lock()
	defer s.multipartMu.Unlock()
	s.multipartUploads[uploadID] = &multipartUpload{bucket: input.Bucket, key: input.Key, contentType: input.ContentType, createdAt: time.Now().UTC(), parts: map[int]multipartPart{}}
	s.evictOldestMultipartUploadLocked()
	return CreateMultipartUploadResult{UploadID: uploadID}, nil
}

func (s *ObjectStore) evictOldestMultipartUploadLocked() {
	if len(s.multipartUploads) <= maxMultipartUploads {
		return
	}
	oldestID := ""
	var oldest time.Time
	for uploadID, upload := range s.multipartUploads {
		if oldestID == "" || upload.createdAt.Before(oldest) {
			oldestID = uploadID
			oldest = upload.createdAt
		}
	}
	if oldestID != "" {
		s.logMultipartOrphansLocked(oldestID, "evict")
		delete(s.multipartUploads, oldestID)
	}
}

func (s *ObjectStore) logMultipartOrphansLocked(uploadID, reason string) {
	if s.logger == nil {
		return
	}
	upload := s.multipartUploads[uploadID]
	if upload == nil {
		return
	}
	records := make([]uploadRecord, 0)
	for _, part := range upload.parts {
		for _, chunk := range part.chunks {
			records = append(records, uploadRecord{FileID: chunk.telegramFileID, MessageID: chunk.telegramMessageID})
		}
	}
	if len(records) > 0 {
		s.logOrphanUpload(upload.bucket, upload.key, records, fmt.Errorf("multipart upload %s: %s", uploadID, reason))
	}
}
```

- [ ] **Step 4: Run store create and S3 create tests**

Run:

```bash
go test ./store -run TestStoreCreateMultipartUploadReturnsDistinctUploadIDs
go test ./internal/s3api -run TestCreateMultipartUploadReturnsUploadID
```

Expected: both PASS.

- [ ] **Step 5: Add and pass missing bucket create test**

Add to `store/store_test.go`:

```go
func TestStoreCreateMultipartUploadMissingBucket(t *testing.T) {
	ctx := context.Background()
	objectStore, _ := newReadyTestObjectStore(t, map[string]string{"photos": "-100"})

	_, err := objectStore.CreateMultipartUpload(ctx, CreateMultipartUploadInput{Bucket: "missing", Key: "big.bin", ContentType: "application/octet-stream"})
	if err != ErrNoSuchBucket {
		t.Fatalf("err = %v, want ErrNoSuchBucket", err)
	}
}
```

Run:

```bash
go test ./store -run 'TestStoreCreateMultipartUpload(ReturnsDistinctUploadIDs|MissingBucket)'
```

Expected: PASS.

- [ ] **Step 6: Commit Task 3**

```bash
git add store/store.go store/store_test.go internal/s3api/server.go internal/s3api/server_test.go internal/s3api/xml.go internal/s3api/errors.go internal/s3api/errors_test.go store/types.go
git commit -m "feat: initiate multipart uploads"
```

## Task 4: Implement UploadPart with direct Telegram chunk uploads

**Files:**
- Modify: `store/store.go`
- Modify: `store/store_test.go`
- Modify: `internal/s3api/server.go`
- Modify: `internal/s3api/server_test.go`

- [ ] **Step 1: Add failing store upload-part test for multiple internal chunks**

Append to `store/store_test.go`:

```go
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
```

- [ ] **Step 2: Run store upload-part test and verify it fails**

Run:

```bash
go test ./store -run TestStoreUploadPartSplitsIntoTelegramChunks
```

Expected: failure because UploadPart returns `ErrNotImplemented`.

- [ ] **Step 3: Implement UploadPart validation and chunk upload**

Replace `UploadPart` stub in `store/store.go` with:

```go
func (s *ObjectStore) UploadPart(ctx context.Context, input UploadPartInput) (UploadPartResult, error) {
	if input.PartNumber < 1 || input.UploadID == "" || input.Size < 0 {
		return UploadPartResult{}, ErrInvalidArgument
	}
	if input.Body == nil {
		input.Body = strings.NewReader("")
	}

	s.multipartMu.Lock()
	upload := s.multipartUploads[input.UploadID]
	if upload == nil || upload.bucket != input.Bucket || upload.key != input.Key {
		s.multipartMu.Unlock()
		return UploadPartResult{}, ErrNoSuchUpload
	}
	contentType := upload.contentType
	s.multipartMu.Unlock()

	releaseUpload := s.acquire(ctx, s.uploads)
	if releaseUpload == nil {
		return UploadPartResult{}, ctx.Err()
	}
	defer releaseUpload()

	chunks, etag, md5Bytes, size, err := s.uploadMultipartPartChunks(ctx, input, contentType)
	if err != nil {
		return UploadPartResult{}, err
	}

	s.multipartMu.Lock()
	defer s.multipartMu.Unlock()
	upload = s.multipartUploads[input.UploadID]
	if upload == nil || upload.bucket != input.Bucket || upload.key != input.Key {
		return UploadPartResult{}, ErrNoSuchUpload
	}
	if old, ok := upload.parts[input.PartNumber]; ok && len(old.chunks) > 0 {
		s.logMultipartPartOrphans(input.Bucket, input.Key, old, fmt.Errorf("multipart part %d replaced", input.PartNumber))
	}
	upload.parts[input.PartNumber] = multipartPart{number: input.PartNumber, etag: etag, md5Bytes: md5Bytes, size: size, chunks: chunks}
	return UploadPartResult{ETag: etag}, nil
}
```

Add helpers in `store/store.go`:

```go
func (s *ObjectStore) uploadMultipartPartChunks(ctx context.Context, input UploadPartInput, contentType string) ([]multipartChunk, string, []byte, int64, error) {
	md5Hash := md5.New()
	chunkSize := s.options.Upload.ChunkSize
	if chunkSize <= 0 {
		chunkSize = DefaultUploadConfig().ChunkSize
	}
	limit := s.options.Upload.TypeLimits[telegram.TypeDocument]
	if limit > 0 && chunkSize > limit {
		chunkSize = limit
	}
	if chunkSize <= 0 {
		return nil, "", nil, 0, ErrEntityTooLarge
	}

	chunks := []multipartChunk{}
	uploads := []uploadRecord{}
	remaining := input.Size
	partIndex := 1
	for remaining > 0 {
		thisChunkSize := chunkSize
		if remaining < thisChunkSize {
			thisChunkSize = remaining
		}
		data := make([]byte, thisChunkSize)
		read, err := io.ReadFull(input.Body, data)
		if err != nil {
			if len(uploads) > 0 {
				s.logOrphanUpload(input.Bucket, input.Key, uploads, err)
			}
			return nil, "", nil, 0, err
		}
		if int64(read) != thisChunkSize {
			return nil, "", nil, 0, fmt.Errorf("copied %d bytes, want %d", read, thisChunkSize)
		}
		partData := data[:read]
		_, _ = md5Hash.Write(partData)
		chunkSHA := sha256.Sum256(partData)
		uploaded, err := s.uploadTelegram(ctx, telegram.UploadRequest{Type: telegram.TypeDocument, ChatID: s.bucketChatID(input.Bucket), Reader: strings.NewReader(string(partData)), Filename: path.Base(input.Key), MIMEType: contentType, Caption: s.renderCaption(PutObjectInput{Bucket: input.Bucket, Key: input.Key, ContentType: contentType, Size: input.Size}, partIndex, int((input.Size+chunkSize-1)/chunkSize))})
		if err != nil {
			if len(uploads) > 0 {
				s.logOrphanUpload(input.Bucket, input.Key, uploads, err)
			}
			return nil, "", nil, 0, err
		}
		uploads = append(uploads, uploadRecord{FileID: uploaded.FileID, MessageID: uploaded.MessageID})
		chunks = append(chunks, multipartChunk{size: int64(len(partData)), telegramType: uploaded.Type, telegramFileID: uploaded.FileID, telegramMessageID: uploaded.MessageID, telegramFileUniqueID: uploaded.FileUniqueID, sha256: hex.EncodeToString(chunkSHA[:])})
		remaining -= int64(len(partData))
		partIndex++
	}
	if extra, err := io.ReadAll(input.Body); err != nil {
		return nil, "", nil, 0, err
	} else if len(extra) > 0 {
		return nil, "", nil, 0, fmt.Errorf("copied %d bytes, want %d", input.Size+int64(len(extra)), input.Size)
	}
	md5Bytes := md5Hash.Sum(nil)
	return chunks, hex.EncodeToString(md5Bytes), md5Bytes, input.Size, nil
}

func (s *ObjectStore) logMultipartPartOrphans(bucket, key string, part multipartPart, err error) {
	records := make([]uploadRecord, 0, len(part.chunks))
	for _, chunk := range part.chunks {
		records = append(records, uploadRecord{FileID: chunk.telegramFileID, MessageID: chunk.telegramMessageID})
	}
	if len(records) > 0 {
		s.logOrphanUpload(bucket, key, records, err)
	}
}
```

- [ ] **Step 4: Run store upload-part test and verify it passes**

Run:

```bash
go test ./store -run TestStoreUploadPartSplitsIntoTelegramChunks
```

Expected: PASS.

- [ ] **Step 5: Add failing S3 upload-part handler test**

Append to `internal/s3api/server_test.go`:

```go
func TestMultipartUploadPartReturnsETag(t *testing.T) {
	server := newSignedTestServer(t)

	create := signedRecorderRequest(t, http.MethodPost, "/photos/big.bin?uploads", "", map[string]string{"Content-Type": "application/octet-stream"})
	server.ServeHTTP(create.recorder, create.request)
	if create.recorder.Code != http.StatusOK {
		t.Fatalf("create status = %d body = %s", create.recorder.Code, create.recorder.Body.String())
	}
	uploadID := extractBetween(create.recorder.Body.String(), "<UploadId>", "</UploadId>")
	if uploadID == "" {
		t.Fatalf("create body = %s", create.recorder.Body.String())
	}

	part := signedRecorderRequest(t, http.MethodPut, "/photos/big.bin?partNumber=1&uploadId="+url.QueryEscape(uploadID), "abc", nil)
	server.ServeHTTP(part.recorder, part.request)
	if part.recorder.Code != http.StatusOK {
		t.Fatalf("part status = %d body = %s", part.recorder.Code, part.recorder.Body.String())
	}
	if part.recorder.Header().Get("ETag") != "\"900150983cd24fb0d6963f7d28e17f72\"" {
		t.Fatalf("part headers = %v", part.recorder.Header())
	}
}
```

Ensure `server_test.go` imports include `net/url`; add it if missing.

- [ ] **Step 6: Run S3 upload-part handler test and verify it fails**

Run:

```bash
go test ./internal/s3api -run TestMultipartUploadPartReturnsETag
```

Expected: failure because handler still returns NotImplemented.

- [ ] **Step 7: Implement `uploadPart` handler**

Replace `uploadPart` in `internal/s3api/server.go`:

```go
func (s *Server) uploadPart(w http.ResponseWriter, r *http.Request, bucket, key string) {
	query := r.URL.Query()
	uploadID := query.Get("uploadId")
	partNumber, err := strconv.Atoi(query.Get("partNumber"))
	if uploadID == "" || err != nil || partNumber < 1 {
		WriteErrorResponse(w, r, ErrInvalidArgument, r.URL.Path, "")
		return
	}
	if r.ContentLength < 0 {
		WriteErrorResponse(w, r, ErrMissingContentLength, r.URL.Path, "")
		return
	}
	result, err := s.store.UploadPart(r.Context(), store.UploadPartInput{Bucket: bucket, Key: key, UploadID: uploadID, PartNumber: partNumber, Size: r.ContentLength, Body: r.Body})
	if err != nil {
		WriteErrorResponse(w, r, MapError(err), r.URL.Path, "")
		return
	}
	w.Header().Set("ETag", fmt.Sprintf("\"%s\"", result.ETag))
	w.WriteHeader(http.StatusOK)
}
```

- [ ] **Step 8: Run focused upload-part tests**

Run:

```bash
go test ./store -run TestStoreUploadPartSplitsIntoTelegramChunks
go test ./internal/s3api -run TestMultipartUploadPartReturnsETag
```

Expected: both PASS.

- [ ] **Step 9: Commit Task 4**

```bash
git add store/store.go store/store_test.go internal/s3api/server.go internal/s3api/server_test.go
git commit -m "feat: upload multipart parts to telegram chunks"
```

## Task 5: Implement CompleteMultipartUpload metadata commit and multipart ETag

**Files:**
- Modify: `store/store.go`
- Modify: `store/store_test.go`
- Modify: `internal/s3api/server.go`
- Modify: `internal/s3api/server_test.go`

- [ ] **Step 1: Add failing store complete test**

Append to `store/store_test.go`:

```go
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
	if completed.ETag != "8e18a6d3619b553c27c7028ea9067e05-2" {
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
```

- [ ] **Step 2: Run store complete test and verify it fails**

Run:

```bash
go test ./store -run TestStoreCompleteMultipartUploadCommitsChunksAndMultipartETag
```

Expected: failure because complete returns `ErrNotImplemented`.

- [ ] **Step 3: Implement CompleteMultipartUpload**

Replace complete stub in `store/store.go`:

```go
func (s *ObjectStore) CompleteMultipartUpload(ctx context.Context, input CompleteMultipartUploadInput) (CompleteMultipartUploadResult, error) {
	if input.UploadID == "" || len(input.Parts) == 0 {
		return CompleteMultipartUploadResult{}, ErrInvalidArgument
	}

	releaseLock := s.locker.Lock(input.Bucket, input.Key)
	defer releaseLock()

	s.multipartMu.Lock()
	upload := s.multipartUploads[input.UploadID]
	if upload == nil || upload.bucket != input.Bucket || upload.key != input.Key {
		s.multipartMu.Unlock()
		return CompleteMultipartUploadResult{}, ErrNoSuchUpload
	}
	parts, err := upload.completeParts(input.Parts)
	if err != nil {
		s.multipartMu.Unlock()
		return CompleteMultipartUploadResult{}, err
	}
	contentType := upload.contentType
	s.multipartMu.Unlock()

	object, chunks, etag := buildMultipartObject(input.Bucket, input.Key, contentType, parts)
	if err := s.meta.PutObject(ctx, object, chunks); err != nil {
		s.logMetadataPutObject(input.Bucket, input.Key, len(chunks), etag, err)
		return CompleteMultipartUploadResult{}, err
	}
	s.logMetadataPutObject(input.Bucket, input.Key, len(chunks), etag, nil)

	s.multipartMu.Lock()
	delete(s.multipartUploads, input.UploadID)
	s.multipartMu.Unlock()
	return CompleteMultipartUploadResult{ETag: etag}, nil
}
```

Add helpers:

```go
func (u *multipartUpload) completeParts(requested []CompletedPart) ([]multipartPart, error) {
	parts := make([]multipartPart, 0, len(requested))
	lastPart := 0
	for _, requestedPart := range requested {
		if requestedPart.PartNumber <= lastPart {
			return nil, ErrInvalidPartOrder
		}
		lastPart = requestedPart.PartNumber
		part, ok := u.parts[requestedPart.PartNumber]
		if !ok || !etagMatches(part.etag, requestedPart.ETag) {
			return nil, ErrInvalidPart
		}
		parts = append(parts, part)
	}
	return parts, nil
}

func etagMatches(stored, submitted string) bool {
	return strings.Trim(submitted, "\"") == strings.Trim(stored, "\"")
}

func buildMultipartObject(bucket, key, contentType string, parts []multipartPart) (metadata.Object, []metadata.Chunk, string) {
	size := int64(0)
	chunkCount := 0
	for _, part := range parts {
		size += part.size
		chunkCount += len(part.chunks)
	}
	etag := multipartETag(parts)
	chunks := make([]metadata.Chunk, 0, chunkCount)
	offset := int64(0)
	partNumber := 1
	for _, part := range parts {
		for _, chunk := range part.chunks {
			chunks = append(chunks, metadata.Chunk{Bucket: bucket, Key: key, PartNumber: partNumber, Offset: offset, Size: chunk.size, TelegramType: chunk.telegramType, TelegramFileID: chunk.telegramFileID, TelegramMessageID: chunk.telegramMessageID, TelegramFileUniqueID: chunk.telegramFileUniqueID, SHA256: chunk.sha256})
			offset += chunk.size
			partNumber++
		}
	}
	object := metadata.Object{Bucket: bucket, Key: key, Size: size, ContentType: contentType, ETag: etag, SHA256: "", LastModified: time.Now().UTC(), ChunkCount: len(chunks), TelegramType: telegram.TypeDocument, UploadStrategy: "multipart"}
	return object, chunks, etag
}

func multipartETag(parts []multipartPart) string {
	whole := md5.New()
	for _, part := range parts {
		_, _ = whole.Write(part.md5Bytes)
	}
	return fmt.Sprintf("%s-%d", hex.EncodeToString(whole.Sum(nil)), len(parts))
}
```

- [ ] **Step 4: Run store complete test and verify it passes**

Run:

```bash
go test ./store -run TestStoreCompleteMultipartUploadCommitsChunksAndMultipartETag
```

Expected: PASS.

- [ ] **Step 5: Add failing S3 complete lifecycle test**

Append to `internal/s3api/server_test.go`:

```go
func TestMultipartCompleteMakesObjectReadable(t *testing.T) {
	server := newSignedTestServer(t)

	create := signedRecorderRequest(t, http.MethodPost, "/photos/big.bin?uploads", "", map[string]string{"Content-Type": "application/octet-stream"})
	server.ServeHTTP(create.recorder, create.request)
	if create.recorder.Code != http.StatusOK {
		t.Fatalf("create status = %d body = %s", create.recorder.Code, create.recorder.Body.String())
	}
	uploadID := extractBetween(create.recorder.Body.String(), "<UploadId>", "</UploadId>")

	part1 := signedRecorderRequest(t, http.MethodPut, "/photos/big.bin?partNumber=1&uploadId="+url.QueryEscape(uploadID), "abcde", nil)
	server.ServeHTTP(part1.recorder, part1.request)
	part2 := signedRecorderRequest(t, http.MethodPut, "/photos/big.bin?partNumber=2&uploadId="+url.QueryEscape(uploadID), "fghi", nil)
	server.ServeHTTP(part2.recorder, part2.request)
	if part1.recorder.Code != http.StatusOK || part2.recorder.Code != http.StatusOK {
		t.Fatalf("part statuses = %d %d", part1.recorder.Code, part2.recorder.Code)
	}

	body := `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>` + part1.recorder.Header().Get("ETag") + `</ETag></Part><Part><PartNumber>2</PartNumber><ETag>` + part2.recorder.Header().Get("ETag") + `</ETag></Part></CompleteMultipartUpload>`
	complete := signedRecorderRequest(t, http.MethodPost, "/photos/big.bin?uploadId="+url.QueryEscape(uploadID), body, nil)
	server.ServeHTTP(complete.recorder, complete.request)
	if complete.recorder.Code != http.StatusOK {
		t.Fatalf("complete status = %d body = %s", complete.recorder.Code, complete.recorder.Body.String())
	}
	if !strings.Contains(complete.recorder.Body.String(), "<ETag>&#34;8e18a6d3619b553c27c7028ea9067e05-2&#34;</ETag>") {
		t.Fatalf("complete body = %s", complete.recorder.Body.String())
	}

	get := signedRecorderRequest(t, http.MethodGet, "/photos/big.bin", "", nil)
	server.ServeHTTP(get.recorder, get.request)
	if get.recorder.Code != http.StatusOK || get.recorder.Body.String() != "abcdefghi" {
		t.Fatalf("get status = %d body = %q", get.recorder.Code, get.recorder.Body.String())
	}
	if get.recorder.Header().Get("ETag") != "\"8e18a6d3619b553c27c7028ea9067e05-2\"" {
		t.Fatalf("get headers = %v", get.recorder.Header())
	}
}
```

- [ ] **Step 6: Run S3 complete lifecycle test and verify it fails**

Run:

```bash
go test ./internal/s3api -run TestMultipartCompleteMakesObjectReadable
```

Expected: failure because complete handler still returns NotImplemented.

- [ ] **Step 7: Implement `completeMultipartUpload` handler**

Replace handler in `internal/s3api/server.go`:

```go
func (s *Server) completeMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) {
	uploadID := r.URL.Query().Get("uploadId")
	if uploadID == "" {
		WriteErrorResponse(w, r, ErrInvalidArgument, r.URL.Path, "")
		return
	}
	var request CompleteMultipartUploadRequest
	if err := xml.NewDecoder(r.Body).Decode(&request); err != nil {
		WriteErrorResponse(w, r, ErrInvalidArgument, r.URL.Path, "")
		return
	}
	parts := make([]store.CompletedPart, 0, len(request.Parts))
	for _, part := range request.Parts {
		parts = append(parts, store.CompletedPart{PartNumber: part.PartNumber, ETag: part.ETag})
	}
	result, err := s.store.CompleteMultipartUpload(r.Context(), store.CompleteMultipartUploadInput{Bucket: bucket, Key: key, UploadID: uploadID, Parts: parts})
	if err != nil {
		WriteErrorResponse(w, r, MapError(err), r.URL.Path, "")
		return
	}
	writeXML(w, http.StatusOK, CompleteMultipartUploadResult{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/", Bucket: bucket, Key: key, ETag: fmt.Sprintf("\"%s\"", result.ETag)})
}
```

- [ ] **Step 8: Run focused complete tests**

Run:

```bash
go test ./store -run TestStoreCompleteMultipartUploadCommitsChunksAndMultipartETag
go test ./internal/s3api -run TestMultipartCompleteMakesObjectReadable
```

Expected: both PASS.

- [ ] **Step 9: Commit Task 5**

```bash
git add store/store.go store/store_test.go internal/s3api/server.go internal/s3api/server_test.go
git commit -m "feat: complete multipart uploads"
```

## Task 6: Implement AbortMultipartUpload and session eviction

**Files:**
- Modify: `store/store.go`
- Modify: `store/store_test.go`
- Modify: `internal/s3api/server.go`
- Modify: `internal/s3api/server_test.go`

- [ ] **Step 1: Add failing store abort test**

Append to `store/store_test.go`:

```go
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
```

- [ ] **Step 2: Run abort test and verify it fails**

Run:

```bash
go test ./store -run TestStoreAbortMultipartUploadRemovesSession
```

Expected: failure because abort returns NotImplemented.

- [ ] **Step 3: Implement AbortMultipartUpload**

Replace abort stub in `store/store.go`:

```go
func (s *ObjectStore) AbortMultipartUpload(ctx context.Context, input AbortMultipartUploadInput) error {
	if input.UploadID == "" {
		return ErrInvalidArgument
	}
	s.multipartMu.Lock()
	defer s.multipartMu.Unlock()
	upload := s.multipartUploads[input.UploadID]
	if upload == nil || upload.bucket != input.Bucket || upload.key != input.Key {
		return ErrNoSuchUpload
	}
	s.logMultipartOrphansLocked(input.UploadID, "abort")
	delete(s.multipartUploads, input.UploadID)
	return nil
}
```

- [ ] **Step 4: Run abort test and verify it passes**

Run:

```bash
go test ./store -run TestStoreAbortMultipartUploadRemovesSession
```

Expected: PASS.

- [ ] **Step 5: Add failing eviction test**

Append to `store/store_test.go`:

```go
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
```

Ensure `store/store_test.go` imports include `fmt`; add it if missing.

- [ ] **Step 6: Run eviction test and verify it passes**

Run:

```bash
go test ./store -run TestStoreCreateMultipartUploadEvictsOldestSession
```

Expected: PASS if Task 3 eviction helper was implemented correctly; otherwise fix `evictOldestMultipartUploadLocked`.

- [ ] **Step 7: Add failing S3 abort handler test**

Append to `internal/s3api/server_test.go`:

```go
func TestMultipartAbortRemovesUpload(t *testing.T) {
	server := newSignedTestServer(t)

	create := signedRecorderRequest(t, http.MethodPost, "/photos/big.bin?uploads", "", nil)
	server.ServeHTTP(create.recorder, create.request)
	uploadID := extractBetween(create.recorder.Body.String(), "<UploadId>", "</UploadId>")
	if create.recorder.Code != http.StatusOK || uploadID == "" {
		t.Fatalf("create status = %d body = %s", create.recorder.Code, create.recorder.Body.String())
	}

	abort := signedRecorderRequest(t, http.MethodDelete, "/photos/big.bin?uploadId="+url.QueryEscape(uploadID), "", nil)
	server.ServeHTTP(abort.recorder, abort.request)
	if abort.recorder.Code != http.StatusNoContent {
		t.Fatalf("abort status = %d body = %s", abort.recorder.Code, abort.recorder.Body.String())
	}

	part := signedRecorderRequest(t, http.MethodPut, "/photos/big.bin?partNumber=1&uploadId="+url.QueryEscape(uploadID), "abc", nil)
	server.ServeHTTP(part.recorder, part.request)
	if part.recorder.Code != http.StatusNotFound || !strings.Contains(part.recorder.Body.String(), "NoSuchUpload") {
		t.Fatalf("part status = %d body = %s", part.recorder.Code, part.recorder.Body.String())
	}
}
```

- [ ] **Step 8: Run S3 abort test and verify it fails**

Run:

```bash
go test ./internal/s3api -run TestMultipartAbortRemovesUpload
```

Expected: failure because abort handler returns NotImplemented.

- [ ] **Step 9: Implement abort handler**

Replace `abortMultipartUpload` in `internal/s3api/server.go`:

```go
func (s *Server) abortMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) {
	uploadID := r.URL.Query().Get("uploadId")
	if uploadID == "" {
		WriteErrorResponse(w, r, ErrInvalidArgument, r.URL.Path, "")
		return
	}
	if err := s.store.AbortMultipartUpload(r.Context(), store.AbortMultipartUploadInput{Bucket: bucket, Key: key, UploadID: uploadID}); err != nil {
		WriteErrorResponse(w, r, MapError(err), r.URL.Path, "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

- [ ] **Step 10: Run focused abort/eviction tests**

Run:

```bash
go test ./store -run 'TestStore(AbortMultipartUploadRemovesSession|CreateMultipartUploadEvictsOldestSession)'
go test ./internal/s3api -run TestMultipartAbortRemovesUpload
```

Expected: all PASS.

- [ ] **Step 11: Commit Task 6**

```bash
git add store/store.go store/store_test.go internal/s3api/server.go internal/s3api/server_test.go
git commit -m "feat: abort multipart uploads"
```

## Task 7: Multipart validation, invisibility before complete, range regression, and ordinary PUT regression

**Files:**
- Modify: `store/store_test.go`
- Modify: `internal/s3api/server_test.go`
- Modify if needed: `store/store.go`
- Modify if needed: `internal/s3api/server.go`

- [ ] **Step 1: Add failing complete validation tests**

Append to `store/store_test.go`:

```go
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
```

- [ ] **Step 2: Run validation tests and verify expected result**

Run:

```bash
go test ./store -run TestStoreCompleteMultipartUploadValidatesParts
```

Expected: PASS if validation from Task 5 is complete. If it fails, fix only `completeParts` / `etagMatches` until it passes.

- [ ] **Step 3: Add object invisibility before complete test**

Append to `internal/s3api/server_test.go`:

```go
func TestMultipartUploadPartDoesNotExposeObjectBeforeComplete(t *testing.T) {
	server := newSignedTestServer(t)

	create := signedRecorderRequest(t, http.MethodPost, "/photos/pending.bin?uploads", "", nil)
	server.ServeHTTP(create.recorder, create.request)
	uploadID := extractBetween(create.recorder.Body.String(), "<UploadId>", "</UploadId>")
	part := signedRecorderRequest(t, http.MethodPut, "/photos/pending.bin?partNumber=1&uploadId="+url.QueryEscape(uploadID), "abc", nil)
	server.ServeHTTP(part.recorder, part.request)
	if part.recorder.Code != http.StatusOK {
		t.Fatalf("part status = %d body = %s", part.recorder.Code, part.recorder.Body.String())
	}

	get := signedRecorderRequest(t, http.MethodGet, "/photos/pending.bin", "", nil)
	server.ServeHTTP(get.recorder, get.request)
	if get.recorder.Code != http.StatusNotFound || !strings.Contains(get.recorder.Body.String(), "NoSuchKey") {
		t.Fatalf("get status = %d body = %s", get.recorder.Code, get.recorder.Body.String())
	}
}
```

Run:

```bash
go test ./internal/s3api -run TestMultipartUploadPartDoesNotExposeObjectBeforeComplete
```

Expected: PASS if unfinished state stays out of metadata. If it fails, ensure `UploadPart` does not call `meta.PutObject`.

- [ ] **Step 4: Add range regression test across variable-size multipart chunks**

Append to `store/store_test.go`:

```go
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
```

Run:

```bash
go test ./store -run TestStoreMultipartRangeGetAcrossVariableChunks
```

Expected: PASS.

- [ ] **Step 5: Add ordinary PutObject regression test**

Append to `store/store_test.go`:

```go
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
```

Run:

```bash
go test ./store -run TestStorePutObjectStillUsesWholeObjectMD5AndSHA256
```

Expected: PASS.

- [ ] **Step 6: Add S3 complete error mapping tests**

Append to `internal/s3api/server_test.go`:

```go
func TestMultipartCompleteRejectsMissingPart(t *testing.T) {
	server := newSignedTestServer(t)
	create := signedRecorderRequest(t, http.MethodPost, "/photos/big.bin?uploads", "", nil)
	server.ServeHTTP(create.recorder, create.request)
	uploadID := extractBetween(create.recorder.Body.String(), "<UploadId>", "</UploadId>")

	body := `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"missing"</ETag></Part></CompleteMultipartUpload>`
	complete := signedRecorderRequest(t, http.MethodPost, "/photos/big.bin?uploadId="+url.QueryEscape(uploadID), body, nil)
	server.ServeHTTP(complete.recorder, complete.request)
	if complete.recorder.Code != http.StatusBadRequest || !strings.Contains(complete.recorder.Body.String(), "InvalidPart") {
		t.Fatalf("complete status = %d body = %s", complete.recorder.Code, complete.recorder.Body.String())
	}
}

func TestMultipartCompleteRejectsInvalidPartOrder(t *testing.T) {
	server := newSignedTestServer(t)
	create := signedRecorderRequest(t, http.MethodPost, "/photos/big.bin?uploads", "", nil)
	server.ServeHTTP(create.recorder, create.request)
	uploadID := extractBetween(create.recorder.Body.String(), "<UploadId>", "</UploadId>")
	part1 := signedRecorderRequest(t, http.MethodPut, "/photos/big.bin?partNumber=1&uploadId="+url.QueryEscape(uploadID), "abc", nil)
	server.ServeHTTP(part1.recorder, part1.request)
	part2 := signedRecorderRequest(t, http.MethodPut, "/photos/big.bin?partNumber=2&uploadId="+url.QueryEscape(uploadID), "def", nil)
	server.ServeHTTP(part2.recorder, part2.request)

	body := `<CompleteMultipartUpload><Part><PartNumber>2</PartNumber><ETag>` + part2.recorder.Header().Get("ETag") + `</ETag></Part><Part><PartNumber>1</PartNumber><ETag>` + part1.recorder.Header().Get("ETag") + `</ETag></Part></CompleteMultipartUpload>`
	complete := signedRecorderRequest(t, http.MethodPost, "/photos/big.bin?uploadId="+url.QueryEscape(uploadID), body, nil)
	server.ServeHTTP(complete.recorder, complete.request)
	if complete.recorder.Code != http.StatusBadRequest || !strings.Contains(complete.recorder.Body.String(), "InvalidPartOrder") {
		t.Fatalf("complete status = %d body = %s", complete.recorder.Code, complete.recorder.Body.String())
	}
}
```

Run:

```bash
go test ./internal/s3api -run 'TestMultipartCompleteRejects(MissingPart|InvalidPartOrder)'
```

Expected: PASS.

- [ ] **Step 7: Run focused regression suite**

Run:

```bash
go test ./store -run 'TestStore(Multipart|CompleteMultipartUploadValidatesParts|PutObjectStillUsesWholeObjectMD5AndSHA256)'
go test ./internal/s3api -run 'Multipart|CreateMultipartUpload|UploadPart|CompleteMultipartUpload|AbortMultipartUpload'
```

Expected: PASS.

- [ ] **Step 8: Commit Task 7**

```bash
git add store/store.go store/store_test.go internal/s3api/server.go internal/s3api/server_test.go
git commit -m "test: cover multipart upload regressions"
```

## Task 8: Full verification and review loop preparation

**Files:**
- No code changes expected unless tests reveal issues.

- [ ] **Step 1: Run focused tests**

Run:

```bash
go test ./internal/s3api -run 'Multipart|CreateMultipartUpload|UploadPart|CompleteMultipartUpload|AbortMultipartUpload'
go test ./store -run Multipart
```

Expected: PASS.

- [ ] **Step 2: Run full test suite**

Run:

```bash
go test ./...
```

Expected: PASS.

- [ ] **Step 3: If tests fail, fix with TDD**

For any failure:

1. Read the full failure output.
2. Identify whether the failure is a new multipart behavior issue or a regression.
3. Add or adjust the narrowest failing test that captures the expected behavior.
4. Implement the smallest fix.
5. Re-run the focused package test.
6. Re-run `go test ./...`.

- [ ] **Step 4: Commit verification fixes if any**

If Step 3 changed code, stage the specific files changed by that fix and commit them:

```bash
git status --short
git add store/store.go store/store_test.go internal/s3api/server.go internal/s3api/server_test.go internal/s3api/errors.go internal/s3api/xml.go
git commit -m "fix: stabilize multipart upload behavior"
```

If only a subset of those files changed, stage only the changed files shown by `git status --short`.

## Required post-implementation review loop

After all implementation tasks pass verification, run subagent review loops at least three times and continue until no issues remain.

Each review loop must include:

1. A spec compliance review against `docs/superpowers/specs/2026-05-25-tgnas-s3-multipart-upload-design.md` and this plan.
2. A code quality review of the actual diff.
3. Fix all Critical and Important issues.
4. Re-run focused tests and `go test ./...` after fixes.

Minimum required loops:

- Review loop 1: full multipart lifecycle and S3 compatibility.
- Review loop 2: store concurrency, memory session eviction, orphan logging, and metadata correctness.
- Review loop 3: regression risks for existing PutObject/GetObject/Range/ListObjects/auth behavior.

Stop only when at least three loops have completed and the latest loop reports no Critical or Important issues.
