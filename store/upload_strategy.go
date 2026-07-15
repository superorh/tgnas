package store

import (
	"path/filepath"
	"strings"
)

type UploadStrategyResolver struct {
	config UploadConfig
}

type UploadStrategy struct {
	TelegramType   string
	UploadStrategy string
	Chunked        bool
	ChunkSize      int64
}

func NewUploadStrategyResolver(config UploadConfig) *UploadStrategyResolver {
	return &UploadStrategyResolver{config: config}
}

func (r *UploadStrategyResolver) Resolve(filename, contentType string, size int64) (UploadStrategy, error) {
	if r.config.MaxFileSize > 0 && size > r.config.MaxFileSize {
		return UploadStrategy{}, ErrEntityTooLarge
	}

	strategy := r.config.Strategy
	if strategy == "" {
		strategy = "document"
	}

	switch strategy {
	case "auto":
		return r.resolveAuto(filename, contentType, size)
	case "document":
		fallthrough
	default:
		return r.resolveDocument(size)
	}
}

func (r *UploadStrategyResolver) resolveAuto(filename, contentType string, size int64) (UploadStrategy, error) {
	telegramType := inferTelegramType(filename, contentType)
	if telegramType != "document" && withinLimit(size, r.config.TypeLimits[telegramType]) {
		return UploadStrategy{TelegramType: telegramType, UploadStrategy: "typed"}, nil
	}

	return r.resolveDocument(size)
}

func (r *UploadStrategyResolver) resolveDocument(size int64) (UploadStrategy, error) {
	documentLimit := r.config.TypeLimits["document"]
	if withinLimit(size, documentLimit) {
		return UploadStrategy{TelegramType: "document", UploadStrategy: "document"}, nil
	}
	if r.config.EnableChunking {
		chunkSize := r.config.ChunkSize
		if chunkSize <= 0 {
			chunkSize = DefaultUploadConfig().ChunkSize
		}

		return UploadStrategy{
			TelegramType:   "document",
			UploadStrategy: "chunked_document",
			Chunked:        true,
			ChunkSize:      chunkSize,
		}, nil
	}

	return UploadStrategy{}, ErrEntityTooLarge
}

func inferTelegramType(filename, contentType string) string {
	mediaType := strings.ToLower(strings.TrimSpace(contentType))
	switch {
	case mediaType == "image/gif":
		return "animation"
	case strings.HasPrefix(mediaType, "image/"):
		return "photo"
	case strings.HasPrefix(mediaType, "video/"):
		return "video"
	case strings.HasPrefix(mediaType, "audio/"):
		return "audio"
	case mediaType != "":
		return "document"
	}

	switch strings.ToLower(filepath.Ext(filename)) {
	case ".gif":
		return "animation"
	case ".jpg", ".jpeg", ".png", ".webp", ".bmp", ".tif", ".tiff":
		return "photo"
	case ".mp4", ".mov", ".mkv", ".avi", ".webm", ".m4v":
		return "video"
	case ".mp3", ".wav", ".flac", ".m4a", ".ogg", ".aac":
		return "audio"
	default:
		return "document"
	}
}

func withinLimit(size, limit int64) bool {
	return limit <= 0 || size <= limit
}
