package s3api

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aahl/tgnas/store"
)

func TestWriteErrorXML(t *testing.T) {
	recorder := httptest.NewRecorder()

	WriteError(recorder, S3Error{Code: "NoSuchKey", Message: "The specified key does not exist.", Status: http.StatusNotFound}, "/photos/missing.jpg", "req-123")

	response := recorder.Result()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusNotFound)
	}
	if got := response.Header.Get("Content-Type"); got != "application/xml" {
		t.Fatalf("content-type = %q, want %q", got, "application/xml")
	}

	var body ErrorResponse
	if err := xml.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode XML: %v", err)
	}
	if body.Code != "NoSuchKey" {
		t.Fatalf("code = %q, want %q", body.Code, "NoSuchKey")
	}
	if body.Message != "The specified key does not exist." {
		t.Fatalf("message = %q, want %q", body.Message, "The specified key does not exist.")
	}
	if body.Resource != "/photos/missing.jpg" {
		t.Fatalf("resource = %q, want %q", body.Resource, "/photos/missing.jpg")
	}
	if body.RequestID != "req-123" {
		t.Fatalf("request id = %q, want %q", body.RequestID, "req-123")
	}
}

func TestWriteErrorDefaultsZeroStatusToInternalError(t *testing.T) {
	recorder := httptest.NewRecorder()

	WriteError(recorder, S3Error{}, "/resource", "req-1")

	response := recorder.Result()
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusInternalServerError)
	}
	var body ErrorResponse
	if err := xml.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode XML: %v", err)
	}
	if body.Code != ErrInternalError.Code {
		t.Fatalf("code = %q, want %q", body.Code, ErrInternalError.Code)
	}
}

func TestMapStoreError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want S3Error
	}{
		{name: "not implemented", err: store.ErrNotImplemented, want: ErrNotImplemented},
		{name: "entity too large", err: store.ErrEntityTooLarge, want: ErrEntityTooLarge},
		{name: "no such bucket", err: store.ErrNoSuchBucket, want: ErrNoSuchBucket},
		{name: "no such key", err: store.ErrNoSuchKey, want: ErrNoSuchKey},
		{name: "invalid range", err: store.ErrInvalidRange, want: ErrInvalidRange},
		{name: "missing content length", err: store.ErrMissingContentLength, want: ErrMissingContentLength},
		{name: "invalid access key", err: ErrInvalidAccessKeyID, want: ErrInvalidAccessKey},
		{name: "signature does not match", err: ErrSignatureDoesNotMatch, want: ErrSignatureMismatch},
		{name: "invalid argument", err: errors.New("invalid argument"), want: ErrInvalidArgument},
		{name: "service unavailable", err: errors.New("service unavailable"), want: ErrServiceUnavailable},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MapError(tc.err)
			if got != tc.want {
				t.Fatalf("MapError(%v) = %+v, want %+v", tc.err, got, tc.want)
			}
		})
	}
}

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

func TestWriteErrorSetsContentEncodingIdentityAndContentLength(t *testing.T) {
	recorder := httptest.NewRecorder()

	WriteError(recorder, S3Error{Code: "SignatureDoesNotMatch", Message: "The request signature we calculated does not match the signature you provided.", Status: http.StatusForbidden}, "/bucket/key", "req-456")

	response := recorder.Result()
	if got := response.Header.Get("Content-Encoding"); got != "identity" {
		t.Fatalf("content-encoding = %q, want %q", got, "identity")
	}
	if got := response.Header.Get("Content-Length"); got == "" {
		t.Fatal("content-length header is missing")
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if got := response.Header.Get("Content-Length"); got != fmt.Sprintf("%d", len(body)) {
		t.Fatalf("content-length = %q, but body length = %d", got, len(body))
	}
}

func TestWriteErrorResponseSuppressesBodyForHead(t *testing.T) {
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/bucket/key", nil)

	WriteErrorResponse(recorder, req, ErrSignatureMismatch, "/bucket/key", "req-789")

	response := recorder.Result()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
	if got := response.Header.Get("Content-Encoding"); got != "identity" {
		t.Fatalf("content-encoding = %q, want %q", got, "identity")
	}
	body, _ := io.ReadAll(response.Body)
	if len(body) != 0 {
		t.Fatalf("HEAD response should have empty body, got %d bytes", len(body))
	}
}

func TestMapErrorFallsBackToInternalError(t *testing.T) {
	got := MapError(errors.New("boom"))
	if got != ErrInternalError {
		t.Fatalf("MapError fallback = %+v, want %+v", got, ErrInternalError)
	}
}
