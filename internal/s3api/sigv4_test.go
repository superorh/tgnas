package s3api

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
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

func TestVerifySigV4LogsSignatureMismatchDiagnostics(t *testing.T) {
	var logs strings.Builder
	request := signedTestRequest(t, signedRequestOptions{accessKey: "AKID", secret: "WRONG", region: "us-east-1", service: "s3", target: "https://s3.example.com/?x-id=ListBuckets"})
	verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time { return time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC) }), WithSigV4Logger(log.New(&logs, "", 0)))

	_, err := verifier.Verify(request)
	if err != ErrSignatureDoesNotMatch {
		t.Fatalf("err = %v, want ErrSignatureDoesNotMatch", err)
	}

	got := logs.String()
	for _, want := range []string{
		`event=sigv4_verify_failure`,
		`stage="signature"`,
		`method="GET"`,
		`path="/"`,
		`raw_query="x-id=ListBuckets"`,
		`host="s3.example.com"`,
		`access_key="AKID"`,
		`scope="20240102/us-east-1/s3/aws4_request"`,
		`signed_headers="host;x-amz-content-sha256;x-amz-date"`,
		`payload_hash="e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"`,
		`signed_header_value_hashes="host:`,
		`x-amz-content-sha256:`,
		`x-amz-date:`,
		`canonical_request_hash=`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("log %q does not contain %s", got, want)
		}
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

func TestVerifySigV4ListBucketsSignedByAWSSDK(t *testing.T) {
	request := signedTestRequest(t, signedRequestOptions{accessKey: "AKID", secret: "SECRET", region: "us-east-1", service: "s3", target: "https://s3.example.com/?x-id=ListBuckets"})
	verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time { return time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC) }))
	identity, err := verifier.Verify(request)
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	if identity.AccessKey != "AKID" {
		t.Fatalf("identity = %+v", identity)
	}
}

func TestVerifySigV4ListBucketsWithSignedSDKHeaders(t *testing.T) {
	request := signedTestRequest(t, signedRequestOptions{
		accessKey: "AKID",
		secret:    "SECRET",
		region:    "us-east-1",
		service:   "s3",
		target:    "https://s3.example.com/?x-id=ListBuckets",
		headers: map[string]string{
			"Accept-Encoding":       "identity",
			"Amz-Sdk-Invocation-Id": "12345678-1234-1234-1234-123456789abc",
			"Amz-Sdk-Request":       "attempt=1; max=3",
		},
	})
	verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time { return time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC) }))
	identity, err := verifier.Verify(request)
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	if identity.AccessKey != "AKID" {
		t.Fatalf("identity = %+v", identity)
	}
}

func TestVerifySigV4AllowsProxyRewrittenAcceptEncoding(t *testing.T) {
	for _, tc := range []struct {
		name           string
		signedValue    string
		receivedValue  string
	}{
		{name: "identity rewritten to gzip br", signedValue: "identity", receivedValue: "gzip, br"},
		{name: "gzip rewritten to gzip br", signedValue: "gzip", receivedValue: "gzip, br"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := signedTestRequest(t, signedRequestOptions{
				accessKey: "AKID",
				secret:    "SECRET",
				region:    "us-east-1",
				service:   "s3",
				target:    "https://s3.example.com/?x-id=ListBuckets",
				headers: map[string]string{
					"Accept-Encoding":       tc.signedValue,
					"Amz-Sdk-Invocation-Id": "12345678-1234-1234-1234-123456789abc",
					"Amz-Sdk-Request":       "attempt=1; max=3",
				},
			})
			request.Header.Set("Accept-Encoding", tc.receivedValue)

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

func TestVerifySigV4AllowsProxyZeroContentLengthForUnsignedPayload(t *testing.T) {
	request := signedTestRequest(t, signedRequestOptions{
		accessKey: "AKID",
		secret:    "SECRET",
		region:    "us-east-1",
		service:   "s3",
		method:    http.MethodGet,
		target:    "https://s3.example.com/",
		payloadHash: "UNSIGNED-PAYLOAD",
		headers: map[string]string{
			"Content-Type":          "",
			"Amz-Sdk-Invocation-Id": "12345678-1234-1234-1234-123456789abc",
			"Amz-Sdk-Request":       "attempt=1; max=3",
			"X-Amz-Api-Version":     "2006-03-01",
		},
	})
	request.Header.Set("Content-Length", "0")
	request.ContentLength = -1

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

func TestVerifySigV4PresignedGetAndHead(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			request := presignedTestRequest(t, presignedRequestOptions{method: method})
			verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time { return time.Date(2024, 1, 2, 3, 5, 0, 0, time.UTC) }))

			identity, err := verifier.Verify(request)
			if err != nil {
				t.Fatalf("Verify returned error: %v", err)
			}
			if identity.AccessKey != "AKID" {
				t.Fatalf("identity = %+v", identity)
			}
			if !identity.Presigned {
				t.Fatalf("identity = %+v, want Presigned true", identity)
			}
		})
	}
}

func TestVerifySigV4PresignedResponseOverrideIsSigned(t *testing.T) {
	request := presignedTestRequest(t, presignedRequestOptions{responseContentDisposition: "attachment; filename*=UTF-8''hello.txt"})
	verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time { return time.Date(2024, 1, 2, 3, 5, 0, 0, time.UTC) }))

	identity, err := verifier.Verify(request)
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	if identity.AccessKey != "AKID" {
		t.Fatalf("identity = %+v", identity)
	}
	if !identity.Presigned {
		t.Fatalf("identity = %+v, want Presigned true", identity)
	}

	tampered := request.Clone(request.Context())
	tampered.Host = request.Host
	query := tampered.URL.Query()
	query.Set("response-content-disposition", "attachment; filename*=UTF-8''tampered.txt")
	tampered.URL.RawQuery = query.Encode()

	_, err = verifier.Verify(tampered)
	if err != ErrSignatureDoesNotMatch {
		t.Fatalf("err = %v, want ErrSignatureDoesNotMatch", err)
	}
}

func TestVerifySigV4PresignedRejectsMissingRequiredQueryParameter(t *testing.T) {
	for _, key := range []string{
		"X-Amz-Algorithm",
		"X-Amz-Credential",
		"X-Amz-Date",
		"X-Amz-Expires",
		"X-Amz-SignedHeaders",
		"X-Amz-Signature",
	} {
		t.Run(key, func(t *testing.T) {
			request := presignedTestRequest(t, presignedRequestOptions{})
			query := request.URL.Query()
			query.Del(key)
			request.URL.RawQuery = query.Encode()

			verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time { return time.Date(2024, 1, 2, 3, 5, 0, 0, time.UTC) }))
			_, err := verifier.Verify(request)
			if err != ErrSignatureDoesNotMatch {
				t.Fatalf("err = %v, want ErrSignatureDoesNotMatch", err)
			}
		})
	}
}

func TestVerifySigV4PresignedRejectsExpiredURL(t *testing.T) {
	request := presignedTestRequest(t, presignedRequestOptions{expires: time.Minute})
	verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time { return time.Date(2024, 1, 2, 3, 5, 6, 0, time.UTC) }))

	_, err := verifier.Verify(request)
	if err != ErrSignatureDoesNotMatch {
		t.Fatalf("err = %v, want ErrSignatureDoesNotMatch", err)
	}
}

func TestVerifySigV4PresignedRejectsExpiresAboveSevenDays(t *testing.T) {
	request := presignedTestRequest(t, presignedRequestOptions{expires: 7*24*time.Hour + time.Second})
	verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time { return time.Date(2024, 1, 2, 3, 5, 0, 0, time.UTC) }))

	_, err := verifier.Verify(request)
	if err != ErrSignatureDoesNotMatch {
		t.Fatalf("err = %v, want ErrSignatureDoesNotMatch", err)
	}
}

func TestVerifySigV4PresignedRejectsWrongRegionServiceAndAccessKey(t *testing.T) {
	verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time { return time.Date(2024, 1, 2, 3, 5, 0, 0, time.UTC) }))

	for _, tc := range []struct {
		name    string
		request *http.Request
		wantErr error
	}{
		{
			name:    "wrong region",
			request: presignedTestRequest(t, presignedRequestOptions{region: "eu-west-1"}),
			wantErr: ErrSignatureDoesNotMatch,
		},
		{
			name:    "wrong service",
			request: presignedTestRequest(t, presignedRequestOptions{service: "ec2"}),
			wantErr: ErrSignatureDoesNotMatch,
		},
		{
			name:    "unknown access key",
			request: presignedTestRequest(t, presignedRequestOptions{accessKey: "UNKNOWN"}),
			wantErr: ErrInvalidAccessKeyID,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := verifier.Verify(tc.request)
			if err != tc.wantErr {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

type presignedRequestOptions struct {
	method                     string
	accessKey                  string
	secret                     string
	region                     string
	service                    string
	expires                    time.Duration
	responseContentDisposition string
}

type signedRequestOptions struct {
	accessKey   string
	secret      string
	region      string
	service     string
	method      string
	target      string
	rawQuery    string
	headers     map[string]string
	payloadHash string
	body        []byte
}

func presignedTestRequest(t *testing.T, opts presignedRequestOptions) *http.Request {
	t.Helper()

	method := opts.method
	if method == "" {
		method = http.MethodGet
	}
	accessKey := opts.accessKey
	if accessKey == "" {
		accessKey = "AKID"
	}
	secret := opts.secret
	if secret == "" {
		secret = "SECRET"
	}
	region := opts.region
	if region == "" {
		region = "us-east-1"
	}
	service := opts.service
	if service == "" {
		service = "s3"
	}
	expires := opts.expires
	if expires == 0 {
		expires = 15 * time.Minute
	}

	request := httptest.NewRequest(method, "https://example.com/photos/hello.txt", nil)
	request.Host = "example.com"
	query := request.URL.Query()
	query.Set("X-Amz-Expires", strconv.FormatInt(int64(expires/time.Second), 10))
	if opts.responseContentDisposition != "" {
		query.Set("response-content-disposition", opts.responseContentDisposition)
	}
	request.URL.RawQuery = query.Encode()

	credentials := aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secret}
	signedURL, _, err := v4.NewSigner().PresignHTTP(context.Background(), credentials, request, "UNSIGNED-PAYLOAD", service, region, time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), func(o *v4.SignerOptions) {
		o.DisableURIPathEscaping = true
	})
	if err != nil {
		t.Fatalf("PresignHTTP returned error: %v", err)
	}

	presigned := httptest.NewRequest(method, signedURL, nil)
	presigned.Host = "example.com"
	return presigned
}

func signedTestRequest(t *testing.T, opts signedRequestOptions) *http.Request {
	t.Helper()

	method := opts.method
	if method == "" {
		method = http.MethodGet
	}

	url := opts.target
	if url == "" {
		url = "https://example.com/photos/hello.txt"
		if opts.rawQuery != "" {
			url += "?" + opts.rawQuery
		}
	}

	var body io.Reader
	payloadHash := EmptyPayloadSHA256
	if opts.payloadHash != "" {
		payloadHash = opts.payloadHash
	}
	if len(opts.body) > 0 {
		body = bytes.NewReader(opts.body)
		if opts.payloadHash == "" {
			payloadHash = hexSHA256(string(opts.body))
		}
	}

	request := httptest.NewRequest(method, url, body)
	for name, value := range opts.headers {
		request.Header.Set(name, value)
	}
	request.Header.Set("X-Amz-Content-Sha256", payloadHash)
	credentials := aws.Credentials{AccessKeyID: opts.accessKey, SecretAccessKey: opts.secret}
	err := v4.NewSigner().SignHTTP(context.Background(), credentials, request, payloadHash, opts.service, opts.region, time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatalf("SignHTTP returned error: %v", err)
	}
	return request
}
