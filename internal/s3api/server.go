package s3api

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aahl/tgs3/metadata"
	"github.com/aahl/tgs3/store"
)

type ObjectStore interface {
	ListBuckets(ctx context.Context) ([]metadata.Bucket, error)
	HeadBucket(ctx context.Context, name string) error
	PutObject(ctx context.Context, input store.PutObjectInput) (store.PutObjectResult, error)
	GetObject(ctx context.Context, input store.GetObjectInput) (io.ReadCloser, store.ObjectInfo, error)
	HeadObject(ctx context.Context, bucket, key string) (store.ObjectInfo, error)
	ListObjects(ctx context.Context, input store.ListObjectsInput) (store.ListObjectsResult, error)
	DeleteObject(ctx context.Context, bucket, key string) error
}

type Options struct {
	Region       string
	Credentials  map[string]string
	Ready        func() bool
	SigV4Clock   func() time.Time
	SigV4MaxSkew time.Duration
	Logger       *log.Logger
}

type Server struct {
	store  ObjectStore
	ready  func() bool
	verify *SigV4Verifier
	logger *log.Logger
}

func NewServer(objectStore ObjectStore, options Options) http.Handler {
	ready := options.Ready
	if ready == nil {
		ready = func() bool { return true }
	}
	verifierOptions := []SigV4VerifierOption{}
	if options.SigV4Clock != nil {
		verifierOptions = append(verifierOptions, WithSigV4Clock(options.SigV4Clock))
	}
	if options.SigV4MaxSkew != 0 {
		verifierOptions = append(verifierOptions, WithSigV4MaxSkew(options.SigV4MaxSkew))
	}
	logger := options.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Server{
		store:  objectStore,
		ready:  ready,
		verify: NewSigV4Verifier(options.Region, options.Credentials, verifierOptions...),
		logger: logger,
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
		return
	case "/readyz":
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		if s.ready() {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ready")
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "not ready")
		return
	}

	if shouldServeHTMLRoot(r) {
		http.Error(w, http.StatusText(http.StatusNotImplemented), http.StatusNotImplemented)
		return
	}

	if r.Header.Get("X-Amz-Content-Sha256") == "UNSIGNED-PAYLOAD" {
		s.logger.Printf("debug event=s3_unsigned_payload method=%q path=%q accepted=true", r.Method, r.URL.Path)
	}
	if _, err := s.verify.Verify(r); err != nil {
		WriteErrorResponse(w, r, MapError(err), r.URL.Path, "")
		return
	}

	bucket, key, hasBucket := splitPath(r.URL)
	if !hasBucket {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		s.listBuckets(w, r)
		return
	}

	if key == "" {
		s.handleBucket(w, r, bucket)
		return
	}
	s.handleObject(w, r, bucket, key)
}

func shouldServeHTMLRoot(r *http.Request) bool {
	if r.URL.Path != "/" || !prefersHTML(r.Header.Get("Accept")) {
		return false
	}
	if r.Header.Get("Authorization") != "" || r.Header.Get("X-Amz-Date") != "" || r.Header.Get("X-Amz-Content-Sha256") != "" {
		return false
	}
	for name := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-amz-") {
			return false
		}
	}
	query := r.URL.Query()
	for key := range query {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "x-amz-") {
			return false
		}
		switch lower {
		case "list-type", "delimiter", "prefix", "continuation-token", "max-keys", "start-after", "encoding-type", "fetch-owner":
			return false
		}
	}
	return true
}

func prefersHTML(accept string) bool {
	if accept == "" {
		return false
	}
	parts := strings.Split(strings.ToLower(accept), ",")
	for _, part := range parts {
		mediaType := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if mediaType == "text/html" {
			return true
		}
		if mediaType == "application/xml" || mediaType == "text/xml" || mediaType == "*/*" {
			return false
		}
	}
	return false
}

func splitPath(u *url.URL) (bucket, key string, hasBucket bool) {
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return "", "", false
	}
	bucketPart, keyPart, _ := strings.Cut(trimmed, "/")
	bucket, err := url.PathUnescape(bucketPart)
	if err != nil {
		bucket = bucketPart
	}
	if keyPart == "" {
		return bucket, "", true
	}
	key, err = url.PathUnescape(keyPart)
	if err != nil {
		key = keyPart
	}
	return bucket, key, true
}

func (s *Server) handleBucket(w http.ResponseWriter, r *http.Request, bucket string) {
	switch r.Method {
	case http.MethodPut:
		if err := s.store.HeadBucket(r.Context(), bucket); err != nil {
			s.logger.Printf("debug event=s3_create_bucket bucket=%q result=error error=%q", bucket, sanitizeLogError(err))
			WriteErrorResponse(w, r, MapError(err), r.URL.Path, "")
			return
		}
		s.logger.Printf("debug event=s3_create_bucket bucket=%q result=success", bucket)
		w.WriteHeader(http.StatusOK)
	case http.MethodHead:
		if err := s.store.HeadBucket(r.Context(), bucket); err != nil {
			WriteErrorResponse(w, r, MapError(err), r.URL.Path, "")
			return
		}
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		s.listObjectsV2(w, r, bucket)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	switch r.Method {
	case http.MethodPut:
		s.putObject(w, r, bucket, key)
	case http.MethodGet:
		s.getObject(w, r, bucket, key)
	case http.MethodHead:
		s.headObject(w, r, bucket, key)
	case http.MethodDelete:
		if err := s.store.DeleteObject(r.Context(), bucket, key); err != nil {
			WriteErrorResponse(w, r, MapError(err), r.URL.Path, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) listBuckets(w http.ResponseWriter, r *http.Request) {
	buckets, err := s.store.ListBuckets(r.Context())
	if err != nil {
		WriteErrorResponse(w, r, MapError(err), r.URL.Path, "")
		return
	}
	result := ListAllMyBucketsResult{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/"}
	for _, bucket := range buckets {
		result.Buckets.Buckets = append(result.Buckets.Buckets, Bucket{Name: bucket.Name, CreationDate: bucket.CreatedAt.UTC().Format(time.RFC3339)})
	}
	writeXML(w, http.StatusOK, result)
}

func (s *Server) listObjectsV2(w http.ResponseWriter, r *http.Request, bucket string) {
	query := r.URL.Query()
	if value := query.Get("list-type"); value != "" && value != "2" {
		WriteErrorResponse(w, r, ErrInvalidArgument, r.URL.Path, "")
		return
	}
	maxKeys := 1000
	if raw := query.Get("max-keys"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			WriteErrorResponse(w, r, ErrInvalidArgument, r.URL.Path, "")
			return
		}
		maxKeys = parsed
	}
	afterKey := ""
	if token := query.Get("continuation-token"); token != "" {
		decoded, err := DecodeContinuationToken(token)
		if err != nil {
			WriteErrorResponse(w, r, ErrInvalidArgument, r.URL.Path, "")
			return
		}
		afterKey = decoded
	}
	result, err := s.store.ListObjects(r.Context(), store.ListObjectsInput{
		Bucket:    bucket,
		Prefix:    query.Get("prefix"),
		Delimiter: query.Get("delimiter"),
		AfterKey:  afterKey,
		Limit:     maxKeys,
	})
	if err != nil {
		WriteErrorResponse(w, r, MapError(err), r.URL.Path, "")
		return
	}
	response := ListBucketResult{
		Xmlns:             "http://s3.amazonaws.com/doc/2006-03-01/",
		Name:              bucket,
		Prefix:            query.Get("prefix"),
		Delimiter:         query.Get("delimiter"),
		MaxKeys:           maxKeys,
		KeyCount:          len(result.Objects) + len(result.CommonPrefixes),
		ContinuationToken: query.Get("continuation-token"),
		IsTruncated:       result.IsTruncated,
	}
	for _, object := range result.Objects {
		response.Contents = append(response.Contents, Contents{
			Key:          object.Key,
			LastModified: object.LastModified.UTC().Format(time.RFC3339),
			ETag:         fmt.Sprintf("\"%s\"", object.ETag),
			Size:         object.Size,
			StorageClass: "STANDARD",
		})
	}
	for _, prefix := range result.CommonPrefixes {
		response.CommonPrefixes = append(response.CommonPrefixes, CommonPrefixes{Prefix: prefix})
	}
	if result.IsTruncated && result.NextContinuationAfter != "" {
		token, err := EncodeContinuationToken(result.NextContinuationAfter)
		if err != nil {
			WriteErrorResponse(w, r, ErrInternalError, r.URL.Path, "")
			return
		}
		response.NextContinuationToken = token
	}
	writeXML(w, http.StatusOK, response)
}

func (s *Server) putObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if r.ContentLength < 0 {
		WriteErrorResponse(w, r, ErrMissingContentLength, r.URL.Path, "")
		return
	}
	payloadHashMode := "sha256"
	if r.Header.Get("X-Amz-Content-Sha256") == "UNSIGNED-PAYLOAD" {
		payloadHashMode = "unsigned"
	}
	s.logger.Printf("debug event=s3_put_object_request bucket=%q key=%q content_length=%d content_type=%q payload_hash_mode=%q", bucket, key, r.ContentLength, r.Header.Get("Content-Type"), payloadHashMode)
	result, err := s.store.PutObject(r.Context(), store.PutObjectInput{
		Bucket:      bucket,
		Key:         key,
		ContentType: r.Header.Get("Content-Type"),
		Size:        r.ContentLength,
		Body:        r.Body,
	})
	if err != nil {
		s.logger.Printf("debug event=s3_put_object_result bucket=%q key=%q result=error error=%q", bucket, key, sanitizeLogError(err))
		WriteErrorResponse(w, r, MapError(err), r.URL.Path, "")
		return
	}
	s.logger.Printf("debug event=s3_put_object_result bucket=%q key=%q result=success etag=%q", bucket, key, result.ETag)
	w.Header().Set("ETag", fmt.Sprintf("\"%s\"", result.ETag))
	w.WriteHeader(http.StatusOK)
}

func sanitizeLogError(err error) string {
	if err == nil {
		return ""
	}
	return sensitiveAssignmentPattern.ReplaceAllString(err.Error(), "$1$2[redacted]")
}

var sensitiveAssignmentPattern = regexp.MustCompile(`(?i)\b(bot_token|secret_key)\b\s*([=:])\s*([^\s,;]+)`)

func (s *Server) getObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	var requestedRange *store.ByteRange
	status := http.StatusOK
	if header := r.Header.Get("Range"); header != "" {
		info, err := s.store.HeadObject(r.Context(), bucket, key)
		if err != nil {
			WriteErrorResponse(w, r, MapError(err), r.URL.Path, "")
			return
		}
		parsed, err := store.ParseRange(header, info.Size)
		if err != nil {
			WriteErrorResponse(w, r, MapError(err), r.URL.Path, "")
			return
		}
		requestedRange = &parsed
		status = http.StatusPartialContent
	}
	reader, info, err := s.store.GetObject(r.Context(), store.GetObjectInput{Bucket: bucket, Key: key, Range: requestedRange})
	if err != nil {
		WriteErrorResponse(w, r, MapError(err), r.URL.Path, "")
		return
	}
	defer reader.Close()
	writeObjectHeaders(w, info)
	length := info.Size
	if requestedRange != nil {
		length = requestedRange.Length()
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", requestedRange.Start, requestedRange.End, info.Size))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(status)
	_, _ = io.Copy(w, reader)
}

func (s *Server) headObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	info, err := s.store.HeadObject(r.Context(), bucket, key)
	if err != nil {
		WriteErrorResponse(w, r, MapError(err), r.URL.Path, "")
		return
	}
	writeObjectHeaders(w, info)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.WriteHeader(http.StatusOK)
}

func writeObjectHeaders(w http.ResponseWriter, info store.ObjectInfo) {
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("ETag", fmt.Sprintf("\"%s\"", info.ETag))
	if info.ContentType != "" {
		w.Header().Set("Content-Type", info.ContentType)
	}
	w.Header().Set("Last-Modified", info.LastModified.UTC().Format(http.TimeFormat))
}

func writeXML(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(value)
}
