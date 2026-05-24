package dav

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aahl/tgnas/metadata"
	"golang.org/x/net/webdav"
)

func openTestHandler(t *testing.T) (*Handler, metadata.Store, fakeObjectStore) {
	t.Helper()
	fs, meta, objectStore := openTestFS(t)
	handler := NewHandler(meta, fs, HandlerOptions{Prefix: "/dav/", Credentials: map[string]string{"admin": "secret"}})
	return handler, meta, objectStore
}

func serveDAV(handler http.Handler, method, target string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	req.SetBasicAuth("admin", "secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

type panicOnDoubleWriteHeaderRecorder struct {
	*httptest.ResponseRecorder
	wroteHeader bool
}

func newPanicOnDoubleWriteHeaderRecorder() *panicOnDoubleWriteHeaderRecorder {
	return &panicOnDoubleWriteHeaderRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (r *panicOnDoubleWriteHeaderRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		panic("superfluous WriteHeader")
	}
	r.wroteHeader = true
	r.ResponseRecorder.WriteHeader(code)
}

func (r *panicOnDoubleWriteHeaderRecorder) Write(p []byte) (int, error) {
	r.wroteHeader = true
	return r.ResponseRecorder.Write(p)
}

type statFailMetadataStore struct {
	metadata.Store
	failBucket string
	failKey    string
}

func (s statFailMetadataStore) HeadObject(ctx context.Context, bucket, key string) (metadata.Object, error) {
	if bucket == s.failBucket && key == s.failKey {
		return metadata.Object{}, errors.New("injected stat failure")
	}
	return s.Store.HeadObject(ctx, bucket, key)
}

func TestHandlerRequiresBasicAuth(t *testing.T) {
	handler, _, _ := openTestHandler(t)
	req := httptest.NewRequest(http.MethodOptions, "/dav/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("missing WWW-Authenticate header")
	}
}

func TestHandlerRejectsWrongBasicAuth(t *testing.T) {
	handler, _, _ := openTestHandler(t)
	for _, tc := range []struct {
		name string
		user string
		pass string
	}{
		{name: "known user wrong password", user: "admin", pass: "wrong"},
		{name: "unknown user empty password", user: "intruder", pass: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodOptions, "/dav/", nil)
			req.SetBasicAuth(tc.user, tc.pass)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestHandlerRejectsPathsOutsidePrefix(t *testing.T) {
	handler, _, _ := openTestHandler(t)
	rec := serveDAV(handler, http.MethodOptions, "/photos/", nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHandlerOptionsAdvertisesLocks(t *testing.T) {
	handler, meta, _ := openTestHandler(t)
	seedDAVBucket(t, meta, "photos", true)
	rec := serveDAV(handler, http.MethodOptions, "/dav/photos/", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	allow := rec.Header().Get("Allow")
	for _, method := range []string{"LOCK", "UNLOCK"} {
		if !strings.Contains(allow, method) {
			t.Fatalf("Allow = %q, want %s", allow, method)
		}
	}
}

func TestHandlerOptionsAdvertisesCollectionWrites(t *testing.T) {
	handler, meta, _ := openTestHandler(t)
	seedDAVBucket(t, meta, "photos", true)
	rec := serveDAV(handler, http.MethodOptions, "/dav/photos/", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	allow := rec.Header().Get("Allow")
	for _, method := range []string{http.MethodPut, "MKCOL"} {
		if !strings.Contains(allow, method) {
			t.Fatalf("Allow = %q, want %s", allow, method)
		}
	}
	if dav := rec.Header().Get("DAV"); !strings.Contains(dav, "2") {
		t.Fatalf("DAV = %q, want class 2", dav)
	}
}

func TestHandlerLockPutUnlockWritesObject(t *testing.T) {
	handler, meta, objectStore := openTestHandler(t)
	seedDAVBucket(t, meta, "photos", true)

	lockBody := `<D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype><D:owner>test</D:owner></D:lockinfo>`
	lockReq := httptest.NewRequest("LOCK", "/dav/photos/a.txt", strings.NewReader(lockBody))
	lockReq.SetBasicAuth("admin", "secret")
	lockReq.Header.Set("Depth", "0")
	lockRec := httptest.NewRecorder()
	handler.ServeHTTP(lockRec, lockReq)
	if lockRec.Code != http.StatusCreated {
		t.Fatalf("LOCK status = %d, want 201; body=%s", lockRec.Code, lockRec.Body.String())
	}
	lockToken := lockRec.Header().Get("Lock-Token")
	if lockToken == "" {
		t.Fatal("LOCK response missing Lock-Token")
	}

	putReq := httptest.NewRequest(http.MethodPut, "/dav/photos/a.txt", strings.NewReader("hello"))
	putReq.SetBasicAuth("admin", "secret")
	putReq.Header.Set("If", "("+lockToken+")")
	putRec := httptest.NewRecorder()
	handler.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d, want 201; body=%s", putRec.Code, putRec.Body.String())
	}
	if got := objectStore.objects["photos/a.txt"]; got != "hello" {
		t.Fatalf("stored body = %q, want hello", got)
	}

	unlockReq := httptest.NewRequest("UNLOCK", "/dav/photos/a.txt", nil)
	unlockReq.SetBasicAuth("admin", "secret")
	unlockReq.Header.Set("Lock-Token", lockToken)
	unlockRec := httptest.NewRecorder()
	handler.ServeHTTP(unlockRec, unlockReq)
	if unlockRec.Code != http.StatusNoContent {
		t.Fatalf("UNLOCK status = %d, want 204; body=%s", unlockRec.Code, unlockRec.Body.String())
	}
}

func TestHandlerPropfindListsBuckets(t *testing.T) {
	handler, meta, _ := openTestHandler(t)
	seedDAVBucket(t, meta, "photos", true)
	seedDAVBucket(t, meta, "archive", false)

	req := httptest.NewRequest("PROPFIND", "/dav/", strings.NewReader(`<?xml version="1.0"?><propfind xmlns="DAV:"><allprop/></propfind>`))
	req.SetBasicAuth("admin", "secret")
	req.Header.Set("Depth", "1")
	rec := newPanicOnDoubleWriteHeaderRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != webdav.StatusMulti {
		t.Fatalf("status = %d, want 207; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/dav/photos") || strings.Contains(body, "/dav/archive") {
		t.Fatalf("body = %s", body)
	}
}

func TestHandlerPropfindDepthInfinityForbidden(t *testing.T) {
	handler, meta, _ := openTestHandler(t)
	seedDAVBucket(t, meta, "photos", true)

	req := httptest.NewRequest("PROPFIND", "/dav/photos/", strings.NewReader(`<?xml version="1.0"?><propfind xmlns="DAV:"><allprop/></propfind>`))
	req.SetBasicAuth("admin", "secret")
	req.Header.Set("Depth", "infinity")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestHandlerPropfindMissingFileDoesNotLogError(t *testing.T) {
	fs, meta, _ := openTestFS(t)
	seedDAVBucket(t, meta, "photos", true)
	var logs bytes.Buffer
	handler := NewHandler(meta, fs, HandlerOptions{Prefix: "/dav/", Credentials: map[string]string{"admin": "secret"}, Logger: log.New(&logs, "", 0)})

	req := httptest.NewRequest("PROPFIND", "/dav/photos/missing.txt", strings.NewReader(`<?xml version="1.0"?><propfind xmlns="DAV:"><allprop/></propfind>`))
	req.SetBasicAuth("admin", "secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if logs.Len() != 0 {
		t.Fatalf("logs = %q, want empty", logs.String())
	}
}

func TestHandlerPropfindSkipsInconsistentChildWithoutDoubleWriteHeader(t *testing.T) {
	ctx := context.Background()
	base, err := metadata.OpenSQLite(t.TempDir() + "/metadata.sqlite")
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	t.Cleanup(func() { _ = base.Close() })
	seedDAVBucket(t, base, "photos", true)
	seedDAVObject(t, base, "photos", "ok.txt", 1)
	seedDAVObject(t, base, "photos", "broken.txt", 1)
	meta := statFailMetadataStore{Store: base, failBucket: "photos", failKey: "broken.txt"}
	fs := NewFileSystem(meta, fakeObjectStore{objects: map[string]string{"photos/ok.txt": "x", "photos/broken.txt": "x"}})
	handler := NewHandler(meta, fs, HandlerOptions{Prefix: "/dav/", Credentials: map[string]string{"admin": "secret"}})

	req := httptest.NewRequest("PROPFIND", "/dav/photos/", strings.NewReader(`<?xml version="1.0"?><propfind xmlns="DAV:"><allprop/></propfind>`))
	req.SetBasicAuth("admin", "secret")
	req.Header.Set("Depth", "1")
	rec := newPanicOnDoubleWriteHeaderRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != webdav.StatusMulti {
		t.Fatalf("status = %d, want 207; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/dav/photos/ok.txt") || strings.Contains(body, "/dav/photos/broken.txt") {
		t.Fatalf("body = %s", body)
	}
	if _, err := meta.HeadObject(ctx, "photos", "broken.txt"); err == nil {
		t.Fatal("fault injection should still fail HeadObject for broken child")
	}
}

func TestHandlerWebDAVDelegatedMethodsDoNotWriteHeaderTwice(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		target string
		body   string
		setup  func(t *testing.T, meta metadata.Store)
	}{
		{name: "propfind root", method: "PROPFIND", target: "/dav/", body: `<?xml version="1.0"?><propfind xmlns="DAV:"><allprop/></propfind>`, setup: func(t *testing.T, meta metadata.Store) {
			seedDAVBucket(t, meta, "photos", true)
		}},
		{name: "propfind collection", method: "PROPFIND", target: "/dav/photos/", body: `<?xml version="1.0"?><propfind xmlns="DAV:"><allprop/></propfind>`, setup: func(t *testing.T, meta metadata.Store) {
			seedDAVBucket(t, meta, "photos", true)
			seedDAVObject(t, meta, "photos", "dir/file.txt", 1)
		}},
		{name: "proppatch collection", method: "PROPPATCH", target: "/dav/photos/", body: `<?xml version="1.0"?><propertyupdate xmlns="DAV:"></propertyupdate>`, setup: func(t *testing.T, meta metadata.Store) {
			seedDAVBucket(t, meta, "photos", true)
		}},
		{name: "delete object", method: http.MethodDelete, target: "/dav/photos/a.txt", setup: func(t *testing.T, meta metadata.Store) {
			seedDAVBucket(t, meta, "photos", true)
			seedDAVObject(t, meta, "photos", "a.txt", 1)
		}},
		{name: "put object", method: http.MethodPut, target: "/dav/photos/a.txt", body: "hello", setup: func(t *testing.T, meta metadata.Store) {
			seedDAVBucket(t, meta, "photos", true)
		}},
		{name: "mkcol", method: "MKCOL", target: "/dav/photos/dir/", setup: func(t *testing.T, meta metadata.Store) {
			seedDAVBucket(t, meta, "photos", true)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, meta, _ := openTestHandler(t)
			if tc.setup != nil {
				tc.setup(t, meta)
			}
			req := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
			req.SetBasicAuth("admin", "secret")
			if tc.method == "PROPFIND" {
				req.Header.Set("Depth", "1")
			}
			rec := newPanicOnDoubleWriteHeaderRecorder()
			handler.ServeHTTP(rec, req)
		})
	}
}

func TestHandlerGetReadsObject(t *testing.T) {
	handler, meta, objectStore := openTestHandler(t)
	seedDAVBucket(t, meta, "photos", true)
	seedDAVObject(t, meta, "photos", "a.txt", 5)
	objectStore.objects["photos/a.txt"] = "hello"

	rec := serveDAV(handler, http.MethodGet, "/dav/photos/a.txt", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "hello" {
		t.Fatalf("body = %q, want hello", rec.Body.String())
	}
}

func TestHandlerPutWritesObject(t *testing.T) {
	handler, meta, objectStore := openTestHandler(t)
	seedDAVBucket(t, meta, "photos", true)
	rec := serveDAV(handler, http.MethodPut, "/dav/photos/a.txt", strings.NewReader("hello"))

	if rec.Code != http.StatusCreated && rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 201 or 204; body=%s", rec.Code, rec.Body.String())
	}
	if got := objectStore.objects["photos/a.txt"]; got != "hello" {
		t.Fatalf("stored body = %q, want hello", got)
	}
}

func TestHandlerMkdirCreatesDirectoryMarker(t *testing.T) {
	handler, meta, _ := openTestHandler(t)
	ctx := context.Background()
	seedDAVBucket(t, meta, "photos", true)
	rec := serveDAV(handler, "MKCOL", "/dav/photos/2026/", nil)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := meta.HeadObject(ctx, "photos", "2026/"); err != nil {
		t.Fatalf("HeadObject returned error: %v", err)
	}
}

func TestHandlerDeleteOrphanBucket(t *testing.T) {
	handler, meta, _ := openTestHandler(t)
	ctx := context.Background()
	seedDAVBucket(t, meta, "archive", false)
	seedDAVObject(t, meta, "archive", "old.txt", 1)
	rec := serveDAV(handler, http.MethodDelete, "/dav/archive/", nil)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := meta.GetBucket(ctx, "archive"); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatalf("GetBucket err = %v, want ErrNotFound", err)
	}
}

func TestHandlerForbidsNonDeleteOrphanAccess(t *testing.T) {
	handler, meta, _ := openTestHandler(t)
	seedDAVBucket(t, meta, "archive", false)
	rec := serveDAV(handler, "PROPFIND", "/dav/archive/", nil)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestHandlerPropfindWithoutDepthListsDirectChildrenOnly(t *testing.T) {
	handler, meta, _ := openTestHandler(t)
	seedDAVBucket(t, meta, "photos", true)
	seedDAVObject(t, meta, "photos", "2026/jan/a.txt", 1)

	req := httptest.NewRequest("PROPFIND", "/dav/photos/", strings.NewReader(`<?xml version="1.0"?><propfind xmlns="DAV:"><allprop/></propfind>`))
	req.SetBasicAuth("admin", "secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != webdav.StatusMulti {
		t.Fatalf("status = %d, want 207; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/dav/photos/2026/") {
		t.Fatalf("body missing direct child collection: %s", body)
	}
	if strings.Contains(body, "/dav/photos/2026/jan/") || strings.Contains(body, "/dav/photos/2026/jan/a.txt") {
		t.Fatalf("body includes recursive descendants: %s", body)
	}
}

func TestHandlerMkdirBucketRootForbiddenForUnknownBucket(t *testing.T) {
	handler, _, _ := openTestHandler(t)
	rec := serveDAV(handler, "MKCOL", "/dav/missing", nil)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlerCopyMoveMissingDirectoryReturnsNotFound(t *testing.T) {
	for _, method := range []string{"COPY", "MOVE"} {
		t.Run(method, func(t *testing.T) {
			handler, meta, _ := openTestHandler(t)
			seedDAVBucket(t, meta, "photos", true)

			req := httptest.NewRequest(method, "/dav/photos/missing/", nil)
			req.SetBasicAuth("admin", "secret")
			req.Header.Set("Destination", "/dav/photos/dst/")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandlerMoveSamePathRejectedWithoutDeletingSource(t *testing.T) {
	for _, tc := range []struct {
		name        string
		target      string
		destination string
	}{
		{name: "file", target: "/dav/photos/a.txt", destination: "/dav/photos/a.txt"},
		{name: "directory", target: "/dav/photos/dir/", destination: "/dav/photos/dir/"},
		{name: "canonical directory", target: "/dav/photos/dir", destination: "/dav/photos/dir/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, meta, _ := openTestHandler(t)
			ctx := context.Background()
			seedDAVBucket(t, meta, "photos", true)
			seedDAVObject(t, meta, "photos", "a.txt", 1)
			seedDAVObject(t, meta, "photos", "dir/file.txt", 1)

			req := httptest.NewRequest("MOVE", tc.target, nil)
			req.SetBasicAuth("admin", "secret")
			req.Header.Set("Destination", tc.destination)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
			}
			if _, err := meta.HeadObject(ctx, "photos", "a.txt"); err != nil {
				t.Fatalf("a.txt missing after rejected move: %v", err)
			}
			if _, err := meta.HeadObject(ctx, "photos", "dir/file.txt"); err != nil {
				t.Fatalf("dir/file.txt missing after rejected move: %v", err)
			}
		})
	}
}

func TestHandlerCopyMoveRejectsDestinationKindCollisions(t *testing.T) {
	for _, tc := range []struct {
		name        string
		method      string
		source      string
		destination string
	}{
		{name: "copy file to existing directory", method: "COPY", source: "/dav/photos/file.txt", destination: "/dav/photos/dir"},
		{name: "move file to existing directory", method: "MOVE", source: "/dav/photos/file.txt", destination: "/dav/photos/dir"},
		{name: "copy directory to existing file", method: "COPY", source: "/dav/photos/src/", destination: "/dav/photos/file.txt"},
		{name: "move directory to existing file", method: "MOVE", source: "/dav/photos/src/", destination: "/dav/photos/file.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, meta, _ := openTestHandler(t)
			ctx := context.Background()
			seedDAVBucket(t, meta, "photos", true)
			seedDAVObject(t, meta, "photos", "file.txt", 1)
			seedDAVObject(t, meta, "photos", "dir/child.txt", 1)
			seedDAVObject(t, meta, "photos", "src/child.txt", 1)

			req := httptest.NewRequest(tc.method, tc.source, nil)
			req.SetBasicAuth("admin", "secret")
			req.Header.Set("Destination", tc.destination)
			req.Header.Set("Overwrite", "F")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusPreconditionFailed {
				t.Fatalf("status = %d, want 412; body=%s", rec.Code, rec.Body.String())
			}
			if _, err := meta.HeadObject(ctx, "photos", "file.txt"); err != nil {
				t.Fatalf("file.txt missing after rejected operation: %v", err)
			}
			if _, err := meta.HeadObject(ctx, "photos", "dir/child.txt"); err != nil {
				t.Fatalf("dir/child.txt missing after rejected operation: %v", err)
			}
			if _, err := meta.HeadObject(ctx, "photos", "src/child.txt"); err != nil {
				t.Fatalf("src/child.txt missing after rejected operation: %v", err)
			}
		})
	}
}

func TestHandlerPutRejectsWhenDirectoryExists(t *testing.T) {
	handler, meta, _ := openTestHandler(t)
	seedDAVBucket(t, meta, "photos", true)
	seedDAVObject(t, meta, "photos", "dir/child.txt", 1)

	rec := serveDAV(handler, http.MethodPut, "/dav/photos/dir", strings.NewReader("data"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlerMkdirRejectsWhenCollectionAlreadyExists(t *testing.T) {
	handler, meta, _ := openTestHandler(t)
	seedDAVBucket(t, meta, "photos", true)
	seedDAVObject(t, meta, "photos", "dir/child.txt", 1)

	rec := serveDAV(handler, "MKCOL", "/dav/photos/dir/", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlerMkdirRejectsWhenObjectExists(t *testing.T) {
	handler, meta, _ := openTestHandler(t)
	seedDAVBucket(t, meta, "photos", true)
	seedDAVObject(t, meta, "photos", "dir", 5)

	rec := serveDAV(handler, "MKCOL", "/dav/photos/dir/", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlerCopyObjectSameBucketMetadataOnly(t *testing.T) {
	handler, meta, _ := openTestHandler(t)
	ctx := context.Background()
	seedDAVBucket(t, meta, "photos", true)
	seedDAVObject(t, meta, "photos", "a.txt", 5)

	req := httptest.NewRequest("COPY", "/dav/photos/a.txt", nil)
	req.SetBasicAuth("admin", "secret")
	req.Header.Set("Destination", "http://example.com/dav/photos/b.txt")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := meta.HeadObject(ctx, "photos", "b.txt"); err != nil {
		t.Fatalf("destination missing: %v", err)
	}
}

func TestHandlerCopyPrefersObjectWhenObjectAndCollectionSharePath(t *testing.T) {
	handler, meta, _ := openTestHandler(t)
	ctx := context.Background()
	seedDAVBucket(t, meta, "photos", true)
	seedDAVObject(t, meta, "photos", "foo", 7)
	seedDAVObject(t, meta, "photos", "foo/bar.txt", 1)

	req := httptest.NewRequest("COPY", "/dav/photos/foo", nil)
	req.SetBasicAuth("admin", "secret")
	req.Header.Set("Destination", "/dav/photos/copied")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := meta.HeadObject(ctx, "photos", "copied"); err != nil {
		t.Fatalf("copied object missing: %v", err)
	}
	if _, err := meta.HeadObject(ctx, "photos", "copied/bar.txt"); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatalf("copied collection child err = %v, want ErrNotFound", err)
	}
}

func TestHandlerMovePrefersObjectWhenObjectAndCollectionSharePath(t *testing.T) {
	handler, meta, _ := openTestHandler(t)
	ctx := context.Background()
	seedDAVBucket(t, meta, "photos", true)
	seedDAVObject(t, meta, "photos", "foo", 7)
	seedDAVObject(t, meta, "photos", "foo/bar.txt", 1)

	req := httptest.NewRequest("MOVE", "/dav/photos/foo", nil)
	req.SetBasicAuth("admin", "secret")
	req.Header.Set("Destination", "/dav/photos/moved")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := meta.HeadObject(ctx, "photos", "moved"); err != nil {
		t.Fatalf("moved object missing: %v", err)
	}
	if _, err := meta.HeadObject(ctx, "photos", "foo"); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatalf("source object err = %v, want ErrNotFound", err)
	}
	if _, err := meta.HeadObject(ctx, "photos", "foo/bar.txt"); err != nil {
		t.Fatalf("source collection child missing: %v", err)
	}
}

func TestHandlerMoveDirectorySameBucket(t *testing.T) {
	handler, meta, _ := openTestHandler(t)
	ctx := context.Background()
	seedDAVBucket(t, meta, "photos", true)
	seedDAVObject(t, meta, "photos", "src/a.txt", 1)
	seedDAVObject(t, meta, "photos", "src/nested/b.txt", 1)

	req := httptest.NewRequest("MOVE", "/dav/photos/src/", nil)
	req.SetBasicAuth("admin", "secret")
	req.Header.Set("Destination", "/dav/photos/dst/")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := meta.HeadObject(ctx, "photos", "dst/a.txt"); err != nil {
		t.Fatalf("moved destination missing: %v", err)
	}
	if _, err := meta.HeadObject(ctx, "photos", "src/a.txt"); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatalf("source err = %v, want ErrNotFound", err)
	}
}

func TestHandlerCopyRejectsCrossBucketAndOverwriteFalse(t *testing.T) {
	handler, meta, _ := openTestHandler(t)
	seedDAVBucket(t, meta, "photos", true)
	seedDAVBucket(t, meta, "other", true)
	seedDAVObject(t, meta, "photos", "a.txt", 1)
	seedDAVObject(t, meta, "photos", "exists.txt", 1)

	cross := httptest.NewRequest("COPY", "/dav/photos/a.txt", nil)
	cross.SetBasicAuth("admin", "secret")
	cross.Header.Set("Destination", "/dav/other/a.txt")
	crossRec := httptest.NewRecorder()
	handler.ServeHTTP(crossRec, cross)
	if crossRec.Code != http.StatusForbidden {
		t.Fatalf("cross status = %d, want 403", crossRec.Code)
	}

	noOverwrite := httptest.NewRequest("COPY", "/dav/photos/a.txt", nil)
	noOverwrite.SetBasicAuth("admin", "secret")
	noOverwrite.Header.Set("Destination", "/dav/photos/exists.txt")
	noOverwrite.Header.Set("Overwrite", "F")
	noOverwriteRec := httptest.NewRecorder()
	handler.ServeHTTP(noOverwriteRec, noOverwrite)
	if noOverwriteRec.Code != http.StatusPreconditionFailed {
		t.Fatalf("overwrite status = %d, want 412", noOverwriteRec.Code)
	}
}

func TestHandlerRejectsMalformedDestination(t *testing.T) {
	handler, meta, _ := openTestHandler(t)
	seedDAVBucket(t, meta, "photos", true)
	seedDAVObject(t, meta, "photos", "a.txt", 1)

	for _, destination := range []string{"http://other.example/dav/photos/b.txt", "/s3/photos/b.txt", "http://%zz"} {
		req := httptest.NewRequest("COPY", "/dav/photos/a.txt", nil)
		req.SetBasicAuth("admin", "secret")
		req.Header.Set("Destination", destination)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusForbidden {
			t.Fatalf("destination %q status = %d, want 400 or 403", destination, rec.Code)
		}
	}
}

func TestHandlerRejectsCopyIntoOwnSubtree(t *testing.T) {
	handler, meta, _ := openTestHandler(t)
	seedDAVBucket(t, meta, "photos", true)
	seedDAVObject(t, meta, "photos", "src/a.txt", 1)

	req := httptest.NewRequest("COPY", "/dav/photos/src/", nil)
	req.SetBasicAuth("admin", "secret")
	req.Header.Set("Destination", "/dav/photos/src/child/")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestHandlerRejectsInvalidOverwrite(t *testing.T) {
	handler, meta, _ := openTestHandler(t)
	seedDAVBucket(t, meta, "photos", true)
	seedDAVObject(t, meta, "photos", "a.txt", 1)

	req := httptest.NewRequest("COPY", "/dav/photos/a.txt", nil)
	req.SetBasicAuth("admin", "secret")
	req.Header.Set("Destination", "/dav/photos/b.txt")
	req.Header.Set("Overwrite", "invalid")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
