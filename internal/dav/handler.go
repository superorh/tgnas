package dav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"net/url"
	"strings"

	"github.com/aahl/tgnas/metadata"
	"golang.org/x/net/webdav"
)

type HandlerOptions struct {
	Prefix      string
	Credentials map[string]string
	Logger      *log.Logger
}

type Handler struct {
	prefix  string
	creds   map[string]string
	handler webdav.Handler
	fs      *FileSystem
	meta    metadata.Store
	logger  *log.Logger
}

func NewHandler(meta metadata.Store, fs *FileSystem, opts HandlerOptions) *Handler {
	prefix := opts.Prefix
	if prefix == "" {
		prefix = "/dav/"
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	h := &Handler{prefix: prefix, creds: opts.Credentials, fs: fs, meta: meta, logger: logger}
	lockSystem := &noLockSystem{}
	h.handler = webdav.Handler{Prefix: prefix, FileSystem: fs, LockSystem: lockSystem, Logger: func(r *http.Request, err error) {
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Printf("webdav method=%q path=%q error=%q", r.Method, r.URL.Path, err.Error())
		}
	}}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, h.prefix) {
		http.NotFound(w, r)
		return
	}
	if !h.checkBasicAuth(w, r) {
		return
	}
	if r.Method == "OPTIONS" {
		h.handleOptions(w, r)
		return
	}
	if r.Method == "PROPFIND" {
		depth := r.Header.Get("Depth")
		if strings.EqualFold(depth, "infinity") {
			http.Error(w, "Depth infinity is forbidden", http.StatusForbidden)
			return
		}
		if depth == "" {
			r.Header.Set("Depth", "1")
		}
	}
	davPath, err := h.requestPath(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	bucket, key, isRoot, err := parsePath(davPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !isRoot && r.Method == "MKCOL" && key == "" {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	if !isRoot {
		bucketRecord, err := h.meta.GetBucket(r.Context(), bucket)
		if err != nil {
			if errors.Is(err, metadata.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		if !bucketRecord.Enabled {
			if r.Method == http.MethodDelete && key == "" {
				h.handleOrphanDelete(w, r, bucket)
				return
			}
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
	}
	if !isRoot && r.Method == http.MethodPut && !strings.HasSuffix(key, "/") {
		if exists, err := h.hasCollection(r.Context(), bucket, key); err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		} else if exists {
			http.Error(w, "collection already exists", http.StatusConflict)
			return
		}
	}
	if !isRoot && r.Method == "MKCOL" && key != "" {
		sibling := strings.TrimSuffix(key, "/")
		if sibling != "" {
			if exists, err := h.hasObject(r.Context(), bucket, sibling); err != nil {
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				return
			} else if exists {
				http.Error(w, "object already exists", http.StatusConflict)
				return
			}
		}
	}
	switch r.Method {
	case "COPY":
		h.handleCopy(w, r, davPath)
		return
	case "MOVE":
		h.handleMove(w, r, davPath)
		return
	}
	h.handler.ServeHTTP(w, r)
}

func (h *Handler) checkBasicAuth(w http.ResponseWriter, r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	secret, exists := h.creds[user]
	if !ok || !exists || secret != pass {
		w.Header().Set("WWW-Authenticate", `Basic realm="tgnas"`)
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return false
	}
	return true
}

func (h *Handler) handleOptions(w http.ResponseWriter, r *http.Request) {
	davPath, err := h.requestPath(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	allow := "OPTIONS, PUT, MKCOL, LOCK, UNLOCK, PROPFIND"
	if info, err := h.fs.Stat(r.Context(), davPath); err == nil {
		if info.IsDir() {
			allow = "OPTIONS, PUT, MKCOL, DELETE, PROPPATCH, COPY, MOVE, LOCK, UNLOCK, PROPFIND"
		} else {
			allow = "OPTIONS, GET, HEAD, POST, DELETE, PROPPATCH, COPY, MOVE, LOCK, UNLOCK, PROPFIND, PUT"
		}
	}
	w.Header().Set("Allow", allow)
	w.Header().Set("DAV", "1, 2")
	w.Header().Set("MS-Author-Via", "DAV")
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) requestPath(r *http.Request) (string, error) {
	if !strings.HasPrefix(r.URL.Path, h.prefix) {
		return "", ErrNotFound
	}
	return "/" + strings.TrimPrefix(r.URL.Path, h.prefix), nil
}

func (h *Handler) handleOrphanDelete(w http.ResponseWriter, r *http.Request, bucket string) {
	if err := h.meta.DeleteBucket(r.Context(), bucket); err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleCopy(w http.ResponseWriter, r *http.Request, srcPath string) {
	h.handleCopyMove(w, r, srcPath, false)
}

func (h *Handler) handleMove(w http.ResponseWriter, r *http.Request, srcPath string) {
	h.handleCopyMove(w, r, srcPath, true)
}

func (h *Handler) handleCopyMove(w http.ResponseWriter, r *http.Request, srcPath string, move bool) {
	dstPath, err := h.resolveDestination(r.Header.Get("Destination"), r)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, webdav.ErrForbidden) {
			status = http.StatusForbidden
		}
		http.Error(w, err.Error(), status)
		return
	}
	overwrite, err := parseOverwrite(r.Header.Get("Overwrite"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	srcBucket, srcKey, srcIsRoot, err := parsePath(srcPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	dstBucket, dstKey, dstIsRoot, err := parsePath(dstPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if srcIsRoot || dstIsRoot || srcKey == "" || dstKey == "" {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	if srcBucket != dstBucket {
		http.Error(w, "cross-bucket copy/move forbidden", http.StatusForbidden)
		return
	}
	srcIsCollection := strings.HasSuffix(srcKey, "/")
	if !srcIsCollection {
		if _, err := h.meta.HeadObject(r.Context(), srcBucket, srcKey); err == nil {
			srcIsCollection = false
		} else if errors.Is(err, metadata.ErrNotFound) {
			exists, collErr := h.hasCollection(r.Context(), srcBucket, srcKey)
			if collErr != nil {
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				return
			}
			srcIsCollection = exists
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if srcIsCollection {
		if conflict, err := h.destinationKindConflict(r.Context(), srcBucket, dstKey, true); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else if conflict {
			http.Error(w, "destination already exists", http.StatusPreconditionFailed)
			return
		}
		srcPrefix := canonicalCollectionKey(srcKey)
		dstPrefix := canonicalCollectionKey(dstKey)
		if move && srcPrefix == dstPrefix {
			http.Error(w, "source and destination are the same", http.StatusForbidden)
			return
		}
		if isSubpath(srcPrefix, dstPrefix) {
			http.Error(w, "cannot copy or move into own subtree", http.StatusForbidden)
			return
		}
		count, err := h.meta.CountObjects(r.Context(), srcBucket, srcPrefix)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if count == 0 {
			http.Error(w, metadata.ErrNotFound.Error(), http.StatusNotFound)
			return
		}
		if count > maxRecursiveObjects {
			http.Error(w, "too many objects", http.StatusInternalServerError)
			return
		}
		var created bool
		if move {
			result, err := h.meta.MovePrefix(r.Context(), srcBucket, srcPrefix, dstPrefix, metadata.MoveOptions{Overwrite: overwrite})
			if err != nil {
				h.writeCopyMoveError(w, err)
				return
			}
			created = result.Created
		} else {
			result, err := h.meta.CopyPrefix(r.Context(), srcBucket, srcPrefix, dstPrefix, metadata.CopyOptions{Overwrite: overwrite})
			if err != nil {
				h.writeCopyMoveError(w, err)
				return
			}
			created = result.Created
		}
		h.writeCreatedStatus(w, created)
		return
	}
	if conflict, err := h.destinationKindConflict(r.Context(), srcBucket, dstKey, false); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if conflict {
		http.Error(w, "destination already exists", http.StatusPreconditionFailed)
		return
	}
	if move && srcKey == dstKey {
		http.Error(w, "source and destination are the same", http.StatusForbidden)
		return
	}
	var created bool
	if move {
		result, err := h.meta.MoveObject(r.Context(), srcBucket, srcKey, dstKey, metadata.MoveOptions{Overwrite: overwrite})
		if err != nil {
			h.writeCopyMoveError(w, err)
			return
		}
		created = result.Created
	} else {
		result, err := h.meta.CopyObject(r.Context(), srcBucket, srcKey, dstKey, metadata.CopyOptions{Overwrite: overwrite})
		if err != nil {
			h.writeCopyMoveError(w, err)
			return
		}
		created = result.Created
	}
	h.writeCreatedStatus(w, created)
}

func (h *Handler) hasObject(ctx context.Context, bucket, key string) (bool, error) {
	_, err := h.meta.HeadObject(ctx, bucket, key)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, metadata.ErrNotFound) {
		return false, nil
	}
	return false, err
}

func (h *Handler) hasCollection(ctx context.Context, bucket, key string) (bool, error) {
	prefix := canonicalCollectionKey(key)
	if prefix == "" {
		return true, nil
	}
	_, err := h.fs.statCollection(ctx, bucket, prefix)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, metadata.ErrNotFound) {
		return false, nil
	}
	return false, err
}

func (h *Handler) destinationKindConflict(ctx context.Context, bucket, dstKey string, srcIsCollection bool) (bool, error) {
	if srcIsCollection {
		return h.hasObject(ctx, bucket, dstKey)
	}
	return h.hasCollection(ctx, bucket, dstKey)
}

func (h *Handler) writeCopyMoveError(w http.ResponseWriter, err error) {
	if strings.Contains(err.Error(), "already exists") {
		http.Error(w, err.Error(), http.StatusPreconditionFailed)
		return
	}
	if errors.Is(err, metadata.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func (h *Handler) writeCreatedStatus(w http.ResponseWriter, created bool) {
	if created {
		w.WriteHeader(http.StatusCreated)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) resolveDestination(destination string, r *http.Request) (string, error) {
	if strings.TrimSpace(destination) == "" {
		return "", errors.New("missing Destination")
	}
	u, err := url.Parse(destination)
	if err != nil {
		return "", err
	}
	if u.Host != "" && u.Host != r.Host {
		return "", webdav.ErrForbidden
	}
	if u.Scheme != "" && u.Host == "" {
		return "", errors.New("malformed Destination")
	}
	if !strings.HasPrefix(u.Path, h.prefix) {
		return "", fmt.Errorf("destination must start with %s", h.prefix)
	}
	return "/" + strings.TrimPrefix(u.Path, h.prefix), nil
}

func parseOverwrite(value string) (bool, error) {
	if value == "" || value == "T" {
		return true, nil
	}
	if value == "F" {
		return false, nil
	}
	return false, fmt.Errorf("invalid Overwrite header: %s", value)
}

func isSubpath(parent, child string) bool {
	return child != parent && strings.HasPrefix(child, parent)
}
