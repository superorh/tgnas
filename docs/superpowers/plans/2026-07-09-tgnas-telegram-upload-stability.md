# TgNAS Telegram Upload Stability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make batch uploads complete reliably when Telegram Bot API returns `429 Too Many Requests` by serializing uploads, making single-upload bodies replayable, and preserving current download behavior.

**Architecture:** Keep retry logic inside `telegram.HTTPClient`, but expose final rate-limit failures in a recognizable form that `store.ObjectStore` can use to enforce a shared upload cooldown. Replace `putSingle` pipe streaming with replayable staging based on `PutBufferSize`, keep chunked upload flow intact, and move upload serialization into a dedicated upload-only gate that does not govern downloads.

**Tech Stack:** Go 1.24, stdlib (`os`, `bytes`, `io`, `sync`, `time`, `errors`), existing `store`, `telegram`, SQLite-backed metadata tests, `httptest`

## Global Constraints

- Prefer completion over throughput.
- It is acceptable to make Telegram uploads effectively serial for a single bot.
- Avoid introducing a durable background queue or a new subsystem for this iteration.
- Keep changes focused on the existing `store` and `telegram` packages.
- Preserve current orphan logging behavior when Telegram upload succeeds but metadata commit fails.
- Upload gate applies only to upload operations. Download operations keep their existing path and continue to use the current Telegram call concurrency controls without going through the new upload gate.
- Hold the upload slot for the full duration of a single `tg.Upload` call, including the Telegram client's internal retries and any `retry_after` sleeps within that call.
- Track a shared cooldown deadline only when a `tg.Upload` call ultimately returns a final rate-limit (`429`) failure.
- If `input.Size <= PutBufferSize`, use in-memory buffering and `bytes.Reader`.
- If `input.Size > PutBufferSize`, write to a temporary file and upload from `*os.File`.
- If `PutBufferSize <= 0`, fall back to `DefaultUploadConfig().PutBufferSize` before applying the rule.
- Temporary file cleanup must happen on upload success, upload failure, context cancellation, and metadata commit failure after upload success.

---

### Task 1: Expose final Telegram rate-limit failures

**Files:**
- Modify: `telegram/client.go:182-263`
- Modify: `telegram/client_test.go:175-266`

**Interfaces:**
- Consumes: `shouldRetryTelegram(statusCode int, data []byte) (bool, time.Duration, error)` in `telegram/client.go`
- Produces: `func NewRateLimitError(cause error, retryAfter time.Duration) error` in `telegram/client.go`
- Produces: `func IsRateLimitError(err error) (time.Duration, bool)` in `telegram/client.go`
- Produces: final upload errors from `(*HTTPClient).doUploadRequest` that wrap rate-limit metadata only when the upload ultimately returns a final `429`

- [ ] **Step 1: Write the failing tests**

```go
func TestClientUploadReturnsRateLimitErrorMetadataOnFinal429(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		mustDrainBody(t, w, r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"description":"retry later","parameters":{"retry_after":9}}`))
	}))
	defer server.Close()

	client := NewHTTPClient("token", server.URL, http.DefaultClient)
	_, err := client.Upload(context.Background(), UploadRequest{Type: TypeDocument, ChatID: "-100", Reader: strings.NewReader("hello"), Filename: "hello.txt"})
	if err == nil {
		t.Fatal("Upload returned nil error")
	}
	retryAfter, ok := IsRateLimitError(err)
	if !ok {
		t.Fatalf("IsRateLimitError(%v) = false, want true", err)
	}
	if retryAfter != 9*time.Second {
		t.Fatalf("retryAfter = %v, want %v", retryAfter, 9*time.Second)
	}
	if attempts != 4 {
		t.Fatalf("Upload attempts = %d, want 4", attempts)
	}
}

func TestClientUploadSuccessfulRetryDoesNotReturnRateLimitErrorMetadata(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		mustDrainBody(t, w, r)
		w.Header().Set("Content-Type", "application/json")
		if attempts == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"ok":false,"description":"retry later","parameters":{"retry_after":1}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":77,"document":{"file_id":"file-1","file_unique_id":"unique-1","file_size":5}}}`))
	}))
	defer server.Close()

	client := NewHTTPClient("token", server.URL, http.DefaultClient)
	_, err := client.Upload(context.Background(), UploadRequest{Type: TypeDocument, ChatID: "-100", Reader: strings.NewReader("hello"), Filename: "hello.txt"})
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}
	if retryAfter, ok := IsRateLimitError(err); ok {
		t.Fatalf("IsRateLimitError(%v) = (%v, true), want false", err, retryAfter)
	}
	if attempts != 2 {
		t.Fatalf("Upload attempts = %d, want 2", attempts)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./telegram -run 'TestClientUploadReturnsRateLimitErrorMetadataOnFinal429|TestClientUploadSuccessfulRetryDoesNotReturnRateLimitErrorMetadata' -count=1`
Expected: FAIL because `IsRateLimitError` does not exist and final `429` errors do not preserve retry metadata.

- [ ] **Step 3: Write minimal implementation**

```go
type rateLimitError struct {
	cause      error
	retryAfter time.Duration
}

func (e *rateLimitError) Error() string {
	return e.cause.Error()
}

func (e *rateLimitError) Unwrap() error {
	return e.cause
}

func NewRateLimitError(cause error, retryAfter time.Duration) error {
	if cause == nil || retryAfter <= 0 {
		return cause
	}
	return &rateLimitError{cause: cause, retryAfter: retryAfter}
}

func IsRateLimitError(err error) (time.Duration, bool) {
	var target *rateLimitError
	if !errors.As(err, &target) {
		return 0, false
	}
	return target.retryAfter, true
}

func wrapRateLimitError(err error, retry bool, delay time.Duration) error {
	if err == nil || !retry || delay <= 0 {
		return err
	}
	return NewRateLimitError(err, delay)
}
```

```go
func (c *HTTPClient) doUploadRequest(ctx context.Context, requestURL string, request UploadRequest, fieldName string) ([]byte, error) {
	var lastErr error
	var lastDelay time.Duration
	readSeeker, replayable := request.Reader.(io.ReadSeeker)

	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			if !replayable {
				if lastErr != nil {
					return nil, lastErr
				}
				return nil, errors.New("telegram upload cannot retry non-seekable reader")
			}
			if _, err := readSeeker.Seek(0, io.SeekStart); err != nil {
				return nil, fmt.Errorf("reset telegram upload reader: %w", err)
			}
		}

		attemptRequest := request
		attemptRequest.Reader = request.Reader
		if replayable {
			attemptRequest.Reader = readSeeker
		}
		reader, contentType := c.uploadBody(attemptRequest, fieldName)
		data, retry, delay, err := c.doSingleUploadAttempt(ctx, requestURL, reader, contentType)
		if err == nil {
			return data, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !retry {
			return nil, err
		}
		lastErr = err
		lastDelay = delay
		if !replayable {
			return nil, safeUploadReplayError(err)
		}
		if attempt == 3 {
			break
		}
		if err := sleepWithContext(ctx, backoffDelay(ctx, c.httpClient.Timeout, attempt, delay)); err != nil {
			return nil, err
		}
	}

	if lastErr == nil {
		lastErr = errors.New("telegram upload failed")
	}
	return nil, wrapRateLimitError(lastErr, true, lastDelay)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./telegram -run 'TestClientUploadReturnsRateLimitErrorMetadataOnFinal429|TestClientUploadSuccessfulRetryDoesNotReturnRateLimitErrorMetadata|TestClientUploadRetriesRetryableStatusWithReadSeeker|TestClientUploadRetryAfterWithReadSeekerHonorsContextDeadline|TestClientUploadNonSeekableReaderCannotSafelyRetry' -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add telegram/client.go telegram/client_test.go
git commit -m "feat(telegram): expose final upload rate limits"
```

### Task 2: Replace single-upload pipe streaming with replayable staging

**Files:**
- Modify: `store/store.go:802-918`
- Modify: `store/store_test.go:856-873`
- Test: `store/store_test.go`

**Interfaces:**
- Consumes: `func IsRateLimitError(err error) (time.Duration, bool)` from `telegram/client.go`
- Produces: replayable single-upload staging inside `func (s *ObjectStore) putSingle(ctx context.Context, input PutObjectInput, strategy UploadStrategy) (PutObjectResult, error)`
- Produces: helper shape in `store/store.go` such as `type stagedUpload struct { reader io.ReadSeeker; close func() error; remove func() error }`

- [ ] **Step 1: Write the failing tests**

```go
type nonSeekableReader struct{ io.Reader }

func TestStorePutObjectSingleUploadRetriesThroughLocalStaging(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll returned error: %v", err)
		}
		_ = r.Body.Close()
		if !strings.Contains(string(data), "hello") {
			t.Fatalf("request body missing payload: %q", string(data))
		}
		w.Header().Set("Content-Type", "application/json")
		if attempts == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"ok":false,"description":"retry later","parameters":{"retry_after":1}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":77,"document":{"file_id":"file-1","file_unique_id":"file-1-u","file_size":5}}}`))
	}))
	defer server.Close()

	client := telegram.NewHTTPClient("token", server.URL, http.DefaultClient)
	meta, err := metadata.OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	ctx := context.Background()
	if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: "photos", ChatID: "-100", CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
		t.Fatalf("UpsertBucket returned error: %v", err)
	}

	store := mustNewObjectStore(t, meta, client, Options{Upload: DefaultUploadConfig()})
	_, err = store.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: "hello.txt", ContentType: "text/plain", Size: 5, Body: nonSeekableReader{Reader: strings.NewReader("hello")}})
	if err != nil {
		t.Fatalf("PutObject returned error: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("upload attempts = %d, want 2", attempts)
	}
}

func TestStorePutObjectStillUsesWholeObjectMD5AndSHA256(t *testing.T) {
	ctx := context.Background()
	objectStore, _ := newReadyTestObjectStore(t, map[string]string{"photos": "-100"})
	result, err := objectStore.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: "hello.txt", ContentType: "text/plain", Size: 5, Body: nonSeekableReader{Reader: strings.NewReader("hello")}})
	if err != nil {
		t.Fatalf("PutObject returned error: %v", err)
	}
	if result.ETag != "5d41402abc4b2a76b9719d911017c592" {
		t.Fatalf("etag = %q", result.ETag)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./store -run 'TestStorePutObjectSingleUploadRetriesThroughLocalStaging|TestStorePutObjectStillUsesWholeObjectMD5AndSHA256' -count=1`
Expected: FAIL because `putSingle` still uses `io.Pipe` and non-seekable input cannot survive a retry.

- [ ] **Step 3: Write minimal implementation**

```go
type stagedUpload struct {
	reader io.ReadSeeker
	close  func() error
	remove func() error
}

func (s *ObjectStore) stageSingleUpload(input PutObjectInput, md5Hash hash.Hash, shaHash hash.Hash) (*stagedUpload, error) {
	bufferSize := s.options.Upload.PutBufferSize
	if bufferSize <= 0 {
		bufferSize = DefaultUploadConfig().PutBufferSize
	}
	if input.Size <= int64(bufferSize) {
		data, err := io.ReadAll(io.TeeReader(input.Body, io.MultiWriter(md5Hash, shaHash)))
		if err != nil {
			return nil, err
		}
		if int64(len(data)) != input.Size {
			return nil, fmt.Errorf("copied %d bytes, want %d", len(data), input.Size)
		}
		return &stagedUpload{
			reader: bytes.NewReader(data),
			close:  func() error { return nil },
			remove: func() error { return nil },
		}, nil
	}

	file, err := os.CreateTemp("", "tgnas-upload-*")
	if err != nil {
		return nil, err
	}
	written, err := io.Copy(file, io.TeeReader(input.Body, io.MultiWriter(md5Hash, shaHash)))
	if err != nil {
		file.Close()
		os.Remove(file.Name())
		return nil, err
	}
	if written != input.Size {
		file.Close()
		os.Remove(file.Name())
		return nil, fmt.Errorf("copied %d bytes, want %d", written, input.Size)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		os.Remove(file.Name())
		return nil, err
	}
	return &stagedUpload{
		reader: file,
		close:  file.Close,
		remove: func() error { return os.Remove(file.Name()) },
	}, nil
}
```

```go
func (s *ObjectStore) putSingle(ctx context.Context, input PutObjectInput, strategy UploadStrategy) (PutObjectResult, error) {
	now := time.Now().UTC()
	md5Hash := md5.New()
	shaHash := sha256.New()
	staged, err := s.stageSingleUpload(input, md5Hash, shaHash)
	if err != nil {
		return PutObjectResult{}, err
	}
	defer staged.close()
	defer func() {
		if err := staged.remove(); err != nil && s.logger != nil {
			s.logger.Printf("debug event=upload_staging_cleanup result=error error=%q", sanitizeLogError(err))
		}
	}()

	caption := s.renderCaption(input, 1, 1)
	s.logger.Printf("debug event=telegram_upload_part bucket=%q key=%q part=%d parts=%d media_type=%q", input.Bucket, input.Key, 1, 1, strategy.TelegramType)
	uploaded, err := s.uploadTelegram(ctx, telegram.UploadRequest{
		Type:     strategy.TelegramType,
		ChatID:   s.bucketChatID(input.Bucket),
		Reader:   staged.reader,
		Filename: path.Base(input.Key),
		MIMEType: input.ContentType,
		Caption:  caption,
	})
	if err != nil {
		return PutObjectResult{}, err
	}
	// keep existing object/chunk metadata write logic unchanged
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./store -run 'TestStorePutObjectSingleUploadRetriesThroughLocalStaging|TestStorePutObjectStillUsesWholeObjectMD5AndSHA256|TestStoreTypedUploadUsesTelegramReturnedFileSize|TestStoreTypedUploadETagNotPlainMD5' -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add store/store.go store/store_test.go
git commit -m "feat(store): stage single uploads for retry safety"
```

### Task 3: Add upload-only serialization and shared cooldown gate

**Files:**
- Modify: `store/store.go:29-31`
- Modify: `store/store.go:70-114`
- Modify: `store/store.go:1029-1061`
- Modify: `store/store_test.go:483-597`

**Interfaces:**
- Consumes: `func IsRateLimitError(err error) (time.Duration, bool)` from `telegram/client.go`
- Produces: upload-only gate state on `ObjectStore`, for example `uploadMu sync.Mutex` and `uploadCooldownUntil time.Time`
- Produces: serialized `func (s *ObjectStore) uploadTelegram(ctx context.Context, request telegram.UploadRequest) (telegram.UploadedFile, error)` while leaving `downloadChunk` on `telegramSem`

- [ ] **Step 1: Write the failing tests**

```go
func TestStoreUploadGateRecordsCooldownFromFinalRateLimit(t *testing.T) {
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
	fake.UploadFunc = func(ctx context.Context, request telegram.UploadRequest) (telegram.UploadedFile, error) {
		started <- request.Filename
		_, _ = io.ReadAll(request.Reader)
		if request.Filename == "one.txt" {
			return telegram.UploadedFile{}, telegram.NewRateLimitError(errors.New("Too Many Requests: retry after 1"), time.Second)
		}
		return telegram.UploadedFile{Type: request.Type, FileID: request.Filename, FileUniqueID: request.Filename + "-u", MessageID: 1, FileSize: 1}, nil
	}
	store := mustNewObjectStore(t, meta, fake, Options{Upload: DefaultUploadConfig(), MaxTelegramCalls: 2})

	firstDone := make(chan error, 1)
	go func() {
		_, err := store.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: "one.txt", ContentType: "text/plain", Size: 1, Body: strings.NewReader("a")})
		firstDone <- err
	}()
	if first := <-started; first != "one.txt" {
		t.Fatalf("first upload = %q, want one.txt", first)
	}
	if err := <-firstDone; err == nil {
		t.Fatal("first PutObject returned nil error")
	}

	secondCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err = store.PutObject(secondCtx, PutObjectInput{Bucket: "photos", Key: "two.txt", ContentType: "text/plain", Size: 1, Body: strings.NewReader("b")})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second PutObject err = %v, want context.DeadlineExceeded", err)
	}
	select {
	case second := <-started:
		t.Fatalf("second upload started during cooldown: %s", second)
	default:
	}
}

func TestStoreUploadSerializationDoesNotBlockDownloads(t *testing.T) {
	ctx := context.Background()
	objectStore, fake := newReadyTestObjectStore(t, map[string]string{"photos": "-100"})
	_, err := objectStore.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: "hello.txt", ContentType: "text/plain", Size: 5, Body: strings.NewReader("hello")})
	if err != nil {
		t.Fatalf("PutObject returned error: %v", err)
	}

	uploadRelease := make(chan struct{})
	uploadStarted := make(chan struct{}, 1)
	fake.UploadFunc = func(ctx context.Context, request telegram.UploadRequest) (telegram.UploadedFile, error) {
		uploadStarted <- struct{}{}
		<-uploadRelease
		_, _ = io.ReadAll(request.Reader)
		return telegram.UploadedFile{Type: request.Type, FileID: "upload-file", FileUniqueID: "upload-file-u", MessageID: 1, FileSize: 1}, nil
	}
	downloadStarted := make(chan struct{}, 1)
	fake.DownloadFunc = func(ctx context.Context, fileID string) (io.ReadCloser, error) {
		downloadStarted <- struct{}{}
		return io.NopCloser(strings.NewReader(fake.Files[fileID])), nil
	}

	putDone := make(chan error, 1)
	go func() {
		_, err := objectStore.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: "slow.txt", ContentType: "text/plain", Size: 1, Body: strings.NewReader("x")})
		putDone <- err
	}()
	<-uploadStarted

	reader, _, err := objectStore.GetObject(ctx, GetObjectInput{Bucket: "photos", Key: "hello.txt"})
	if err != nil {
		t.Fatalf("GetObject returned error: %v", err)
	}
	defer reader.Close()
	<-downloadStarted
	close(uploadRelease)
	if err := <-putDone; err != nil {
		t.Fatalf("PutObject returned error: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./store -run 'TestStoreUploadGateRecordsCooldownFromFinalRateLimit|TestStoreUploadSerializationDoesNotBlockDownloads|TestStoreMaxTelegramCallsSerializationWhenSetToOne|TestStoreMaxDownloadsSerializationWhenSetToOne' -count=1`
Expected: FAIL because there is no upload-only cooldown state yet, and upload behavior is still controlled by the current Telegram concurrency path rather than the new upload gate.

- [ ] **Step 3: Write minimal implementation**

```go
type ObjectStore struct {
	// existing fields...
	telegramSem chan struct{}
	uploadMu    sync.Mutex
	uploadUntil time.Time
}
```

```go
func (s *ObjectStore) uploadTelegram(ctx context.Context, request telegram.UploadRequest) (telegram.UploadedFile, error) {
	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()

	for {
		wait := time.Until(s.uploadUntil)
		if wait <= 0 {
			break
		}
		if err := sleepWithContext(ctx, wait); err != nil {
			return telegram.UploadedFile{}, err
		}
	}

	uploaded, err := s.tg.Upload(ctx, request)
	if err != nil {
		if retryAfter, ok := telegram.IsRateLimitError(err); ok && retryAfter > 0 {
			s.uploadUntil = time.Now().Add(retryAfter)
		}
		return telegram.UploadedFile{}, err
	}
	return uploaded, nil
}
```

```go
func sleepWithContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./store -run 'TestStoreUploadGateRecordsCooldownFromFinalRateLimit|TestStoreUploadSerializationDoesNotBlockDownloads|TestStoreMaxTelegramCallsSerializationWhenSetToOne|TestStoreMaxDownloadsSerializationWhenSetToOne' -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add store/store.go store/store_test.go
git commit -m "feat(store): serialize uploads with cooldown gate"
```

### Task 4: Switch chunked paths to replayable byte readers and clean up test fixtures

**Files:**
- Modify: `store/store.go:316-386`
- Modify: `store/store.go:920-1011`
- Modify: `internal/testutil/faketelegram.go:26-49`
- Modify: `store/store_test.go:268-315`

**Interfaces:**
- Consumes: existing `uploadMultipartPartChunks` and `putChunked` chunk loops in `store/store.go`
- Produces: `bytes.NewReader(partData)` for chunked Telegram uploads
- Produces: fake Telegram fixture that records uploaded bytes without `string(partData)` round-tripping in stored `UploadRequest.Reader`

- [ ] **Step 1: Write the failing tests**

```go
func TestStoreChunkedUploadUsesReplayableByteReaders(t *testing.T) {
	ctx := context.Background()
	objectStore, fake := newReadyTestObjectStoreWithUploadConfig(t, map[string]string{"backups": "-200"}, UploadConfig{Strategy: "document", EnableChunking: true, MaxFileSize: 50, ChunkSize: 3, TypeLimits: map[string]int64{"document": 3}, PutBufferSize: 2})
	attempts := 0
	fake.UploadFunc = func(ctx context.Context, request telegram.UploadRequest) (telegram.UploadedFile, error) {
		attempts++
		if _, ok := request.Reader.(io.ReadSeeker); !ok {
			return telegram.UploadedFile{}, fmt.Errorf("reader does not implement io.ReadSeeker")
		}
		data, err := io.ReadAll(request.Reader)
		if err != nil {
			return telegram.UploadedFile{}, err
		}
		return telegram.UploadedFile{Type: request.Type, FileID: fmt.Sprintf("file-%d", attempts), FileUniqueID: fmt.Sprintf("file-%d-u", attempts), MessageID: int64(attempts), FileSize: int64(len(data))}, nil
	}

	_, err := objectStore.PutObject(ctx, PutObjectInput{Bucket: "backups", Key: "big.bin", ContentType: "application/octet-stream", Size: 6, Body: strings.NewReader("abcdef")})
	if err != nil {
		t.Fatalf("PutObject returned error: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("upload attempts = %d, want 2", attempts)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./store ./internal/testutil -run 'TestStoreChunkedUploadUsesReplayableByteReaders|TestStoreLogsOrphanUploadWhenChunkedUploadFailsAfterEarlierChunks|TestStoreUploadPartSplitsIntoTelegramChunks' -count=1`
Expected: FAIL because chunked upload paths still use `strings.NewReader(string(partData))`, and the fake Telegram helper still normalizes requests through `strings.NewReader(string(data))`.

- [ ] **Step 3: Write minimal implementation**

```go
uploaded, err := s.uploadTelegram(ctx, telegram.UploadRequest{
	Type:     telegram.TypeDocument,
	ChatID:   s.bucketChatID(input.Bucket),
	Reader:   bytes.NewReader(partData),
	Filename: path.Base(input.Key),
	MIMEType: input.ContentType,
	Caption:  s.renderCaption(input, part, parts),
})
```

```go
uploaded, err := s.uploadTelegram(ctx, telegram.UploadRequest{
	Type:     telegram.TypeDocument,
	ChatID:   s.bucketChatID(input.Bucket),
	Reader:   bytes.NewReader(partData),
	Filename: path.Base(input.Key),
	MIMEType: contentType,
	Caption:  s.renderCaption(PutObjectInput{Bucket: input.Bucket, Key: input.Key, ContentType: contentType, Size: input.Size}, partIndex, totalParts),
})
```

```go
request.Reader = bytes.NewReader(data)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./store ./internal/testutil -run 'TestStoreChunkedUploadUsesReplayableByteReaders|TestStoreLogsOrphanUploadWhenChunkedUploadFailsAfterEarlierChunks|TestStoreUploadPartSplitsIntoTelegramChunks' -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add store/store.go store/store_test.go internal/testutil/faketelegram.go
git commit -m "refactor(store): use replayable readers for chunk uploads"
```

### Task 5: Verify cleanup and full regression coverage

**Files:**
- Modify: `store/store_test.go:216-399`
- Modify: `telegram/client_test.go:217-266`
- Test: `store/store_test.go`
- Test: `telegram/client_test.go`

**Interfaces:**
- Consumes: staging helpers from `store/store.go`, rate-limit error helpers from `telegram/client.go`
- Produces: regression coverage for cleanup, cooldown, and retry behavior without changing production interfaces

- [ ] **Step 1: Write the failing tests**

```go
func TestStoreSingleUploadTempFileRemovedAfterMetadataFailure(t *testing.T) {
	ctx := context.Background()
	tempRoot := t.TempDir()
	t.Setenv("TMPDIR", tempRoot)
	meta, err := metadata.OpenSQLite(filepath.Join(tempRoot, "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: "photos", ChatID: "-100", CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
		t.Fatalf("UpsertBucket returned error: %v", err)
	}
	fake := testutil.NewFakeTelegram()
	store := mustNewObjectStore(t, failingMetadataStore{Store: meta, putErr: errors.New("metadata commit failed")}, fake, Options{Upload: UploadConfig{Strategy: "document", EnableChunking: true, MaxFileSize: 50, ChunkSize: 20 * 1024 * 1024, TypeLimits: map[string]int64{"document": 20 * 1024 * 1024}, PutBufferSize: 1}})
	_, err = store.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: "large.txt", ContentType: "text/plain", Size: 5, Body: strings.NewReader("hello")})
	if err == nil {
		t.Fatal("PutObject returned nil error")
	}
	matches, err := filepath.Glob(filepath.Join(tempRoot, "tgnas-upload-*"))
	if err != nil {
		t.Fatalf("Glob returned error: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("staging temp files still present: %v", matches)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./store ./telegram -run 'TestStoreSingleUploadTempFileRemovedAfterMetadataFailure|TestClientUploadReturnsRateLimitErrorMetadataOnFinal429|TestStoreUploadGateRecordsCooldownFromFinalRateLimit' -count=1`
Expected: FAIL until cleanup and cooldown behavior are both wired through the final implementation.

- [ ] **Step 3: Write minimal implementation**

```go
func removeStagingArtifact(logger *log.Logger, staged *stagedUpload) {
	if staged == nil {
		return
	}
	if err := staged.close(); err != nil && logger != nil {
		logger.Printf("debug event=upload_staging_close result=error error=%q", sanitizeLogError(err))
	}
	if err := staged.remove(); err != nil && logger != nil {
		logger.Printf("debug event=upload_staging_cleanup result=error error=%q", sanitizeLogError(err))
	}
}
```

```go
// call removeStagingArtifact from putSingle with defer immediately after staging succeeds
// so success, upload failure, context cancellation, and metadata failure all share one cleanup path.
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./store ./telegram ./internal/testutil -count=1`
Expected: PASS

Run: `go test ./... -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add store/store_test.go telegram/client_test.go store/store.go telegram/client.go internal/testutil/faketelegram.go
git commit -m "test: cover telegram upload stability regressions"
```

## Spec Coverage Check

- Upload retry metadata on final `429`: Task 1
- Replayable single-upload staging using `PutBufferSize`: Task 2
- Upload-only serialization and cooldown gate: Task 3
- Chunked and multipart byte-reader replayability: Task 4
- Temp-file cleanup and end-to-end regressions: Task 5
- Preserve download behavior and orphan logging semantics: Tasks 3 and 5

## Verification

- Focused client retry verification:
  - `go test ./telegram -run 'TestClientUploadReturnsRateLimitErrorMetadataOnFinal429|TestClientUploadSuccessfulRetryDoesNotReturnRateLimitErrorMetadata|TestClientUploadRetriesRetryableStatusWithReadSeeker|TestClientUploadRetryAfterWithReadSeekerHonorsContextDeadline|TestClientUploadNonSeekableReaderCannotSafelyRetry' -count=1`
- Focused store staging and cooldown verification:
  - `go test ./store -run 'TestStorePutObjectSingleUploadRetriesThroughLocalStaging|TestStoreUploadGateRecordsCooldownFromFinalRateLimit|TestStoreUploadSerializationDoesNotBlockDownloads|TestStoreChunkedUploadUsesReplayableByteReaders' -count=1`
- Full store and telegram suite:
  - `go test ./store ./telegram ./internal/testutil -count=1`
- Full repository regression:
  - `go test ./... -count=1`

- Manual code review checkpoints:
  - Confirm `store/downloadChunk` still uses `telegramSem` and does not consult upload cooldown.
  - Confirm `store/putSingle` no longer allocates `io.Pipe` or launches a background upload goroutine.
  - Confirm both chunked upload call sites use `bytes.NewReader(partData)`.
  - Confirm final returned `429` errors from `telegram/client.go` preserve retry metadata and successful internal retries do not.
