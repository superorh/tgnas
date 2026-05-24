package testutil

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/aahl/tgnas/telegram"
)

type FakeTelegram struct {
	mu           sync.Mutex
	Uploads      []telegram.UploadRequest
	Downloads    []string
	Files        map[string]string
	UploadFunc   func(context.Context, telegram.UploadRequest) (telegram.UploadedFile, error)
	DownloadFunc func(context.Context, string) (io.ReadCloser, error)
}

func NewFakeTelegram() *FakeTelegram {
	return &FakeTelegram{Files: map[string]string{}}
}

func (f *FakeTelegram) Upload(ctx context.Context, request telegram.UploadRequest) (telegram.UploadedFile, error) {
	if f.UploadFunc != nil {
		return f.UploadFunc(ctx, request)
	}
	data, err := io.ReadAll(request.Reader)
	if err != nil {
		return telegram.UploadedFile{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	request.Reader = strings.NewReader(string(data))
	f.Uploads = append(f.Uploads, request)
	fileID := fmt.Sprintf("file-%d", len(f.Uploads))
	f.Files[fileID] = string(data)
	return telegram.UploadedFile{
		Type:         request.Type,
		FileID:       fileID,
		FileUniqueID: fileID + "-unique",
		MessageID:    int64(len(f.Uploads)),
		FileSize:     int64(len(data)),
		MIMEType:     request.MIMEType,
	}, nil
}

func (f *FakeTelegram) Download(ctx context.Context, fileID string) (io.ReadCloser, error) {
	if f.DownloadFunc != nil {
		return f.DownloadFunc(ctx, fileID)
	}
	f.mu.Lock()
	f.Downloads = append(f.Downloads, fileID)
	data, ok := f.Files[fileID]
	f.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("fake telegram file %q not found", fileID)
	}
	return io.NopCloser(strings.NewReader(data)), nil
}
