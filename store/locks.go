package store

import "sync"

type keyedMutex struct {
	mu   sync.Mutex
	refs int
}

type KeyedLocker struct {
	mu    sync.Mutex
	locks map[string]*keyedMutex
}

func NewKeyedLocker() *KeyedLocker {
	return &KeyedLocker{locks: make(map[string]*keyedMutex)}
}

func (l *KeyedLocker) Lock(bucket, key string) func() {
	composite := bucket + "\x00" + key

	l.mu.Lock()
	m := l.locks[composite]
	if m == nil {
		m = &keyedMutex{}
		l.locks[composite] = m
	}
	m.refs++
	l.mu.Unlock()

	m.mu.Lock()
	return func() {
		m.mu.Unlock()

		l.mu.Lock()
		m.refs--
		if m.refs == 0 && l.locks[composite] == m {
			delete(l.locks, composite)
		}
		l.mu.Unlock()
	}
}
