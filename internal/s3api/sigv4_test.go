package s3api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

func TestVerifySigV4AWSKnownExample(t *testing.T) {
	request, _ := http.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	request.Host = "examplebucket.s3.amazonaws.com"
	request.Header.Set("Range", "bytes=0-9")
	request.Header.Set("X-Amz-Date", "20130524T000000Z")
	request.Header.Set("X-Amz-Content-Sha256", EmptyPayloadSHA256)
	request.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=67fe34c8530db585abddc51067328adfedb6e42487d2566dc7d927d6e2722900")

	verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKIDEXAMPLE": "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}, WithSigV4Clock(func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) }))
	identity, err := verifier.Verify(request)
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	if identity.AccessKey != "AKIDEXAMPLE" {
		t.Fatalf("identity = %+v", identity)
	}

	request.Header.Set("Range", "bytes=10-19")
	_, err = verifier.Verify(request)
	if err != ErrSignatureDoesNotMatch {
		t.Fatalf("err = %v, want ErrSignatureDoesNotMatch", err)
	}
}

func TestVerifySigV4RejectsUnknownAccessKey(t *testing.T) {
	request := signedTestRequest(t, signedRequestOptions{accessKey: "UNKNOWN", secret: "SECRET", region: "us-east-1", service: "s3"})
	verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time { return time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC) }))
	_, err := verifier.Verify(request)
	if err != ErrInvalidAccessKeyID {
		t.Fatalf("err = %v, want ErrInvalidAccessKeyID", err)
	}
}

func TestVerifySigV4RejectsSignatureMismatch(t *testing.T) {
	request := signedTestRequest(t, signedRequestOptions{accessKey: "AKID", secret: "WRONG", region: "us-east-1", service: "s3"})
	verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time { return time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC) }))
	_, err := verifier.Verify(request)
	if err != ErrSignatureDoesNotMatch {
		t.Fatalf("err = %v, want ErrSignatureDoesNotMatch", err)
	}
}

func TestVerifySigV4RejectsWrongRegionOrService(t *testing.T) {
	for _, request := range []*http.Request{
		signedTestRequest(t, signedRequestOptions{accessKey: "AKID", secret: "SECRET", region: "eu-west-1", service: "s3"}),
		signedTestRequest(t, signedRequestOptions{accessKey: "AKID", secret: "SECRET", region: "us-east-1", service: "ec2"}),
	} {
		verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time { return time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC) }))
		_, err := verifier.Verify(request)
		if err != ErrSignatureDoesNotMatch {
			t.Fatalf("err = %v, want ErrSignatureDoesNotMatch", err)
		}
	}
}

func TestVerifySigV4SignedByAWSSDK(t *testing.T) {
	request := signedTestRequest(t, signedRequestOptions{accessKey: "AKID", secret: "SECRET", region: "us-east-1", service: "s3"})
	verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time { return time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC) }))
	identity, err := verifier.Verify(request)
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	if identity.AccessKey != "AKID" {
		t.Fatalf("identity = %+v", identity)
	}
}

func TestVerifySigV4RejectsStaleRequestTime(t *testing.T) {
	request := signedTestRequest(t, signedRequestOptions{accessKey: "AKID", secret: "SECRET", region: "us-east-1", service: "s3"})
	verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time { return time.Date(2024, 1, 2, 3, 20, 0, 0, time.UTC) }))
	_, err := verifier.Verify(request)
	if err != ErrSignatureDoesNotMatch {
		t.Fatalf("err = %v, want ErrSignatureDoesNotMatch", err)
	}
}

func TestVerifySigV4RejectsCredentialScopeDateMismatch(t *testing.T) {
	request := signedTestRequest(t, signedRequestOptions{accessKey: "AKID", secret: "SECRET", region: "us-east-1", service: "s3"})
	request.Header.Set("Authorization", strings.Replace(request.Header.Get("Authorization"), "/20240102/", "/20240101/", 1))
	verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time { return time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC) }))
	_, err := verifier.Verify(request)
	if err != ErrSignatureDoesNotMatch {
		t.Fatalf("err = %v, want ErrSignatureDoesNotMatch", err)
	}
}

func TestVerifySigV4RequestTimeSkewBoundaries(t *testing.T) {
	base := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	testCases := []struct {
		name    string
		now     time.Time
		wantErr bool
	}{
		{name: "exact past boundary", now: base.Add(15 * time.Minute)},
		{name: "past outside boundary", now: base.Add(15*time.Minute + time.Second), wantErr: true},
		{name: "exact future boundary", now: base.Add(-15 * time.Minute)},
		{name: "future outside boundary", now: base.Add(-15*time.Minute - time.Second), wantErr: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			request := signedTestRequest(t, signedRequestOptions{accessKey: "AKID", secret: "SECRET", region: "us-east-1", service: "s3"})
			verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time { return tc.now }))
			_, err := verifier.Verify(request)
			if tc.wantErr && err != ErrSignatureDoesNotMatch {
				t.Fatalf("err = %v, want ErrSignatureDoesNotMatch", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Verify returned error: %v", err)
			}
		})
	}
}

func TestCanonicalQueryStringMatchesAWSSigner(t *testing.T) {
	testCases := []struct {
		name     string
		rawQuery string
	}{
		{name: "repeated keys and empty values", rawQuery: "dup=b&dup=&dup=a&empty&empty=&z=last"},
		{name: "plus and percent twenty", rawQuery: "plus=a+b&space=a%20b&mix=%20+"},
		{name: "pre-escaped bytes", rawQuery: "slash=%2F&literal=%252F&hex=%7e&utf8=%E2%82%AC"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			request := signedTestRequest(t, signedRequestOptions{
				accessKey: "AKID",
				secret:    "SECRET",
				region:    "us-east-1",
				service:   "s3",
				rawQuery:  tc.rawQuery,
			})
			verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time { return time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC) }))
			identity, err := verifier.Verify(request)
			if err != nil {
				t.Fatalf("Verify returned error: %v", err)
			}
			if identity.AccessKey != "AKID" {
				t.Fatalf("identity = %+v", identity)
			}
		})
	}
}

func TestVerifySigV4ValidatesSignedBody(t *testing.T) {
	request := signedTestRequest(t, signedRequestOptions{
		accessKey: "AKID",
		secret:    "SECRET",
		region:    "us-east-1",
		service:   "s3",
		method:    http.MethodPut,
		body:      []byte("signed payload"),
	})
	verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time { return time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC) }))

	identity, err := verifier.Verify(request)
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	if identity.AccessKey != "AKID" {
		t.Fatalf("identity = %+v", identity)
	}

	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatalf("ReadAll returned error: %v", err)
	}
	if string(body) != "signed payload" {
		t.Fatalf("body = %q, want %q", string(body), "signed payload")
	}
}

func TestVerifySigV4RejectsModifiedBodyAfterSigning(t *testing.T) {
	request := signedTestRequest(t, signedRequestOptions{
		accessKey: "AKID",
		secret:    "SECRET",
		region:    "us-east-1",
		service:   "s3",
		method:    http.MethodPut,
		body:      []byte("signed payload"),
	})
	request.Body = io.NopCloser(bytes.NewReader([]byte("tampered payload")))
	request.ContentLength = int64(len("tampered payload"))

	verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time { return time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC) }))
	_, err := verifier.Verify(request)
	if err != ErrSignatureDoesNotMatch {
		t.Fatalf("err = %v, want ErrSignatureDoesNotMatch", err)
	}
}

type signedRequestOptions struct {
	accessKey string
	secret    string
	region    string
	service   string
	method    string
	rawQuery  string
	body      []byte
}

func signedTestRequest(t *testing.T, opts signedRequestOptions) *http.Request {
	t.Helper()

	method := opts.method
	if method == "" {
		method = http.MethodGet
	}

	url := "https://example.com/photos/hello.txt"
	if opts.rawQuery != "" {
		url += "?" + opts.rawQuery
	}

	var body io.Reader
	payloadHash := EmptyPayloadSHA256
	if len(opts.body) > 0 {
		body = bytes.NewReader(opts.body)
		payloadHash = hexSHA256(string(opts.body))
	}

	request := httptest.NewRequest(method, url, body)
	request.Header.Set("X-Amz-Content-Sha256", payloadHash)
	credentials := aws.Credentials{AccessKeyID: opts.accessKey, SecretAccessKey: opts.secret}
	err := v4.NewSigner().SignHTTP(context.Background(), credentials, request, payloadHash, opts.service, opts.region, time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatalf("SignHTTP returned error: %v", err)
	}
	return request
}
