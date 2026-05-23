package s3api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/aahl/tgs3/internal/testutil"
	"github.com/aahl/tgs3/metadata"
	"github.com/aahl/tgs3/store"
)

func TestRootNegotiationDefaultsToS3ListBuckets(t *testing.T) {
	server := newSignedTestServer(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	signRequest(t, request, "AKID", "SECRET")
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "ListAllMyBucketsResult") {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestRootNegotiationAllowsFutureHTML(t *testing.T) {
	server := newSignedTestServer(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept", "text/html")
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestRootAcceptApplicationXMLUsesS3ListBuckets(t *testing.T) {
	server := newSignedTestServer(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept", "application/xml")
	signRequest(t, request, "AKID", "SECRET")
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "ListAllMyBucketsResult") {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestRootAcceptHTMLSignedRequestStillUsesS3ListBuckets(t *testing.T) {
	server := newSignedTestServer(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept", "text/html")
	signRequest(t, request, "AKID", "SECRET")
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "ListAllMyBucketsResult") {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestRootAcceptHTMLWithS3QueryDoesNotUseHTMLShortcut(t *testing.T) {
	server := newSignedTestServer(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/?X-Amz-Algorithm=AWS4-HMAC-SHA256", nil)
	request.Header.Set("Accept", "text/html")
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "<Code>SignatureDoesNotMatch</Code>") {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestPutHeadGetDeleteObject(t *testing.T) {
	server := newSignedTestServer(t)
	put := signedRecorderRequest(t, http.MethodPut, "/photos/hello.txt", "hello", map[string]string{"Content-Type": "text/plain"})
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusOK {
		t.Fatalf("put status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}
	head := signedRecorderRequest(t, http.MethodHead, "/photos/hello.txt", "", nil)
	server.ServeHTTP(head.recorder, head.request)
	if head.recorder.Code != http.StatusOK || head.recorder.Header().Get("ETag") != `"5d41402abc4b2a76b9719d911017c592"` {
		t.Fatalf("head status = %d headers = %v", head.recorder.Code, head.recorder.Header())
	}
	get := signedRecorderRequest(t, http.MethodGet, "/photos/hello.txt", "", nil)
	server.ServeHTTP(get.recorder, get.request)
	if get.recorder.Code != http.StatusOK || get.recorder.Body.String() != "hello" {
		t.Fatalf("get status = %d body = %q", get.recorder.Code, get.recorder.Body.String())
	}
	deleteReq := signedRecorderRequest(t, http.MethodDelete, "/photos/hello.txt", "", nil)
	server.ServeHTTP(deleteReq.recorder, deleteReq.request)
	if deleteReq.recorder.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", deleteReq.recorder.Code)
	}
}

func TestGetObjectRange(t *testing.T) {
	server := newSignedTestServer(t)
	put := signedRecorderRequest(t, http.MethodPut, "/photos/letters.txt", "abcdefgh", nil)
	server.ServeHTTP(put.recorder, put.request)
	get := signedRecorderRequest(t, http.MethodGet, "/photos/letters.txt", "", nil)
	get.request.Header.Set("Range", "bytes=2-5")
	signRequest(t, get.request, "AKID", "SECRET")
	server.ServeHTTP(get.recorder, get.request)
	if get.recorder.Code != http.StatusPartialContent || get.recorder.Body.String() != "cdef" || get.recorder.Header().Get("Content-Range") != "bytes 2-5/8" {
		t.Fatalf("status = %d headers = %v body = %q", get.recorder.Code, get.recorder.Header(), get.recorder.Body.String())
	}
}

func TestGetObjectInvalidRange(t *testing.T) {
	server := newSignedTestServer(t)
	put := signedRecorderRequest(t, http.MethodPut, "/photos/letters.txt", "abcdefgh", nil)
	server.ServeHTTP(put.recorder, put.request)

	get := signedRecorderRequest(t, http.MethodGet, "/photos/letters.txt", "", nil)
	get.request.Header.Set("Range", "bytes=9-12")
	signRequest(t, get.request, "AKID", "SECRET")
	server.ServeHTTP(get.recorder, get.request)
	if get.recorder.Code != http.StatusRequestedRangeNotSatisfiable || !strings.Contains(get.recorder.Body.String(), "<Code>InvalidRange</Code>") {
		t.Fatalf("status = %d body = %s", get.recorder.Code, get.recorder.Body.String())
	}
}

func TestCreateBucketDisabled(t *testing.T) {
	server := newSignedTestServer(t)
	put := signedRecorderRequest(t, http.MethodPut, "/photos", "", nil)
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusNotImplemented || !strings.Contains(put.recorder.Body.String(), "<Code>NotImplemented</Code>") {
		t.Fatalf("status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}
}

func TestHeadBucket(t *testing.T) {
	server := newSignedTestServer(t)

	existing := signedRecorderRequest(t, http.MethodHead, "/photos", "", nil)
	server.ServeHTTP(existing.recorder, existing.request)
	if existing.recorder.Code != http.StatusOK {
		t.Fatalf("existing status = %d body = %s", existing.recorder.Code, existing.recorder.Body.String())
	}

	missing := signedRecorderRequest(t, http.MethodHead, "/missing", "", nil)
	server.ServeHTTP(missing.recorder, missing.request)
	if missing.recorder.Code != http.StatusNotFound || missing.recorder.Body.Len() != 0 {
		t.Fatalf("missing status = %d body = %q", missing.recorder.Code, missing.recorder.Body.String())
	}
}

func TestHeadObjectResponsesAreBodyFree(t *testing.T) {
	server := newSignedTestServer(t)

	put := signedRecorderRequest(t, http.MethodPut, "/photos/hello.txt", "hello", map[string]string{"Content-Type": "text/plain"})
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusOK {
		t.Fatalf("put status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}

	head := signedRecorderRequest(t, http.MethodHead, "/photos/hello.txt", "", nil)
	server.ServeHTTP(head.recorder, head.request)
	if head.recorder.Code != http.StatusOK || head.recorder.Body.Len() != 0 {
		t.Fatalf("head status = %d body = %q", head.recorder.Code, head.recorder.Body.String())
	}
	if head.recorder.Header().Get("Content-Length") != "5" || head.recorder.Header().Get("Content-Type") != "text/plain" {
		t.Fatalf("head headers = %v", head.recorder.Header())
	}

	missing := signedRecorderRequest(t, http.MethodHead, "/photos/missing.txt", "", nil)
	server.ServeHTTP(missing.recorder, missing.request)
	if missing.recorder.Code != http.StatusNotFound || missing.recorder.Body.Len() != 0 {
		t.Fatalf("missing status = %d body = %q", missing.recorder.Code, missing.recorder.Body.String())
	}
	if missing.recorder.Header().Get("Content-Type") != "application/xml" {
		t.Fatalf("missing headers = %v", missing.recorder.Header())
	}
}

func TestListObjectsV2WithContinuationToken(t *testing.T) {
	server := newSignedTestServer(t)
	for _, key := range []string{"a.txt", "b.txt", "c.txt"} {
		put := signedRecorderRequest(t, http.MethodPut, "/photos/"+key, key, nil)
		server.ServeHTTP(put.recorder, put.request)
		if put.recorder.Code != http.StatusOK {
			t.Fatalf("put %s status = %d body = %s", key, put.recorder.Code, put.recorder.Body.String())
		}
	}

	first := signedRecorderRequest(t, http.MethodGet, "/photos?list-type=2&max-keys=2", "", nil)
	server.ServeHTTP(first.recorder, first.request)
	body := first.recorder.Body.String()
	if first.recorder.Code != http.StatusOK || !strings.Contains(body, "<IsTruncated>true</IsTruncated>") {
		t.Fatalf("first status = %d body = %s", first.recorder.Code, body)
	}
	if !strings.Contains(body, "<Key>a.txt</Key>") || !strings.Contains(body, "<Key>b.txt</Key>") || strings.Contains(body, "<Key>c.txt</Key>") {
		t.Fatalf("first page body = %s", body)
	}
	token := extractBetween(body, "<NextContinuationToken>", "</NextContinuationToken>")
	if token == "" {
		t.Fatalf("missing continuation token in body = %s", body)
	}

	second := signedRecorderRequest(t, http.MethodGet, "/photos?list-type=2&continuation-token="+token, "", nil)
	server.ServeHTTP(second.recorder, second.request)
	body = second.recorder.Body.String()
	if second.recorder.Code != http.StatusOK {
		t.Fatalf("second status = %d body = %s", second.recorder.Code, body)
	}
	if strings.Contains(body, "<Key>a.txt</Key>") || strings.Contains(body, "<Key>b.txt</Key>") || !strings.Contains(body, "<Key>c.txt</Key>") {
		t.Fatalf("second page body = %s", body)
	}
}

func TestListObjectsV2MaxKeysZeroReturnsEmptyResult(t *testing.T) {
	server := newSignedTestServer(t)
	put := signedRecorderRequest(t, http.MethodPut, "/photos/a.txt", "a", nil)
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusOK {
		t.Fatalf("put status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}

	request := signedRecorderRequest(t, http.MethodGet, "/photos?list-type=2&max-keys=0", "", nil)
	server.ServeHTTP(request.recorder, request.request)
	body := request.recorder.Body.String()
	if request.recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", request.recorder.Code, body)
	}
	if strings.Contains(body, "<Contents>") || strings.Contains(body, "<NextContinuationToken>") || !strings.Contains(body, "<KeyCount>0</KeyCount>") || !strings.Contains(body, "<IsTruncated>false</IsTruncated>") {
		t.Fatalf("body = %s", body)
	}
}

func TestListObjectsV2DelimiterIncludesCommonPrefixesAndKeyCount(t *testing.T) {
	server := newSignedTestServer(t)
	for _, key := range []string{"folder/a.txt", "folder/b.txt", "nested/child.txt", "root.txt"} {
		put := signedRecorderRequest(t, http.MethodPut, "/photos/"+key, key, nil)
		server.ServeHTTP(put.recorder, put.request)
		if put.recorder.Code != http.StatusOK {
			t.Fatalf("put %s status = %d body = %s", key, put.recorder.Code, put.recorder.Body.String())
		}
	}

	request := signedRecorderRequest(t, http.MethodGet, "/photos?list-type=2&delimiter=/", "", nil)
	server.ServeHTTP(request.recorder, request.request)
	if request.recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", request.recorder.Code, request.recorder.Body.String())
	}

	var result ListBucketResult
	if err := xml.Unmarshal(request.recorder.Body.Bytes(), &result); err != nil {
		t.Fatalf("Unmarshal returned error: %v body = %s", err, request.recorder.Body.String())
	}
	if result.KeyCount != 3 {
		t.Fatalf("KeyCount = %d body = %s", result.KeyCount, request.recorder.Body.String())
	}
	if len(result.Contents) != 1 || result.Contents[0].Key != "root.txt" {
		t.Fatalf("contents = %+v", result.Contents)
	}
	if len(result.CommonPrefixes) != 2 || result.CommonPrefixes[0].Prefix != "folder/" || result.CommonPrefixes[1].Prefix != "nested/" {
		t.Fatalf("common prefixes = %+v", result.CommonPrefixes)
	}
}

func TestEscapedObjectKeyRoundTrip(t *testing.T) {
	server := newSignedTestServer(t)
	path := "/photos/a%2Fb%20c%2B.txt"

	put := signedRecorderRequest(t, http.MethodPut, path, "payload", map[string]string{"Content-Type": "text/plain"})
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusOK {
		t.Fatalf("put status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}

	get := signedRecorderRequest(t, http.MethodGet, path, "", nil)
	server.ServeHTTP(get.recorder, get.request)
	if get.recorder.Code != http.StatusOK || get.recorder.Body.String() != "payload" {
		t.Fatalf("get status = %d body = %q", get.recorder.Code, get.recorder.Body.String())
	}

	list := signedRecorderRequest(t, http.MethodGet, "/photos?list-type=2", "", nil)
	server.ServeHTTP(list.recorder, list.request)
	if !strings.Contains(list.recorder.Body.String(), "<Key>a/b c+.txt</Key>") {
		t.Fatalf("list body = %s", list.recorder.Body.String())
	}
}

func TestInvalidContinuationToken(t *testing.T) {
	server := newSignedTestServer(t)
	request := signedRecorderRequest(t, http.MethodGet, "/photos?list-type=2&continuation-token=not-base64!", "", nil)
	server.ServeHTTP(request.recorder, request.request)
	if request.recorder.Code != http.StatusBadRequest || !strings.Contains(request.recorder.Body.String(), "<Code>InvalidArgument</Code>") {
		t.Fatalf("status = %d body = %s", request.recorder.Code, request.recorder.Body.String())
	}
}

func TestAuthErrorsAreS3XML(t *testing.T) {
	server := newSignedTestServer(t)

	invalid := httptest.NewRequest(http.MethodGet, "/", nil)
	signRequest(t, invalid, "AKID", "WRONG")
	invalidRecorder := httptest.NewRecorder()
	server.ServeHTTP(invalidRecorder, invalid)
	if invalidRecorder.Code != http.StatusForbidden || !strings.Contains(invalidRecorder.Body.String(), "<Error>") {
		t.Fatalf("invalid status = %d body = %s", invalidRecorder.Code, invalidRecorder.Body.String())
	}

	missing := httptest.NewRequest(http.MethodGet, "/", nil)
	missing.Header.Set("Accept", "application/xml")
	missingRecorder := httptest.NewRecorder()
	server.ServeHTTP(missingRecorder, missing)
	if missingRecorder.Code != http.StatusForbidden || !strings.Contains(missingRecorder.Body.String(), "<Error>") {
		t.Fatalf("missing status = %d body = %s", missingRecorder.Code, missingRecorder.Body.String())
	}
}

func TestReadyzReturnsUnavailableWhenNotReady(t *testing.T) {
	ctx := context.Background()
	meta, err := metadata.OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: "photos", ChatID: "-100", CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
		t.Fatalf("UpsertBucket returned error: %v", err)
	}
	objectStore, err := store.NewObjectStore(meta, testutil.NewFakeTelegram(), store.Options{Upload: store.DefaultUploadConfig()})
	if err != nil {
		t.Fatalf("NewObjectStore returned error: %v", err)
	}
	server := NewServer(objectStore, Options{Region: "us-east-1", Credentials: map[string]string{"AKID": "SECRET"}, Ready: func() bool { return false }})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

type signedHTTPTest struct {
	recorder *httptest.ResponseRecorder
	request  *http.Request
}

func newSignedTestServer(t *testing.T) http.Handler {
	t.Helper()
	ctx := context.Background()
	meta, err := metadata.OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	for name, chatID := range map[string]string{"photos": "-100", "backups": "-200"} {
		if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: name, ChatID: chatID, CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
			t.Fatalf("UpsertBucket(%s) returned error: %v", name, err)
		}
	}
	fake := testutil.NewFakeTelegram()
	objectStore, err := store.NewObjectStore(meta, fake, store.Options{Upload: store.DefaultUploadConfig()})
	if err != nil {
		t.Fatalf("NewObjectStore returned error: %v", err)
	}
	return NewServer(objectStore, Options{Region: "us-east-1", Credentials: map[string]string{"AKID": "SECRET"}, SigV4Clock: func() time.Time { return time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC) }, Ready: func() bool { return true }})
}

func signedRecorderRequest(t *testing.T, method, path, body string, headers map[string]string) signedHTTPTest {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	sum := sha256.Sum256([]byte(body))
	request.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(sum[:]))
	signRequest(t, request, "AKID", "SECRET")
	return signedHTTPTest{recorder: httptest.NewRecorder(), request: request}
}

func signRequest(t *testing.T, request *http.Request, accessKey, secret string) {
	t.Helper()
	payloadHash := request.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		payloadHash = EmptyPayloadSHA256
		request.Header.Set("X-Amz-Content-Sha256", payloadHash)
	}
	request.Header.Del("Authorization")
	request.Header.Del("X-Amz-Date")
	credentials := aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secret}
	err := v4.NewSigner().SignHTTP(context.Background(), credentials, request, payloadHash, "s3", "us-east-1", time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatalf("SignHTTP returned error: %v", err)
	}
}

func extractBetween(value, start, end string) string {
	from := strings.Index(value, start)
	if from < 0 {
		return ""
	}
	from += len(start)
	to := strings.Index(value[from:], end)
	if to < 0 {
		return ""
	}
	return value[from : from+to]
}
