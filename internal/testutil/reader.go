package testutil

import (
	"errors"
	"io"
	"strings"
)

type ErrorReader struct{ Err error }

func (r ErrorReader) Read(p []byte) (int, error) {
	if r.Err != nil {
		return 0, r.Err
	}
	return 0, errors.New("test read error")
}

func LimitedStringReader(value string, limit int64) io.Reader {
	return io.LimitReader(strings.NewReader(value), limit)
}
