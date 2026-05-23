package s3api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const EmptyPayloadSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

type Identity struct {
	AccessKey string
}

type SigV4Verifier struct {
	region  string
	keys    map[string]string
	clock   func() time.Time
	maxSkew time.Duration
}

type SigV4VerifierOption func(*SigV4Verifier)

func WithSigV4Clock(clock func() time.Time) SigV4VerifierOption {
	return func(v *SigV4Verifier) {
		v.clock = clock
	}
}

func WithSigV4MaxSkew(maxSkew time.Duration) SigV4VerifierOption {
	return func(v *SigV4Verifier) {
		v.maxSkew = maxSkew
	}
}

type sigV4Authorization struct {
	accessKey     string
	date          string
	region        string
	service       string
	signedHeaders []string
	signature     string
}

func NewSigV4Verifier(region string, keys map[string]string, opts ...SigV4VerifierOption) *SigV4Verifier {
	clonedKeys := make(map[string]string, len(keys))
	for accessKey, secret := range keys {
		clonedKeys[accessKey] = secret
	}
	verifier := &SigV4Verifier{region: region, keys: clonedKeys, clock: time.Now, maxSkew: 15 * time.Minute}
	for _, opt := range opts {
		opt(verifier)
	}
	return verifier
}

func (v *SigV4Verifier) Verify(r *http.Request) (Identity, error) {
	auth, err := parseSigV4Authorization(r.Header.Get("Authorization"))
	if err != nil {
		return Identity{}, ErrSignatureDoesNotMatch
	}
	if auth.region != v.region || auth.service != "s3" {
		return Identity{}, ErrSignatureDoesNotMatch
	}

	secret, ok := v.keys[auth.accessKey]
	if !ok {
		return Identity{}, ErrInvalidAccessKeyID
	}

	xAmzDate := r.Header.Get("X-Amz-Date")
	if xAmzDate == "" {
		return Identity{}, ErrSignatureDoesNotMatch
	}
	if err := v.validateRequestTime(xAmzDate, auth.date); err != nil {
		return Identity{}, ErrSignatureDoesNotMatch
	}

	payloadHash, err := payloadHash(r)
	if err != nil {
		return Identity{}, ErrSignatureDoesNotMatch
	}

	canonicalRequest, err := buildCanonicalRequest(r, auth.signedHeaders, payloadHash)
	if err != nil {
		return Identity{}, ErrSignatureDoesNotMatch
	}

	credentialScope := strings.Join([]string{auth.date, auth.region, auth.service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		xAmzDate,
		credentialScope,
		hexSHA256(canonicalRequest),
	}, "\n")

	signingKey := deriveSigningKey(secret, auth.date, auth.region, auth.service)
	expectedSignature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
	if subtle.ConstantTimeCompare([]byte(expectedSignature), []byte(auth.signature)) != 1 {
		return Identity{}, ErrSignatureDoesNotMatch
	}

	return Identity{AccessKey: auth.accessKey}, nil
}

func (v *SigV4Verifier) validateRequestTime(xAmzDate, scopeDate string) error {
	signedAt, err := time.Parse("20060102T150405Z", xAmzDate)
	if err != nil {
		return err
	}
	if scopeDate != signedAt.UTC().Format("20060102") {
		return fmt.Errorf("credential scope date does not match request timestamp")
	}
	if v.maxSkew <= 0 {
		return nil
	}
	now := v.clock().UTC()
	if signedAt.Before(now.Add(-v.maxSkew)) || signedAt.After(now.Add(v.maxSkew)) {
		return fmt.Errorf("request timestamp outside allowed skew")
	}
	return nil
}

func parseSigV4Authorization(header string) (sigV4Authorization, error) {
	const prefix = "AWS4-HMAC-SHA256 "
	if !strings.HasPrefix(header, prefix) {
		return sigV4Authorization{}, fmt.Errorf("invalid authorization scheme")
	}

	parts := strings.Split(header[len(prefix):], ",")
	values := make(map[string]string, len(parts))
	for _, part := range parts {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || value == "" {
			return sigV4Authorization{}, fmt.Errorf("invalid authorization field")
		}
		values[key] = value
	}

	credentialParts := strings.Split(values["Credential"], "/")
	if len(credentialParts) != 5 || credentialParts[4] != "aws4_request" {
		return sigV4Authorization{}, fmt.Errorf("invalid credential scope")
	}

	signedHeaders := strings.Split(values["SignedHeaders"], ";")
	if len(signedHeaders) == 0 || signedHeaders[0] == "" || values["Signature"] == "" {
		return sigV4Authorization{}, fmt.Errorf("missing authorization fields")
	}

	return sigV4Authorization{
		accessKey:     credentialParts[0],
		date:          credentialParts[1],
		region:        credentialParts[2],
		service:       credentialParts[3],
		signedHeaders: signedHeaders,
		signature:     strings.ToLower(values["Signature"]),
	}, nil
}

func payloadHash(r *http.Request) (string, error) {
	hash := r.Header.Get("X-Amz-Content-Sha256")
	if hash == "" {
		if r.Body == nil || r.Body == http.NoBody {
			return EmptyPayloadSHA256, nil
		}
		return "", fmt.Errorf("missing payload hash")
	}
	if r.Body == nil || r.Body == http.NoBody {
		if hash != EmptyPayloadSHA256 {
			return "", fmt.Errorf("payload hash mismatch")
		}
		return hash, nil
	}
	if hash == "UNSIGNED-PAYLOAD" {
		return hash, nil
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))

	actual := sha256.Sum256(body)
	if hex.EncodeToString(actual[:]) != strings.ToLower(hash) {
		return "", fmt.Errorf("payload hash mismatch")
	}
	return strings.ToLower(hash), nil
}

func buildCanonicalRequest(r *http.Request, signedHeaders []string, payloadHash string) (string, error) {
	canonicalHeaders, signedHeaderList, err := canonicalHeaders(r, signedHeaders)
	if err != nil {
		return "", err
	}

	return strings.Join([]string{
		r.Method,
		canonicalURI(r.URL),
		canonicalQueryString(r.URL),
		canonicalHeaders,
		signedHeaderList,
		payloadHash,
	}, "\n"), nil
}

func canonicalURI(u *url.URL) string {
	path := u.EscapedPath()
	if path == "" {
		return "/"
	}

	segments := strings.Split(path, "/")
	for i, segment := range segments {
		segments[i] = sigV4Encode(segment, false)
	}
	result := strings.Join(segments, "/")
	if result == "" || result[0] != '/' {
		return "/" + result
	}
	return result
}

func canonicalQueryString(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}

	type pair struct {
		key   string
		value string
	}

	tokens := strings.Split(u.RawQuery, "&")
	pairs := make([]pair, 0, len(tokens))
	for _, token := range tokens {
		key, value, hasValue := strings.Cut(token, "=")
		if !hasValue {
			value = ""
		}
		pairs = append(pairs, pair{
			key:   canonicalQueryComponent(key),
			value: canonicalQueryComponent(value),
		})
	}

	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].key == pairs[j].key {
			return pairs[i].value < pairs[j].value
		}
		return pairs[i].key < pairs[j].key
	})

	encoded := make([]string, 0, len(pairs))
	for _, item := range pairs {
		encoded = append(encoded, item.key+"="+item.value)
	}
	return strings.Join(encoded, "&")
}

func canonicalQueryComponent(raw string) string {
	decoded := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] == '%' && i+2 < len(raw) {
			hi, okHi := fromHex(raw[i+1])
			lo, okLo := fromHex(raw[i+2])
			if okHi && okLo {
				decoded = append(decoded, hi<<4|lo)
				i += 2
				continue
			}
		}
		decoded = append(decoded, raw[i])
	}
	return sigV4Encode(string(decoded), true)
}

func fromHex(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	default:
		return 0, false
	}
}

func canonicalHeaders(r *http.Request, signedHeaders []string) (string, string, error) {
	normalizedHeaders := make([]string, 0, len(signedHeaders))
	seen := make(map[string]struct{}, len(signedHeaders))
	for _, header := range signedHeaders {
		name := strings.ToLower(strings.TrimSpace(header))
		if name == "" {
			return "", "", fmt.Errorf("empty signed header")
		}
		if _, ok := seen[name]; ok {
			return "", "", fmt.Errorf("duplicate signed header")
		}
		seen[name] = struct{}{}
		normalizedHeaders = append(normalizedHeaders, name)
	}

	sort.Strings(normalizedHeaders)

	canonical := make([]string, 0, len(normalizedHeaders))
	for _, name := range normalizedHeaders {
		value, ok := headerValue(r, name)
		if !ok {
			return "", "", fmt.Errorf("missing signed header")
		}
		canonical = append(canonical, name+":"+normalizeHeaderValue(value))
	}

	return strings.Join(canonical, "\n") + "\n", strings.Join(normalizedHeaders, ";"), nil
}

func headerValue(r *http.Request, name string) (string, bool) {
	switch name {
	case "host":
		if r.Host != "" {
			return r.Host, true
		}
		if r.URL.Host != "" {
			return r.URL.Host, true
		}
		return "", false
	case "content-length":
		if len(r.Header.Values("Content-Length")) > 0 {
			break
		}
		if r.ContentLength >= 0 {
			return fmt.Sprintf("%d", r.ContentLength), true
		}
	}
	values := r.Header.Values(http.CanonicalHeaderKey(name))
	if len(values) == 0 {
		return "", false
	}
	return strings.Join(values, ","), true
}

func normalizeHeaderValue(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func deriveSigningKey(secret, date, region, service string) []byte {
	dateKey := hmacSHA256([]byte("AWS4"+secret), date)
	regionKey := hmacSHA256(dateKey, region)
	serviceKey := hmacSHA256(regionKey, service)
	return hmacSHA256(serviceKey, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(data))
	return mac.Sum(nil)
}

func hexSHA256(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

func sigV4Encode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		if c == '/' && !encodeSlash {
			b.WriteByte(c)
			continue
		}
		b.WriteString(fmt.Sprintf("%%%02X", c))
	}
	return b.String()
}
