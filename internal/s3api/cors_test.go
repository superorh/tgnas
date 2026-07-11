package s3api

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestCompileCORSPolicyMatchesOriginPatterns(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		origin  string
		want    bool
	}{
		{name: "exact", pattern: "https://app.example.com", origin: "HTTPS://APP.EXAMPLE.COM", want: true},
		{name: "exact anchored", pattern: "https://app.example.com", origin: "https://app.example.com.evil", want: false},
		{name: "glob", pattern: "https://*.staging.example.com", origin: "https://a.staging.example.com", want: true},
		{name: "glob suffix anchored", pattern: "https://*.staging.example.com", origin: "https://a.staging.example.com/path", want: false},
		{name: "glob rejects evil host suffix", pattern: "https://*.staging.example.com", origin: "https://a.staging.example.com.evil", want: false},
		{name: "glob requires literal dot", pattern: "https://*.staging.example.com", origin: "https://staging.example.com", want: false},
		{name: "consecutive stars", pattern: "https://**.example.com", origin: "https://a.example.com", want: true},
		{name: "glob metacharacters literal", pattern: "https://*.example[dev].com", origin: "https://a.example[dev].com", want: true},
		{name: "glob metacharacters not regex", pattern: "https://*.example[dev].com", origin: "https://a.exampled.com", want: false},
		{name: "single star", pattern: "*", origin: "https://anything.example/path", want: true},
		{name: "single star empty", pattern: "*", origin: "", want: true},
		{name: "regex", pattern: `/^https://[a-z]+\.example\.com$/`, origin: "https://app.example.com", want: true},
		{name: "regex case insensitive", pattern: `/^https://[a-z]+\.example\.com$/`, origin: "HTTPS://APP.EXAMPLE.COM", want: true},
		{name: "regex anchored", pattern: `/^https://[a-z]+\.example\.com$/`, origin: "https://app.example.com.evil", want: false},
		{name: "regex multiple slashes", pattern: `/^https://example\.com/a/b$/`, origin: "https://example.com/a/b", want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := CompileCORSPolicy([]string{tc.pattern}, nil)
			if err != nil {
				t.Fatalf("CompileCORSPolicy returned error: %v", err)
			}
			matcher := policy.matcherForBucket("", false)
			if got := matcher.allow(tc.origin); got != tc.want {
				t.Fatalf("allow(%q) = %v, want %v", tc.origin, got, tc.want)
			}
		})
	}
}

func TestCompileCORSPolicyIgnoresEmptyRulesAndMatchesAnyRule(t *testing.T) {
	policy, err := CompileCORSPolicy([]string{"", "https://one.example", "https://two.example"}, nil)
	if err != nil {
		t.Fatalf("CompileCORSPolicy returned error: %v", err)
	}
	matcher := policy.matcherForBucket("", false)
	if !matcher.allow("https://two.example") {
		t.Fatal("second rule did not match")
	}
}

func TestCompileCORSPolicyRejectsInvalidRegexWithoutPartialPolicy(t *testing.T) {
	tests := []struct {
		name     string
		global   []string
		buckets  map[string][]string
		wantText string
	}{
		{name: "global", global: []string{"/[invalid/"}, wantText: "global CORS policy"},
		{name: "bucket", buckets: map[string][]string{"photos": {"/[invalid/"}}, wantText: `bucket "photos" CORS policy`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := CompileCORSPolicy(tc.global, tc.buckets)
			if err == nil || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("policy = %#v, err = %v, want contextual error containing %q", policy, err, tc.wantText)
			}
			if policy != nil {
				t.Fatalf("policy = %#v, want nil on compile failure", policy)
			}
		})
	}
}

func TestCORSPolicyBucketOverrideAndFallback(t *testing.T) {
	policy, err := CompileCORSPolicy(
		[]string{"https://global.example"},
		map[string][]string{
			"override":        {"https://bucket.example"},
			"fallback-empty":  {},
			"fallback-blanks": {"", ""},
		},
	)
	if err != nil {
		t.Fatalf("CompileCORSPolicy returned error: %v", err)
	}

	if policy.matcherForBucket("override", true).allow("https://global.example") {
		t.Fatal("non-empty bucket override consulted global matcher")
	}
	if !policy.matcherForBucket("override", true).allow("https://bucket.example") {
		t.Fatal("bucket override did not match")
	}
	for _, bucket := range []string{"fallback-empty", "fallback-blanks", "absent"} {
		if !policy.matcherForBucket(bucket, true).allow("https://global.example") {
			t.Fatalf("bucket %q did not fall back to global matcher", bucket)
		}
	}
	if !policy.matcherForBucket("", false).allow("https://global.example") {
		t.Fatal("bucket-less path did not use global matcher")
	}
}

func TestAddVaryPreservesAndDeduplicatesAllValues(t *testing.T) {
	header := http.Header{}
	header.Add("Vary", "Accept-Encoding, origin")
	header.Add("Vary", "User-Agent")

	addVary(header, "Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers")
	addVary(header, "ORIGIN", "access-control-request-method")

	var tokens []string
	for _, value := range header.Values("Vary") {
		for _, token := range strings.Split(value, ",") {
			tokens = append(tokens, strings.TrimSpace(token))
		}
	}
	wantCounts := map[string]int{
		"accept-encoding":                1,
		"origin":                         1,
		"user-agent":                     1,
		"access-control-request-method":  1,
		"access-control-request-headers": 1,
	}
	gotCounts := map[string]int{}
	for _, token := range tokens {
		gotCounts[strings.ToLower(token)]++
	}
	if !reflect.DeepEqual(gotCounts, wantCounts) {
		t.Fatalf("Vary token counts = %#v, want %#v; raw values = %#v", gotCounts, wantCounts, header.Values("Vary"))
	}
}
