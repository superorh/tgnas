package s3api

import (
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aahl/tgs3/store"
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

func TestMapErrorFallsBackToInternalError(t *testing.T) {
	got := MapError(errors.New("boom"))
	if got != ErrInternalError {
		t.Fatalf("MapError fallback = %+v, want %+v", got, ErrInternalError)
	}
}
