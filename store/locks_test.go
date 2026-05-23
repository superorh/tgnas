package store

import (
	"testing"
	"time"
)

func TestKeyedLockerSerializesSameKey(t *testing.T) {
	locker := NewKeyedLocker()
	unlock1 := locker.Lock("bucket", "key")

	acquired := make(chan struct{})
	go func() {
		unlock2 := locker.Lock("bucket", "key")
		close(acquired)
		unlock2()
	}()

	select {
	case <-acquired:
		t.Fatal("second lock acquired before first release")
	case <-time.After(50 * time.Millisecond):
	}

	unlock1()

	select {
	case <-acquired:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("second lock did not acquire after release")
	}
}

func TestKeyedLockerAllowsDifferentKeys(t *testing.T) {
	locker := NewKeyedLocker()
	unlock1 := locker.Lock("bucket", "key-1")
	defer unlock1()

	acquired := make(chan struct{})
	go func() {
		unlock2 := locker.Lock("bucket", "key-2")
		close(acquired)
		unlock2()
	}()

	select {
	case <-acquired:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("lock for different key waited unexpectedly")
	}
}

func TestKeyedLockerCleansUpAfterUnlock(t *testing.T) {
	locker := NewKeyedLocker()
	unlock := locker.Lock("bucket", "key")

	if got := len(locker.locks); got != 1 {
		t.Fatalf("len(locks) before unlock = %d, want 1", got)
	}

	unlock()

	if got := len(locker.locks); got != 0 {
		t.Fatalf("len(locks) after unlock = %d, want 0", got)
	}
}

func TestKeyedLockerKeepsMutexWhileWaiterPending(t *testing.T) {
	locker := NewKeyedLocker()
	unlock1 := locker.Lock("bucket", "key")

	acquired := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		unlock2 := locker.Lock("bucket", "key")
		close(acquired)
		unlock2()
	}()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		locker.mu.Lock()
		entry := locker.locks["bucket\x00key"]
		refs := 0
		if entry != nil {
			refs = entry.refs
		}
		locker.mu.Unlock()

		if refs == 2 {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	locker.mu.Lock()
	entry := locker.locks["bucket\x00key"]
	refs := 0
	if entry != nil {
		refs = entry.refs
	}
	locker.mu.Unlock()

	if refs != 2 {
		t.Fatalf("refs while waiter pending = %d, want 2", refs)
	}

	unlock1()

	select {
	case <-acquired:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("waiting lock did not acquire after release")
	}

	select {
	case <-finished:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("waiting lock did not release after acquire")
	}

	if got := len(locker.locks); got != 0 {
		t.Fatalf("len(locks) after waiter release = %d, want 0", got)
	}
}
