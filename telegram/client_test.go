package telegram

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClientUploadDocumentSendsCaptionAndParsesFile(t *testing.T) {
	var path string
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		body = mustReadBodyString(t, w, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":77,"document":{"file_id":"file-1","file_unique_id":"unique-1","file_size":5,"mime_type":"text/plain","file_name":"hello.txt"}}}`))
	}))
	defer server.Close()

	client := NewHTTPClient("token", server.URL, http.DefaultClient)
	uploaded, err := client.Upload(context.Background(), UploadRequest{Type: TypeDocument, ChatID: "-100", Reader: strings.NewReader("hello"), Filename: "hello.txt", MIMEType: "text/plain", Caption: "caption text"})
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}
	if path != "/bottoken/sendDocument" {
		t.Fatalf("path = %q", path)
	}
	if !strings.Contains(body, `name="caption"`) || !strings.Contains(body, "caption text") || !strings.Contains(body, `name="document"`) {
		t.Fatalf("multipart body missing fields: %s", body)
	}
	if uploaded.FileID != "file-1" || uploaded.MessageID != 77 || uploaded.FileSize != 5 {
		t.Fatalf("uploaded = %+v", uploaded)
	}
}

// TestClientUploadDocumentFallsBackToVideoField guards against a real
// production incident (2026-07-14): Telegram reclassified certain mp4
// uploads sent via sendDocument, returning the "video" field populated and
// "document" empty — tgnas only checked "document" and failed the upload
// outright even though it genuinely succeeded on Telegram's side.
func TestClientUploadDocumentFallsBackToVideoField(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":77,"document":{},"video":{"file_id":"video-file-1","file_unique_id":"video-unique-1","file_size":150377,"mime_type":"video/mp4"}}}`))
	}))
	defer server.Close()

	client := NewHTTPClient("token", server.URL, http.DefaultClient)
	uploaded, err := client.Upload(context.Background(), UploadRequest{Type: TypeDocument, ChatID: "-100", Reader: strings.NewReader("fake video bytes"), Filename: "1000044311.mp4"})
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}
	if uploaded.FileID != "video-file-1" || uploaded.MessageID != 77 || uploaded.FileSize != 150377 {
		t.Fatalf("uploaded = %+v", uploaded)
	}
}

// TestClientUploadDocumentFailsWhenNoFieldPopulated confirms the fallback
// doesn't mask a genuine failure — if Telegram's response has no usable
// media field at all, Upload must still return an error.
func TestClientUploadDocumentFailsWhenNoFieldPopulated(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":77,"document":{}}}`))
	}))
	defer server.Close()

	client := NewHTTPClient("token", server.URL, http.DefaultClient)
	_, err := client.Upload(context.Background(), UploadRequest{Type: TypeDocument, ChatID: "-100", Reader: strings.NewReader("hello"), Filename: "hello.txt"})
	if err == nil {
		t.Fatal("expected an error when no media field is populated, got nil")
	}
}

func TestClientUploadPhotoUsesPhotoEndpointAndLargestPhoto(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":78,"photo":[{"file_id":"small","file_unique_id":"u-small","file_size":1},{"file_id":"large","file_unique_id":"u-large","file_size":5}]}}`))
	}))
	defer server.Close()

	client := NewHTTPClient("token", server.URL, http.DefaultClient)
	uploaded, err := client.Upload(context.Background(), UploadRequest{Type: TypePhoto, ChatID: "-100", Reader: strings.NewReader("hello"), Filename: "hello.jpg", MIMEType: "image/jpeg"})
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}
	if path != "/bottoken/sendPhoto" {
		t.Fatalf("path = %q", path)
	}
	if uploaded.FileID != "large" || uploaded.FileUniqueID != "u-large" {
		t.Fatalf("uploaded = %+v", uploaded)
	}
}

func TestClientDownloadStreamUsesGetFilePath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bottoken/getFile":
			mustDrainBody(t, w, r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_id":"file-1","file_path":"documents/file_1.txt","file_size":5}}`))
		case "/file/bottoken/documents/file_1.txt":
			_, _ = w.Write([]byte("hello"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewHTTPClient("token", server.URL, http.DefaultClient)
	stream, err := client.Download(context.Background(), "file-1")
	if err != nil {
		t.Fatalf("Download returned error: %v", err)
	}
	defer stream.Close()
	data, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll returned error: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("data = %q", string(data))
	}
}

func TestClientUploadStreamsMultipartBody(t *testing.T) {
	started := make(chan struct{})
	finish := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("copy request body: %v", err)
			http.Error(w, "copy request body", http.StatusInternalServerError)
			return
		}
		<-finish
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":77,"document":{"file_id":"file-1","file_unique_id":"unique-1","file_size":5}}}`))
	}))
	defer server.Close()

	reader := io.MultiReader(strings.NewReader("hello"), blockingReader{done: finish})
	client := NewHTTPClient("token", server.URL, http.DefaultClient)
	errCh := make(chan error, 1)
	go func() {
		_, err := client.Upload(context.Background(), UploadRequest{Type: TypeDocument, ChatID: "-100", Reader: reader, Filename: "hello.txt"})
		errCh <- err
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("server did not receive request before upload reader completed")
	}
	close(finish)
	if err := <-errCh; err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}
}

func TestClientUploadWithoutFilenameOmitsFilenameParameter(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = mustReadBodyString(t, w, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":77,"document":{"file_id":"file-1","file_unique_id":"unique-1","file_size":5}}}`))
	}))
	defer server.Close()

	client := NewHTTPClient("token", server.URL, http.DefaultClient)
	_, err := client.Upload(context.Background(), UploadRequest{Type: TypeDocument, ChatID: "-100", Reader: strings.NewReader("hello")})
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}
	if !strings.Contains(body, `name="document"`) {
		t.Fatalf("multipart body missing document field: %s", body)
	}
	if strings.Contains(body, `filename="file"`) {
		t.Fatalf("multipart body unexpectedly included default filename: %s", body)
	}
}

func TestBackoffDelayUsesRetryAfterWithoutGenericCap(t *testing.T) {
	delay := backoffDelay(context.Background(), 0, 0, time.Second)
	if delay != time.Second {
		t.Fatalf("backoffDelay(background, 0, 0, 1s) = %v, want 1s", delay)
	}
}

func TestBackoffDelayCapsRetryAfterByHTTPClientTimeout(t *testing.T) {
	delay := backoffDelay(context.Background(), 50*time.Millisecond, 0, time.Second)
	if delay != 50*time.Millisecond {
		t.Fatalf("backoffDelay(background, 50ms, 0, 1s) = %v, want 50ms", delay)
	}
}

func TestBackoffDelayCapsRetryAfterByContextDeadlineRemaining(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	delay := backoffDelay(ctx, time.Second, 0, time.Second)
	if delay <= 0 || delay > 50*time.Millisecond {
		t.Fatalf("backoffDelay(ctx, 1s, 0, 1s) = %v, want >0 and <=50ms", delay)
	}
}

func TestClientUploadRetriesRetryableStatusWithReadSeeker(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	bodies := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := mustReadBodyString(t, w, r)
		mu.Lock()
		attempts++
		bodies = append(bodies, body)
		currentAttempt := attempts
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if currentAttempt == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"ok":false,"description":"retry later"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":77,"document":{"file_id":"file-1","file_unique_id":"unique-1","file_size":5}}}`))
	}))
	defer server.Close()

	client := NewHTTPClient("token", server.URL, http.DefaultClient)
	uploaded, err := client.Upload(context.Background(), UploadRequest{Type: TypeDocument, ChatID: "-100", Reader: strings.NewReader("hello"), Filename: "hello.txt"})
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}
	if uploaded.FileID != "file-1" || uploaded.MessageID != 77 {
		t.Fatalf("uploaded = %+v", uploaded)
	}
	if attempts != 2 {
		t.Fatalf("Upload attempts = %d, want 2", attempts)
	}
	if len(bodies) != 2 {
		t.Fatalf("recorded %d bodies, want 2", len(bodies))
	}
	for i, body := range bodies {
		if !strings.Contains(body, "hello") {
			t.Fatalf("request body %d missing uploaded payload: %q", i+1, body)
		}
	}
}

func TestClientUploadRetryAfterWithReadSeekerHonorsContextDeadline(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		mustDrainBody(t, w, r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"description":"retry later","parameters":{"retry_after":1}}`))
	}))
	defer server.Close()

	client := NewHTTPClient("token", server.URL, http.DefaultClient)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := client.Upload(ctx, UploadRequest{Type: TypeDocument, ChatID: "-100", Reader: strings.NewReader("hello"), Filename: "hello.txt"})
	if err == nil {
		t.Fatal("Upload returned nil error")
	}
	if err != context.DeadlineExceeded {
		t.Fatalf("Upload error = %v, want %v", err, context.DeadlineExceeded)
	}
	if attempts != 1 {
		t.Fatalf("Upload attempts = %d, want 1", attempts)
	}
}

func TestClientUploadReturnsRateLimitErrorMetadataOnFinal429(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		mustDrainBody(t, w, r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"description":"retry later","parameters":{"retry_after":1}}`))
	}))
	defer server.Close()

	client := NewHTTPClient("token", server.URL, &http.Client{Timeout: 10 * time.Millisecond})
	_, err := client.Upload(context.Background(), UploadRequest{Type: TypeDocument, ChatID: "-100", Reader: strings.NewReader("hello"), Filename: "hello.txt"})
	if err == nil {
		t.Fatal("Upload returned nil error")
	}
	retryAfter, ok := IsRateLimitError(err)
	if !ok {
		t.Fatalf("IsRateLimitError(%v) = false, want true", err)
	}
	if retryAfter != time.Second {
		t.Fatalf("retryAfter = %v, want %v", retryAfter, time.Second)
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

func TestClientUploadNonSeekableReaderCannotSafelyRetry(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		mustDrainBody(t, w, r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ok":false,"description":"retry later"}`))
	}))
	defer server.Close()

	client := NewHTTPClient("token", server.URL, http.DefaultClient)
	_, err := client.Upload(context.Background(), UploadRequest{Type: TypeDocument, ChatID: "-100", Reader: nonSeekableReader{Reader: strings.NewReader("hello")}, Filename: "hello.txt"})
	if err == nil {
		t.Fatal("Upload returned nil error")
	}
	if !strings.Contains(err.Error(), "cannot safely retry") {
		t.Fatalf("Upload error = %v, want message containing %q", err, "cannot safely retry")
	}
	if attempts != 1 {
		t.Fatalf("Upload attempts = %d, want 1", attempts)
	}
}

func TestClientDownloadRetriesGetFileThenDownloadsStream(t *testing.T) {
	for _, tc := range []struct {
		name            string
		firstStatusCode int
		firstBody       string
	}{
		{name: "500", firstStatusCode: http.StatusInternalServerError, firstBody: `{"ok":false,"description":"retry later"}`},
		{name: "429", firstStatusCode: http.StatusTooManyRequests, firstBody: `{"ok":false,"description":"retry later"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getFileRequests := 0
			fileRequests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/bottoken/getFile":
					getFileRequests++
					mustDrainBody(t, w, r)
					w.Header().Set("Content-Type", "application/json")
					if getFileRequests == 1 {
						w.WriteHeader(tc.firstStatusCode)
						_, _ = w.Write([]byte(tc.firstBody))
						return
					}
					_, _ = w.Write([]byte(`{"ok":true,"result":{"file_id":"file-1","file_path":"documents/file_1.txt","file_size":5}}`))
				case "/file/bottoken/documents/file_1.txt":
					fileRequests++
					_, _ = w.Write([]byte("hello"))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			client := NewHTTPClient("token", server.URL, http.DefaultClient)
			stream, err := client.Download(context.Background(), "file-1")
			if err != nil {
				t.Fatalf("Download returned error: %v", err)
			}
			defer stream.Close()

			data, err := io.ReadAll(stream)
			if err != nil {
				t.Fatalf("ReadAll(stream) error = %v", err)
			}
			if string(data) != "hello" {
				t.Fatalf("stream body = %q, want %q", string(data), "hello")
			}
			if getFileRequests != 2 {
				t.Fatalf("getFile requests = %d, want 2", getFileRequests)
			}
			if fileRequests != 1 {
				t.Fatalf("file requests = %d, want 1", fileRequests)
			}
		})
	}
}

func TestClientDownloadRetriesFileGETThenReturnsStream(t *testing.T) {
	for _, tc := range []struct {
		name            string
		firstStatusCode int
		firstBody       string
	}{
		{name: "500", firstStatusCode: http.StatusInternalServerError, firstBody: `{"ok":false,"description":"retry later"}`},
		{name: "429", firstStatusCode: http.StatusTooManyRequests, firstBody: `{"ok":false,"description":"retry later"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getFileRequests := 0
			fileRequests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/bottoken/getFile":
					getFileRequests++
					mustDrainBody(t, w, r)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"ok":true,"result":{"file_id":"file-1","file_path":"documents/file_1.txt","file_size":5}}`))
				case "/file/bottoken/documents/file_1.txt":
					fileRequests++
					if fileRequests == 1 {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(tc.firstStatusCode)
						_, _ = w.Write([]byte(tc.firstBody))
						return
					}
					_, _ = w.Write([]byte("hello"))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			client := NewHTTPClient("token", server.URL, http.DefaultClient)
			stream, err := client.Download(context.Background(), "file-1")
			if err != nil {
				t.Fatalf("Download returned error: %v", err)
			}
			defer stream.Close()

			data, err := io.ReadAll(stream)
			if err != nil {
				t.Fatalf("ReadAll(stream) error = %v", err)
			}
			if string(data) != "hello" {
				t.Fatalf("stream body = %q, want %q", string(data), "hello")
			}
			if getFileRequests != 1 {
				t.Fatalf("getFile requests = %d, want 1", getFileRequests)
			}
			if fileRequests != 2 {
				t.Fatalf("file requests = %d, want 2", fileRequests)
			}
		})
	}
}

func TestClientDownloadHonorsRetryAfterUntilContextDeadline(t *testing.T) {
	fileRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bottoken/getFile":
			mustDrainBody(t, w, r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_id":"file-1","file_path":"documents/file_1.txt","file_size":5}}`))
		case "/file/bottoken/documents/file_1.txt":
			fileRequests++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"ok":false,"description":"retry","parameters":{"retry_after":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewHTTPClient("token", server.URL, http.DefaultClient)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	stream, err := client.Download(ctx, "file-1")
	if stream != nil {
		stream.Close()
		t.Fatal("Download returned unexpected stream")
	}
	if err == nil {
		t.Fatal("Download returned nil error")
	}
	if err != context.DeadlineExceeded {
		t.Fatalf("Download error = %v, want %v", err, context.DeadlineExceeded)
	}
	if fileRequests != 1 {
		t.Fatalf("Download file requests = %d, want 1", fileRequests)
	}
}

func TestClientGetFileHonorsRetryAfterUntilContextDeadline(t *testing.T) {
	getFileRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bottoken/getFile":
			getFileRequests++
			mustDrainBody(t, w, r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"ok":false,"description":"retry","parameters":{"retry_after":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewHTTPClient("token", server.URL, http.DefaultClient)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	stream, err := client.Download(ctx, "file-1")
	if stream != nil {
		stream.Close()
		t.Fatal("Download returned unexpected stream")
	}
	if err == nil {
		t.Fatal("Download returned nil error")
	}
	if err != context.DeadlineExceeded {
		t.Fatalf("Download error = %v, want %v", err, context.DeadlineExceeded)
	}
	if getFileRequests != 1 {
		t.Fatalf("Download getFile requests = %d, want 1", getFileRequests)
	}
}

func TestClientUploadTransportErrorRedactsToken(t *testing.T) {
	client := NewHTTPClient("token", "https://example.invalid", &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("Post %q: dial failure", r.URL.String())
	})})

	_, err := client.Upload(context.Background(), UploadRequest{Type: TypeDocument, ChatID: "-100", Reader: strings.NewReader("hello"), Filename: "hello.txt"})
	if err == nil {
		t.Fatal("Upload returned nil error")
	}
	if strings.Contains(err.Error(), "token") || strings.Contains(err.Error(), "/bottoken/") {
		t.Fatalf("Upload error leaked token: %v", err)
	}
}

func TestClientDownloadTransportErrorRedactsToken(t *testing.T) {
	client := NewHTTPClient("token", "https://example.invalid", &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("Get %q: dial failure", r.URL.String())
	})})

	_, err := client.Download(context.Background(), "file-1")
	if err == nil {
		t.Fatal("Download returned nil error")
	}
	if strings.Contains(err.Error(), "token") || strings.Contains(err.Error(), "/bottoken/") {
		t.Fatalf("Download error leaked token: %v", err)
	}
}

func TestClientUploadUnauthorizedReturnsClassifiedRequestError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mustDrainBody(t, w, r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Unauthorized"}`))
	}))
	defer server.Close()

	client := NewHTTPClient("token", server.URL, http.DefaultClient)
	_, err := client.Upload(context.Background(), UploadRequest{Type: TypeDocument, ChatID: "-100", Reader: strings.NewReader("hello"), Filename: "hello.txt"})
	if err == nil {
		t.Fatal("Upload returned nil error")
	}

	requestErr, ok := ClassifyRequestError(err)
	if !ok {
		t.Fatalf("ClassifyRequestError(%v) = false, want true", err)
	}
	if requestErr.Operation != "upload_send" {
		t.Fatalf("operation = %q, want %q", requestErr.Operation, "upload_send")
	}
	if requestErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", requestErr.StatusCode, http.StatusUnauthorized)
	}
	if requestErr.Reason != "unauthorized" {
		t.Fatalf("reason = %q, want %q", requestErr.Reason, "unauthorized")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func mustReadBodyString(t *testing.T, w http.ResponseWriter, r *http.Request) string {
	t.Helper()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read request body: %v", err)
		http.Error(w, "read request body", http.StatusInternalServerError)
		return ""
	}
	return string(data)
}

func mustDrainBody(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		t.Errorf("drain request body: %v", err)
		http.Error(w, "drain request body", http.StatusInternalServerError)
	}
}

type blockingReader struct{ done <-chan struct{} }

func (r blockingReader) Read(p []byte) (int, error) {
	<-r.done
	return 0, io.EOF
}

type nonSeekableReader struct{ io.Reader }
