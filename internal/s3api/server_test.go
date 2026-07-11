package s3api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/aahl/tgnas/internal/testutil"
	"github.com/aahl/tgnas/metadata"
	"github.com/aahl/tgnas/store"
)

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
	if !strings.Contains(complete.recorder.Body.String(), "<ETag>&#34;1c4bb33d6bb358e9305bd0e3f40b1552-2&#34;</ETag>") {
		t.Fatalf("complete body = %s", complete.recorder.Body.String())
	}

	get := signedRecorderRequest(t, http.MethodGet, "/photos/big.bin", "", nil)
	server.ServeHTTP(get.recorder, get.request)
	if get.recorder.Code != http.StatusOK || get.recorder.Body.String() != "abcdefghi" {
		t.Fatalf("get status = %d body = %q", get.recorder.Code, get.recorder.Body.String())
	}
	if get.recorder.Header().Get("ETag") != "\"1c4bb33d6bb358e9305bd0e3f40b1552-2\"" {
		t.Fatalf("get headers = %v", get.recorder.Header())
	}
}

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

func TestPresignedObjectGetAndHead(t *testing.T) {
	server := newSignedTestServer(t)

	put := signedRecorderRequest(t, http.MethodPut, "/photos/presigned.txt", "hello", map[string]string{"Content-Type": "text/plain"})
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusOK {
		t.Fatalf("put status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}

	getRecorder := httptest.NewRecorder()
	getRequest := presignServerRequest(t, http.MethodGet, "/photos/presigned.txt", 0)
	server.ServeHTTP(getRecorder, getRequest)
	if getRecorder.Code != http.StatusOK || getRecorder.Body.String() != "hello" {
		t.Fatalf("presigned get status = %d body = %q", getRecorder.Code, getRecorder.Body.String())
	}
	if getRecorder.Header().Get("Content-Type") != "text/plain" {
		t.Fatalf("presigned get headers = %v", getRecorder.Header())
	}

	headRecorder := httptest.NewRecorder()
	headRequest := presignServerRequest(t, http.MethodHead, "/photos/presigned.txt", 0)
	server.ServeHTTP(headRecorder, headRequest)
	if headRecorder.Code != http.StatusOK || headRecorder.Body.Len() != 0 {
		t.Fatalf("presigned head status = %d body = %q", headRecorder.Code, headRecorder.Body.String())
	}
	if headRecorder.Header().Get("Content-Length") != "5" {
		t.Fatalf("presigned head headers = %v", headRecorder.Header())
	}
}

func TestPresignedUnsupportedOperationsFail(t *testing.T) {
	server := newSignedTestServer(t)

	for _, tc := range []struct {
		method    string
		path      string
		bodyEmpty bool
	}{
		{method: http.MethodGet, path: "/"},
		{method: http.MethodGet, path: "/photos"},
		{method: http.MethodHead, path: "/photos", bodyEmpty: true},
		{method: http.MethodPut, path: "/photos/presigned.txt"},
		{method: http.MethodDelete, path: "/photos/presigned.txt"},
	} {
		recorder := httptest.NewRecorder()
		request := presignServerRequest(t, tc.method, tc.path, 0)
		server.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("presigned %s %s status = %d body = %s", tc.method, tc.path, recorder.Code, recorder.Body.String())
		}
		if tc.bodyEmpty {
			if recorder.Body.Len() != 0 {
				t.Fatalf("presigned %s %s body length = %d, want 0", tc.method, tc.path, recorder.Body.Len())
			}
			continue
		}
		if !strings.Contains(recorder.Body.String(), "<Code>SignatureDoesNotMatch</Code>") {
			t.Fatalf("presigned %s %s body = %s", tc.method, tc.path, recorder.Body.String())
		}
	}
}

func TestPublicReadAllowsAnonymousObjectGetAndHead(t *testing.T) {
	server := newPublicReadTestServer(t, map[string]bool{"photos": true})

	put := signedRecorderRequest(t, http.MethodPut, "/photos/public.txt", "hello", map[string]string{"Content-Type": "text/plain"})
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusOK {
		t.Fatalf("put status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}

	getRecorder := httptest.NewRecorder()
	getRequest := httptest.NewRequest(http.MethodGet, "/photos/public.txt", nil)
	server.ServeHTTP(getRecorder, getRequest)
	if getRecorder.Code != http.StatusOK || getRecorder.Body.String() != "hello" {
		t.Fatalf("anonymous get status = %d body = %q", getRecorder.Code, getRecorder.Body.String())
	}
	if getRecorder.Header().Get("Content-Type") != "text/plain" {
		t.Fatalf("anonymous get headers = %v", getRecorder.Header())
	}

	headRecorder := httptest.NewRecorder()
	headRequest := httptest.NewRequest(http.MethodHead, "/photos/public.txt", nil)
	server.ServeHTTP(headRecorder, headRequest)
	if headRecorder.Code != http.StatusOK || headRecorder.Body.Len() != 0 {
		t.Fatalf("anonymous head status = %d body = %q", headRecorder.Code, headRecorder.Body.String())
	}
	if headRecorder.Header().Get("Content-Length") != "5" || headRecorder.Header().Get("Content-Type") != "text/plain" {
		t.Fatalf("anonymous head headers = %v", headRecorder.Header())
	}
}

func TestPublicReadKeepsPrivateObjectsAuthenticated(t *testing.T) {
	server := newPublicReadTestServer(t, map[string]bool{"photos": true})

	put := signedRecorderRequest(t, http.MethodPut, "/backups/private.txt", "secret", nil)
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusOK {
		t.Fatalf("put status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/backups/private.txt", nil)
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "<Code>SignatureDoesNotMatch</Code>") {
		t.Fatalf("anonymous private get status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestPublicReadDoesNotExposeBucketListing(t *testing.T) {
	server := newPublicReadTestServer(t, map[string]bool{"photos": true})

	put := signedRecorderRequest(t, http.MethodPut, "/photos/public.txt", "hello", nil)
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusOK {
		t.Fatalf("put status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}

	for _, path := range []string{"/", "/photos", "/photos?list-type=2"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		server.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "<Code>SignatureDoesNotMatch</Code>") {
			t.Fatalf("anonymous list %s status = %d body = %s", path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestPublicReadDoesNotAllowAnonymousWrites(t *testing.T) {
	server := newPublicReadTestServer(t, map[string]bool{"photos": true})

	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodPut, path: "/photos/public.txt", body: "replace"},
		{method: http.MethodDelete, path: "/photos/public.txt"},
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		server.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "<Code>SignatureDoesNotMatch</Code>") {
			t.Fatalf("anonymous %s status = %d body = %s", tc.method, recorder.Code, recorder.Body.String())
		}
	}
}

func TestPublicReadDoesNotBypassPresignedQueryAuth(t *testing.T) {
	server := newPublicReadTestServer(t, map[string]bool{"photos": true})

	put := signedRecorderRequest(t, http.MethodPut, "/photos/public.txt", "hello", map[string]string{"Content-Type": "text/plain"})
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusOK {
		t.Fatalf("put status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}

	request := presignServerRequest(t, http.MethodGet, "/photos/public.txt", 15*time.Minute)
	query := request.URL.Query()
	query.Set("response-content-disposition", "tampered")
	request.URL.RawQuery = query.Encode()

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "<Code>SignatureDoesNotMatch</Code>") {
		t.Fatalf("tampered presigned get status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestPublicReadDoesNotBypassIncompletePresignedQueryAuth(t *testing.T) {
	server := newPublicReadTestServer(t, map[string]bool{"photos": true})

	put := signedRecorderRequest(t, http.MethodPut, "/photos/public.txt", "hello", map[string]string{"Content-Type": "text/plain"})
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusOK {
		t.Fatalf("put status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "/photos/public.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256", nil)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "<Code>SignatureDoesNotMatch</Code>") {
		t.Fatalf("incomplete presigned get status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestPublicReadDoesNotBypassSigV4HeaderAuth(t *testing.T) {
	server := newPublicReadTestServer(t, map[string]bool{"photos": true})

	put := signedRecorderRequest(t, http.MethodPut, "/photos/public.txt", "hello", map[string]string{"Content-Type": "text/plain"})
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusOK {
		t.Fatalf("put status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "/photos/public.txt", nil)
	request.Header.Set("X-Amz-Security-Token", "token")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "<Code>SignatureDoesNotMatch</Code>") {
		t.Fatalf("sigv4-shaped header get status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestPutObjectAcceptsUnsignedPayload(t *testing.T) {
	server := newSignedTestServer(t)
	put := signedUnsignedPayloadRecorderRequest(t, http.MethodPut, "/photos/unsigned.txt", "hello", map[string]string{"Content-Type": "text/plain"})
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusOK {
		t.Fatalf("put status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}

	get := signedRecorderRequest(t, http.MethodGet, "/photos/unsigned.txt", "", nil)
	server.ServeHTTP(get.recorder, get.request)
	if get.recorder.Code != http.StatusOK || get.recorder.Body.String() != "hello" {
		t.Fatalf("get status = %d body = %q", get.recorder.Code, get.recorder.Body.String())
	}
}

func TestDebugLogsQuoteRequestFieldsAndSanitizeErrors(t *testing.T) {
	var logs bytes.Buffer
	server := NewServer(errorPutObjectStore{err: errors.New("bot_token=123456:secret secret_key=plain")}, Options{
		Region:      "us-east-1",
		Credentials: map[string]string{"AKID": "SECRET"},
		Ready:       func() bool { return true },
		SigV4Clock:  func() time.Time { return time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC) },
		Logger:      log.New(&logs, "", 0),
	})

	put := signedUnsignedPayloadRecorderRequest(t, http.MethodPut, "/photos/unsafe.txt", "hello", map[string]string{"Content-Type": "text/plain"})
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusInternalServerError {
		t.Fatalf("put status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}

	output := logs.String()
	if strings.Contains(output, "123456:secret") || strings.Contains(output, "secret_key=plain") {
		t.Fatalf("debug log leaked secret: %q", output)
	}
	if !strings.Contains(output, `bucket="photos"`) || !strings.Contains(output, `key="unsafe.txt"`) || strings.Contains(output, "bucket=photos") || strings.Contains(output, "key=unsafe.txt") {
		t.Fatalf("debug log did not quote request fields: %q", output)
	}
	if !strings.Contains(output, `path="/photos/unsafe.txt"`) || !strings.Contains(output, `error="bot_token=[redacted] secret_key=[redacted]"`) {
		t.Fatalf("debug log missing quoted path or sanitized error: %q", output)
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

func TestCreateBucketForConfiguredBucketSucceeds(t *testing.T) {
	server := newSignedTestServer(t)
	put := signedRecorderRequest(t, http.MethodPut, "/photos", "", nil)
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusOK || put.recorder.Body.Len() != 0 {
		t.Fatalf("status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}
}

func TestCreateBucketForMissingBucketReturnsNotFound(t *testing.T) {
	server := newSignedTestServer(t)
	put := signedRecorderRequest(t, http.MethodPut, "/missing", "", nil)
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusNotFound || !strings.Contains(put.recorder.Body.String(), "<Code>NoSuchBucket</Code>") {
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

func TestDeleteBucketRemovesOrphanBucketMetadata(t *testing.T) {
	ctx := context.Background()
	meta, server := newBucketDeleteTestServer(t)
	if err := meta.PutObject(ctx, metadata.Object{Bucket: "archive", Key: "old.txt", Size: 3, LastModified: time.Now().UTC()}, nil); err != nil {
		t.Fatalf("PutObject returned error: %v", err)
	}

	deleteReq := signedRecorderRequest(t, http.MethodDelete, "/archive", "", nil)
	server.ServeHTTP(deleteReq.recorder, deleteReq.request)
	if deleteReq.recorder.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d body = %s", deleteReq.recorder.Code, deleteReq.recorder.Body.String())
	}
	if _, err := meta.GetBucket(ctx, "archive"); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatalf("GetBucket archive err = %v, want ErrNotFound", err)
	}
	objects, err := meta.ListObjects(ctx, metadata.ListQuery{Bucket: "archive", Limit: 10})
	if err != nil {
		t.Fatalf("ListObjects returned error: %v", err)
	}
	if len(objects) != 0 {
		t.Fatalf("archive objects = %d, want 0", len(objects))
	}
}

func TestDeleteBucketRejectsConfiguredBucket(t *testing.T) {
	ctx := context.Background()
	meta, server := newBucketDeleteTestServer(t)

	deleteReq := signedRecorderRequest(t, http.MethodDelete, "/photos", "", nil)
	server.ServeHTTP(deleteReq.recorder, deleteReq.request)
	if deleteReq.recorder.Code != http.StatusNotImplemented || !strings.Contains(deleteReq.recorder.Body.String(), "<Code>NotImplemented</Code>") {
		t.Fatalf("delete status = %d body = %s", deleteReq.recorder.Code, deleteReq.recorder.Body.String())
	}
	if bucket, err := meta.GetBucket(ctx, "photos"); err != nil || !bucket.Enabled {
		t.Fatalf("photos bucket = %+v err = %v", bucket, err)
	}
}

func newBucketDeleteTestServer(t *testing.T) (*metadata.SQLiteStore, http.Handler) {
	t.Helper()
	ctx := context.Background()
	meta, err := metadata.OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: "photos", ChatID: "-100", CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
		t.Fatalf("UpsertBucket photos returned error: %v", err)
	}
	if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: "archive", ChatID: "-200", CreatedAt: time.Now().UTC(), Enabled: false}); err != nil {
		t.Fatalf("UpsertBucket archive returned error: %v", err)
	}
	objectStore, err := store.NewObjectStore(meta, testutil.NewFakeTelegram(), store.Options{Upload: store.DefaultUploadConfig()})
	if err != nil {
		t.Fatalf("NewObjectStore returned error: %v", err)
	}
	server := NewServer(objectStore, Options{Region: "us-east-1", Credentials: map[string]string{"AKID": "SECRET"}, SigV4Clock: func() time.Time { return time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC) }, Ready: func() bool { return true }})
	return meta, server
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

func TestAuthFailureLogsRequestContext(t *testing.T) {
	var logs strings.Builder
	server := NewServer(errorPutObjectStore{}, Options{
		Region:      "us-east-1",
		Credentials: map[string]string{"AKID": "SECRET"},
		SigV4Clock:  func() time.Time { return time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC) },
		Ready:       func() bool { return true },
		Logger:      log.New(&logs, "", 0),
	})

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9000/", nil)
	signRequest(t, request, "AKID", "WRONG")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}

	got := logs.String()
	for _, want := range []string{
		`event=s3_auth_failure`,
		`method="GET"`,
		`path="/"`,
		`host="127.0.0.1:9000"`,
		`scheme="http"`,
		`authorization=true`,
		`sigv4_query=false`,
		`error="signature does not match"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("log %q does not contain %s", got, want)
		}
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

type errorPutObjectStore struct {
	err error
}

func (s errorPutObjectStore) ListBuckets(context.Context) ([]metadata.Bucket, error) {
	return []metadata.Bucket{{Name: "photos", Enabled: true}}, nil
}

func (s errorPutObjectStore) HeadBucket(context.Context, string) error {
	return nil
}

func (s errorPutObjectStore) DeleteBucket(context.Context, string) error {
	return nil
}

func (s errorPutObjectStore) PutObject(context.Context, store.PutObjectInput) (store.PutObjectResult, error) {
	return store.PutObjectResult{}, s.err
}

func (s errorPutObjectStore) GetObject(context.Context, store.GetObjectInput) (io.ReadCloser, store.ObjectInfo, error) {
	return nil, store.ObjectInfo{}, store.ErrNoSuchKey
}

func (s errorPutObjectStore) HeadObject(context.Context, string, string) (store.ObjectInfo, error) {
	return store.ObjectInfo{}, store.ErrNoSuchKey
}

func (s errorPutObjectStore) ListObjects(context.Context, store.ListObjectsInput) (store.ListObjectsResult, error) {
	return store.ListObjectsResult{}, nil
}

func (s errorPutObjectStore) DeleteObject(context.Context, string, string) error {
	return nil
}

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

func newPublicReadTestServer(t *testing.T, publicReadBuckets map[string]bool) http.Handler {
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
	return NewServer(objectStore, Options{
		Region:            "us-east-1",
		Credentials:       map[string]string{"AKID": "SECRET"},
		PublicReadBuckets: publicReadBuckets,
		SigV4Clock:        func() time.Time { return time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC) },
		Ready:             func() bool { return true },
	})
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

func signedUnsignedPayloadRecorderRequest(t *testing.T, method, path, body string, headers map[string]string) signedHTTPTest {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	request.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	signRequest(t, request, "AKID", "SECRET")
	return signedHTTPTest{recorder: httptest.NewRecorder(), request: request}
}

func presignServerRequest(t *testing.T, method, target string, expires time.Duration) *http.Request {
	t.Helper()
	if expires == 0 {
		expires = 15 * time.Minute
	}
	request := httptest.NewRequest(method, "https://example.com"+target, nil)
	request.Host = "example.com"
	query := request.URL.Query()
	query.Set("X-Amz-Expires", strconv.FormatInt(int64(expires/time.Second), 10))
	request.URL.RawQuery = query.Encode()

	credentials := aws.Credentials{AccessKeyID: "AKID", SecretAccessKey: "SECRET"}
	signedURL, _, err := v4.NewSigner().PresignHTTP(context.Background(), credentials, request, "UNSIGNED-PAYLOAD", "s3", "us-east-1", time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), func(options *v4.SignerOptions) {
		options.DisableURIPathEscaping = true
	})
	if err != nil {
		t.Fatalf("PresignHTTP returned error: %v", err)
	}

	presigned := httptest.NewRequest(method, signedURL, nil)
	presigned.Host = "example.com"
	return presigned
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

func mustCompileTestCORSPolicy(t *testing.T, global []string, buckets map[string][]string) *CORSPolicy {
	t.Helper()
	policy, err := CompileCORSPolicy(global, buckets)
	if err != nil {
		t.Fatalf("CompileCORSPolicy returned error: %v", err)
	}
	return policy
}

func newCORSTestServer(t *testing.T, policy *CORSPolicy, options func(*Options)) http.Handler {
	t.Helper()
	base := Options{
		Region:      "us-east-1",
		Credentials: map[string]string{"AKID": "SECRET"},
		Ready:       func() bool { return true },
		CORS:        policy,
	}
	if options != nil {
		options(&base)
	}
	return NewServer(errorPutObjectStore{}, base)
}

func newPreflightRequest(path, origin, method, headers string) *http.Request {
	req := httptest.NewRequest(http.MethodOptions, path, nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if method != "" {
		req.Header.Set("Access-Control-Request-Method", method)
	}
	if headers != "" {
		req.Header.Set("Access-Control-Request-Headers", headers)
	}
	return req
}

func varyTokenCount(header http.Header, name string) int {
	count := 0
	for _, value := range header.Values("Vary") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), name) {
				count++
			}
		}
	}
	return count
}

func assertNoCORSNegotiationHeaders(t *testing.T, header http.Header) {
	t.Helper()
	for _, name := range []string{
		"Access-Control-Allow-Origin",
		"Access-Control-Allow-Methods",
		"Access-Control-Allow-Headers",
		"Access-Control-Max-Age",
		"Access-Control-Expose-Headers",
		"Access-Control-Allow-Credentials",
	} {
		if got := header.Get(name); got != "" {
			t.Fatalf("%s = %q, want absent", name, got)
		}
	}
}

func TestCORSActualResponses(t *testing.T) {
	policy := mustCompileTestCORSPolicy(t, []string{"https://frontend.example"}, nil)
	server := newCORSTestServer(t, policy, nil)

	tests := []struct {
		name      string
		origin    string
		wantAllow bool
		wantVary  bool
	}{
		{name: "allowed", origin: "https://frontend.example", wantAllow: true, wantVary: true},
		{name: "denied", origin: "https://evil.example", wantVary: true},
		{name: "missing origin", wantVary: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/photos/object", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, req)

			if tc.wantVary && varyTokenCount(rec.Header(), "Origin") != 1 {
				t.Fatalf("Vary = %#v, want Origin exactly once", rec.Header().Values("Vary"))
			}
			if tc.wantAllow {
				if got := rec.Header().Get("Access-Control-Allow-Origin"); got != tc.origin {
					t.Fatalf("allow origin = %q, want %q", got, tc.origin)
				}
				if got := rec.Header().Get("Access-Control-Expose-Headers"); got != "ETag, Content-Range, Content-Length, Accept-Ranges, Last-Modified" {
					t.Fatalf("expose headers = %q", got)
				}
			} else if rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Header().Get("Access-Control-Expose-Headers") != "" {
				t.Fatalf("unexpected CORS headers: %#v", rec.Header())
			}
			if rec.Header().Get("Access-Control-Allow-Credentials") != "" {
				t.Fatal("Access-Control-Allow-Credentials must not be emitted")
			}
		})
	}
}

func TestCORSDisabledPreservesExistingBehavior(t *testing.T) {
	for _, policy := range []*CORSPolicy{nil, mustCompileTestCORSPolicy(t, nil, nil)} {
		server := newCORSTestServer(t, policy, nil)
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, newPreflightRequest("/photos/object", "https://frontend.example", "GET", ""))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want existing SigV4 rejection %d", rec.Code, http.StatusForbidden)
		}
		assertNoCORSNegotiationHeaders(t, rec.Header())
		if varyTokenCount(rec.Header(), "Origin") != 0 {
			t.Fatalf("Vary = %#v, want no CORS dimension", rec.Header().Values("Vary"))
		}
	}
}

func TestCORSPreflightNegotiation(t *testing.T) {
	policy := mustCompileTestCORSPolicy(t, []string{"https://frontend.example"}, nil)
	server := newCORSTestServer(t, policy, nil)

	tests := []struct {
		name    string
		origin  string
		method  string
		headers string
		want    int
	}{
		{name: "allowed", origin: "https://frontend.example", method: "put", headers: "authorization, Content-Type, range, X-Amz-Date, X-Amz-Content-Sha256", want: http.StatusNoContent},
		{name: "denied origin", origin: "https://evil.example", method: "GET", want: http.StatusForbidden},
		{name: "denied method", origin: "https://frontend.example", method: "PATCH", want: http.StatusForbidden},
		{name: "denied header", origin: "https://frontend.example", method: "GET", headers: "X-Custom", want: http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, newPreflightRequest("/photos/object", tc.origin, tc.method, tc.headers))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			for _, dimension := range []string{"Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers"} {
				if varyTokenCount(rec.Header(), dimension) != 1 {
					t.Fatalf("Vary = %#v, want %s exactly once", rec.Header().Values("Vary"), dimension)
				}
			}
			if tc.want == http.StatusNoContent {
				if rec.Header().Get("Access-Control-Allow-Origin") != tc.origin ||
					rec.Header().Get("Access-Control-Allow-Methods") != "GET, HEAD, PUT, POST, DELETE, OPTIONS" ||
					rec.Header().Get("Access-Control-Allow-Headers") != "Authorization, Content-Type, Range, X-Amz-Date, X-Amz-Content-Sha256" ||
					rec.Header().Get("Access-Control-Max-Age") != "3600" {
					t.Fatalf("unexpected preflight headers: %#v", rec.Header())
				}
				if rec.Header().Get("Access-Control-Expose-Headers") != "" {
					t.Fatal("preflight must not expose actual-response headers")
				}
				if rec.Header().Get("Access-Control-Allow-Credentials") != "" {
					t.Fatal("preflight must not allow credentials")
				}
			} else {
				assertNoCORSNegotiationHeaders(t, rec.Header())
			}
		})
	}
}

func TestCORSOptionsMissingPreflightFieldsContinuesThroughSigV4(t *testing.T) {
	policy := mustCompileTestCORSPolicy(t, []string{"https://frontend.example"}, nil)
	server := newCORSTestServer(t, policy, nil)

	tests := []struct {
		name            string
		req             *http.Request
		wantStatus      int
		wantOriginVary  int
		wantAllowOrigin string
		wantExpose      bool
	}{
		{
			name:            "missing request method keeps actual decoration and fails SigV4",
			req:             newPreflightRequest("/photos/object", "https://frontend.example", "", ""),
			wantStatus:      http.StatusForbidden,
			wantOriginVary:  1,
			wantAllowOrigin: "https://frontend.example",
			wantExpose:      true,
		},
		{
			name:           "missing origin continues through SigV4 with Origin Vary",
			req:            newPreflightRequest("/photos/object", "", "GET", ""),
			wantStatus:     http.StatusForbidden,
			wantOriginVary: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, tc.req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if varyTokenCount(rec.Header(), "Origin") != tc.wantOriginVary {
				t.Fatalf("Vary Origin count = %d, want %d; values = %#v", varyTokenCount(rec.Header(), "Origin"), tc.wantOriginVary, rec.Header().Values("Vary"))
			}
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != tc.wantAllowOrigin {
				t.Fatalf("Access-Control-Allow-Origin = %q, want %q", got, tc.wantAllowOrigin)
			}
			if got := rec.Header().Get("Access-Control-Expose-Headers"); (got != "") != tc.wantExpose {
				t.Fatalf("Access-Control-Expose-Headers = %q, want present %v", got, tc.wantExpose)
			}
			if tc.wantExpose && rec.Header().Get("Access-Control-Expose-Headers") != corsExposeHeaders {
				t.Fatalf("Access-Control-Expose-Headers = %q, want %q", rec.Header().Get("Access-Control-Expose-Headers"), corsExposeHeaders)
			}
			if varyTokenCount(rec.Header(), "Access-Control-Request-Method") != 0 || varyTokenCount(rec.Header(), "Access-Control-Request-Headers") != 0 {
				t.Fatalf("unrecognized preflight got negotiation Vary: %#v", rec.Header().Values("Vary"))
			}
		})
	}
}

func TestCORSBucketOverrideAndFallbackHTTP(t *testing.T) {
	policy := mustCompileTestCORSPolicy(t,
		[]string{"https://global.example"},
		map[string][]string{
			"override":        {"https://bucket.example"},
			"fallback-empty":  {},
			"fallback-blanks": {"", ""},
		},
	)
	server := newCORSTestServer(t, policy, nil)

	tests := []struct {
		path   string
		origin string
		want   int
	}{
		{path: "/override/key", origin: "https://bucket.example", want: http.StatusNoContent},
		{path: "/override/key", origin: "https://global.example", want: http.StatusForbidden},
		{path: "/fallback-empty/key", origin: "https://global.example", want: http.StatusNoContent},
		{path: "/fallback-blanks/key", origin: "https://global.example", want: http.StatusNoContent},
		{path: "/absent/key", origin: "https://global.example", want: http.StatusNoContent},
		{path: "/", origin: "https://global.example", want: http.StatusNoContent},
	}
	for _, tc := range tests {
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, newPreflightRequest(tc.path, tc.origin, "GET", ""))
		if rec.Code != tc.want {
			t.Fatalf("%s from %s: status = %d, want %d", tc.path, tc.origin, rec.Code, tc.want)
		}
	}
}

func TestCORSExcludesHealthAndReadiness(t *testing.T) {
	policy := mustCompileTestCORSPolicy(t, []string{"https://frontend.example"}, nil)
	tests := []struct {
		name     string
		ready    bool
		path     string
		wantCode int
		wantBody string
	}{
		{name: "health ready", ready: true, path: "/healthz", wantCode: http.StatusOK, wantBody: "ok"},
		{name: "health not ready", ready: false, path: "/healthz", wantCode: http.StatusOK, wantBody: "ok"},
		{name: "ready", ready: true, path: "/readyz", wantCode: http.StatusOK, wantBody: "ready"},
		{name: "not ready", ready: false, path: "/readyz", wantCode: http.StatusServiceUnavailable, wantBody: "not ready"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := newCORSTestServer(t, policy, func(options *Options) {
				options.Ready = func() bool { return tc.ready }
			})

			get := httptest.NewRequest(http.MethodGet, tc.path, nil)
			get.Header.Set("Origin", "https://frontend.example")
			getRec := httptest.NewRecorder()
			server.ServeHTTP(getRec, get)
			if getRec.Code != tc.wantCode || getRec.Body.String() != tc.wantBody {
				t.Fatalf("GET %s status = %d body = %q, want %d %q", tc.path, getRec.Code, getRec.Body.String(), tc.wantCode, tc.wantBody)
			}
			assertNoCORSNegotiationHeaders(t, getRec.Header())
			for _, dimension := range []string{"Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers"} {
				if varyTokenCount(getRec.Header(), dimension) != 0 {
					t.Fatalf("GET %s Vary = %#v, want no CORS dimensions", tc.path, getRec.Header().Values("Vary"))
				}
			}

			preflightRec := httptest.NewRecorder()
			server.ServeHTTP(preflightRec, newPreflightRequest(tc.path, "https://frontend.example", "GET", ""))
			if preflightRec.Code != http.StatusNotFound {
				t.Fatalf("OPTIONS %s status = %d, want 404", tc.path, preflightRec.Code)
			}
			assertNoCORSNegotiationHeaders(t, preflightRec.Header())
			for _, dimension := range []string{"Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers"} {
				if varyTokenCount(preflightRec.Header(), dimension) != 0 {
					t.Fatalf("OPTIONS %s Vary = %#v, want no CORS dimensions", tc.path, preflightRec.Header().Values("Vary"))
				}
			}
		})
	}
}

func TestCORSVaryPreservesExistingValues(t *testing.T) {
	policy := mustCompileTestCORSPolicy(t, []string{"https://frontend.example"}, nil)
	inner := newCORSTestServer(t, policy, nil)
	server := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding, origin")
		w.Header().Add("Vary", "User-Agent")
		inner.ServeHTTP(w, r)
	})

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/photos/key", nil),
		newPreflightRequest("/photos/key", "https://frontend.example", "GET", "Authorization"),
	} {
		req.Header.Set("Origin", "https://frontend.example")
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if varyTokenCount(rec.Header(), "Accept-Encoding") != 1 || varyTokenCount(rec.Header(), "User-Agent") != 1 || varyTokenCount(rec.Header(), "Origin") != 1 {
			t.Fatalf("Vary values not preserved/deduplicated: %#v", rec.Header().Values("Vary"))
		}
		if req.Method == http.MethodOptions {
			if varyTokenCount(rec.Header(), "Access-Control-Request-Method") != 1 || varyTokenCount(rec.Header(), "Access-Control-Request-Headers") != 1 {
				t.Fatalf("preflight Vary dimensions = %#v, want each negotiation dimension once", rec.Header().Values("Vary"))
			}
		}
	}
}

func newPublicReadCORSTestServer(t *testing.T, publicReadBuckets map[string]bool, policy *CORSPolicy) http.Handler {
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
	objectStore, err := store.NewObjectStore(meta, testutil.NewFakeTelegram(), store.Options{Upload: store.DefaultUploadConfig()})
	if err != nil {
		t.Fatalf("NewObjectStore returned error: %v", err)
	}
	return NewServer(objectStore, Options{
		Region:            "us-east-1",
		Credentials:       map[string]string{"AKID": "SECRET"},
		PublicReadBuckets: publicReadBuckets,
		SigV4Clock:        func() time.Time { return time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC) },
		Ready:             func() bool { return true },
		CORS:              policy,
	})
}

func TestCORSPublicReadRetainsAnonymousBehavior(t *testing.T) {
	policy := mustCompileTestCORSPolicy(t, []string{"https://frontend.example"}, nil)
	server := newPublicReadCORSTestServer(t, map[string]bool{"photos": true}, policy)

	put := signedRecorderRequest(t, http.MethodPut, "/photos/public.txt", "hello", map[string]string{"Content-Type": "text/plain"})
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusOK {
		t.Fatalf("put status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/photos/public.txt", nil)
	req.Header.Set("Origin", "https://frontend.example")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "hello" {
		t.Fatalf("anonymous get status = %d body = %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://frontend.example" ||
		rec.Header().Get("Access-Control-Expose-Headers") != corsExposeHeaders ||
		varyTokenCount(rec.Header(), "Origin") != 1 {
		t.Fatalf("anonymous CORS headers = %#v", rec.Header())
	}
}

func TestCORSBusinessErrorRetainsActualResponseHeaders(t *testing.T) {
	policy := mustCompileTestCORSPolicy(t, []string{"https://frontend.example"}, nil)
	server := NewServer(errorPutObjectStore{err: errors.New("upload failed")}, Options{
		Region:      "us-east-1",
		Credentials: map[string]string{"AKID": "SECRET"},
		Ready:       func() bool { return true },
		SigV4Clock:  func() time.Time { return time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC) },
		CORS:        policy,
	})
	put := signedUnsignedPayloadRecorderRequest(t, http.MethodPut, "/photos/fail.txt", "hello", map[string]string{
		"Content-Type": "text/plain",
		"Origin":       "https://frontend.example",
	})
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusInternalServerError {
		t.Fatalf("put status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}
	if put.recorder.Header().Get("Access-Control-Allow-Origin") != "https://frontend.example" ||
		put.recorder.Header().Get("Access-Control-Expose-Headers") != corsExposeHeaders ||
		varyTokenCount(put.recorder.Header(), "Origin") != 1 {
		t.Fatalf("business-error CORS headers = %#v", put.recorder.Header())
	}
}
