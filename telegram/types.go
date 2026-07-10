package telegram

import (
	"context"
	"errors"
	"io"
)

const (
	TypePhoto     = "photo"
	TypeVideo     = "video"
	TypeAudio     = "audio"
	TypeAnimation = "animation"
	TypeDocument  = "document"
)

type UploadRequest struct {
	Type     string
	ChatID   string
	Reader   io.Reader
	Filename string
	MIMEType string
	Caption  string
}

type UploadedFile struct {
	Type         string
	FileID       string
	FileUniqueID string
	MessageID    int64
	FileSize     int64
	MIMEType     string
}

type Client interface {
	Upload(ctx context.Context, request UploadRequest) (UploadedFile, error)
	Download(ctx context.Context, fileID string) (io.ReadCloser, error)
}

type RequestError struct {
	Operation  string
	StatusCode int
	Reason     string
	Cause      error
}

func (e *RequestError) Error() string {
	if e == nil || e.Cause == nil {
		return "telegram request failed"
	}
	return e.Cause.Error()
}

func (e *RequestError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func NewRequestError(operation string, statusCode int, reason string, cause error) error {
	if cause == nil {
		cause = errors.New("telegram request failed")
	}
	return &RequestError{Operation: operation, StatusCode: statusCode, Reason: reason, Cause: cause}
}

func ClassifyRequestError(err error) (*RequestError, bool) {
	var target *RequestError
	if !errors.As(err, &target) {
		return nil, false
	}
	return target, true
}
