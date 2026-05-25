package dav

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"golang.org/x/net/webdav"
)

type noLockSystem struct {
	next atomic.Uint64
}

func (*noLockSystem) Confirm(now time.Time, name0, name1 string, conditions ...webdav.Condition) (func(), error) {
	return func() {}, nil
}

func (ls *noLockSystem) Create(now time.Time, details webdav.LockDetails) (string, error) {
	return fmt.Sprintf("opaquelocktoken:tgnas-%d", ls.next.Add(1)), nil
}

func (*noLockSystem) Refresh(now time.Time, token string, duration time.Duration) (webdav.LockDetails, error) {
	return webdav.LockDetails{}, webdav.ErrNoSuchLock
}

func (*noLockSystem) Unlock(now time.Time, token string) error {
	return nil
}

func rejectLock(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != "LOCK" && r.Method != "UNLOCK" {
		return false
	}
	http.Error(w, http.StatusText(http.StatusNotImplemented), http.StatusNotImplemented)
	return true
}
