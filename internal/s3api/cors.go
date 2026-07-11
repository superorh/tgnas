package s3api

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

type OriginMatcher struct {
	rules []*regexp.Regexp
}

type CORSPolicy struct {
	global  OriginMatcher
	buckets map[string]OriginMatcher
}

func CompileCORSPolicy(global []string, buckets map[string][]string) (*CORSPolicy, error) {
	globalMatcher, err := compileOriginMatcher(global)
	if err != nil {
		return nil, fmt.Errorf("global CORS policy: %w", err)
	}

	policy := &CORSPolicy{
		global:  globalMatcher,
		buckets: make(map[string]OriginMatcher, len(buckets)),
	}
	for bucket, patterns := range buckets {
		matcher, err := compileOriginMatcher(patterns)
		if err != nil {
			return nil, fmt.Errorf("bucket %q CORS policy: %w", bucket, err)
		}
		if len(matcher.rules) > 0 {
			policy.buckets[bucket] = matcher
		}
	}
	return policy, nil
}

func compileOriginMatcher(patterns []string) (OriginMatcher, error) {
	matcher := OriginMatcher{}
	for _, pattern := range patterns {
		if pattern == "" {
			continue
		}

		expression := ""
		if len(pattern) >= 2 && strings.HasPrefix(pattern, "/") && strings.HasSuffix(pattern, "/") {
			expression = pattern[1 : len(pattern)-1]
		} else if strings.Contains(pattern, "*") {
			parts := strings.Split(pattern, "*")
			for i := range parts {
				parts[i] = regexp.QuoteMeta(parts[i])
			}
			expression = "^" + strings.Join(parts, ".*") + "$"
		} else {
			expression = "^" + regexp.QuoteMeta(pattern) + "$"
		}

		rule, err := regexp.Compile("(?i:" + expression + ")")
		if err != nil {
			return OriginMatcher{}, fmt.Errorf("compile origin pattern %q: %w", pattern, err)
		}
		matcher.rules = append(matcher.rules, rule)
	}
	return matcher, nil
}

func (m OriginMatcher) allow(origin string) bool {
	for _, rule := range m.rules {
		if rule.MatchString(origin) {
			return true
		}
	}
	return false
}

func (p *CORSPolicy) matcherForBucket(bucket string, hasBucket bool) OriginMatcher {
	if p == nil {
		return OriginMatcher{}
	}
	if hasBucket {
		if matcher, ok := p.buckets[bucket]; ok {
			return matcher
		}
	}
	return p.global
}

func addVary(header http.Header, names ...string) {
	seen := make(map[string]struct{})
	values := header.Values("Vary")
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			token = strings.TrimSpace(token)
			if token != "" {
				seen[strings.ToLower(token)] = struct{}{}
			}
		}
	}
	for _, name := range names {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		header.Add("Vary", name)
		seen[key] = struct{}{}
	}
}

const (
	corsAllowMethods    = "GET, HEAD, PUT, POST, DELETE, OPTIONS"
	corsAllowHeaders    = "Authorization, Content-Type, Range, X-Amz-Date, X-Amz-Content-Sha256"
	corsExposeHeaders   = "ETag, Content-Range, Content-Length, Accept-Ranges, Last-Modified"
	corsPreflightMaxAge = "3600"
)

var corsMethods = map[string]bool{
	"GET": true, "HEAD": true, "PUT": true,
	"POST": true, "DELETE": true, "OPTIONS": true,
}

var corsHeaders = map[string]bool{
	"authorization":        true,
	"content-type":         true,
	"range":                true,
	"x-amz-date":           true,
	"x-amz-content-sha256": true,
}

func requestedHeadersAllowed(value string) bool {
	for _, name := range strings.Split(value, ",") {
		name = strings.TrimSpace(name)
		if name != "" && !corsHeaders[strings.ToLower(name)] {
			return false
		}
	}
	return true
}

func originAllowed(w http.ResponseWriter, r *http.Request, matcher OriginMatcher) bool {
	origin := r.Header.Get("Origin")
	if !matcher.allow(origin) {
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Expose-Headers", corsExposeHeaders)
	return true
}

func corsPreflight(w http.ResponseWriter, r *http.Request, matcher OriginMatcher) {
	addVary(w.Header(), "Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers")
	origin := r.Header.Get("Origin")
	method := strings.ToUpper(strings.TrimSpace(r.Header.Get("Access-Control-Request-Method")))
	if !matcher.allow(origin) || !corsMethods[method] || !requestedHeadersAllowed(r.Header.Get("Access-Control-Request-Headers")) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Methods", corsAllowMethods)
	w.Header().Set("Access-Control-Allow-Headers", corsAllowHeaders)
	w.Header().Set("Access-Control-Max-Age", corsPreflightMaxAge)
	w.WriteHeader(http.StatusNoContent)
}
