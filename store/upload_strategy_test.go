package store

import "testing"

func TestDocumentStrategyAlwaysUsesDocumentWithinLimit(t *testing.T) {
	resolver := NewUploadStrategyResolver(DefaultUploadConfig())

	strategy, err := resolver.Resolve("photo.jpg", "image/jpeg", 1024)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}

	if strategy.TelegramType != "document" {
		t.Fatalf("TelegramType = %q, want document", strategy.TelegramType)
	}
	if strategy.UploadStrategy != "document" {
		t.Fatalf("UploadStrategy = %q, want document", strategy.UploadStrategy)
	}
	if strategy.Chunked {
		t.Fatal("Chunked = true, want false")
	}
}

func TestAutoStrategyInfersPhoto(t *testing.T) {
	config := DefaultUploadConfig()
	config.Strategy = "auto"
	resolver := NewUploadStrategyResolver(config)

	strategy, err := resolver.Resolve("photo.jpg", "image/jpeg", 1024)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}

	if strategy.TelegramType != "photo" {
		t.Fatalf("TelegramType = %q, want photo", strategy.TelegramType)
	}
	if strategy.UploadStrategy != "typed" {
		t.Fatalf("UploadStrategy = %q, want typed", strategy.UploadStrategy)
	}
}

func TestAutoStrategyUsesAnimationForGIF(t *testing.T) {
	config := DefaultUploadConfig()
	config.Strategy = "auto"
	resolver := NewUploadStrategyResolver(config)

	strategy, err := resolver.Resolve("anim.gif", "image/gif", 1024)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}

	if strategy.TelegramType != "animation" {
		t.Fatalf("TelegramType = %q, want animation", strategy.TelegramType)
	}
	if strategy.UploadStrategy != "typed" {
		t.Fatalf("UploadStrategy = %q, want typed", strategy.UploadStrategy)
	}
}

func TestAutoStrategyInfersFromExtensionWhenContentTypeEmpty(t *testing.T) {
	config := DefaultUploadConfig()
	config.Strategy = "auto"
	resolver := NewUploadStrategyResolver(config)

	strategy, err := resolver.Resolve("clip.mp4", "", 1024)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}

	if strategy.TelegramType != "video" {
		t.Fatalf("TelegramType = %q, want video", strategy.TelegramType)
	}
	if strategy.UploadStrategy != "typed" {
		t.Fatalf("UploadStrategy = %q, want typed", strategy.UploadStrategy)
	}
}

func TestAutoStrategyUsesVideoForVideoMIME(t *testing.T) {
	config := DefaultUploadConfig()
	config.Strategy = "auto"
	resolver := NewUploadStrategyResolver(config)

	strategy, err := resolver.Resolve("clip.bin", "video/mp4", 1024)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}

	if strategy.TelegramType != "video" {
		t.Fatalf("TelegramType = %q, want video", strategy.TelegramType)
	}
	if strategy.UploadStrategy != "typed" {
		t.Fatalf("UploadStrategy = %q, want typed", strategy.UploadStrategy)
	}
}

func TestAutoStrategyUsesAudioForAudioMIME(t *testing.T) {
	config := DefaultUploadConfig()
	config.Strategy = "auto"
	resolver := NewUploadStrategyResolver(config)

	strategy, err := resolver.Resolve("track.bin", "audio/mpeg", 1024)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}

	if strategy.TelegramType != "audio" {
		t.Fatalf("TelegramType = %q, want audio", strategy.TelegramType)
	}
	if strategy.UploadStrategy != "typed" {
		t.Fatalf("UploadStrategy = %q, want typed", strategy.UploadStrategy)
	}
}

func TestAutoStrategyDoesNotInferFromExtensionWhenContentTypeProvided(t *testing.T) {
	config := DefaultUploadConfig()
	config.Strategy = "auto"
	resolver := NewUploadStrategyResolver(config)

	strategy, err := resolver.Resolve("clip.mp4", "application/octet-stream", 1024)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}

	if strategy.TelegramType != "document" {
		t.Fatalf("TelegramType = %q, want document", strategy.TelegramType)
	}
	if strategy.UploadStrategy != "document" {
		t.Fatalf("UploadStrategy = %q, want document", strategy.UploadStrategy)
	}
}

func TestAutoStrategyFallsBackToDocumentForUnknownMIME(t *testing.T) {
	config := DefaultUploadConfig()
	config.Strategy = "auto"
	resolver := NewUploadStrategyResolver(config)

	strategy, err := resolver.Resolve("payload.bin", "application/octet-stream", 1024)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}

	if strategy.TelegramType != "document" {
		t.Fatalf("TelegramType = %q, want document", strategy.TelegramType)
	}
	if strategy.UploadStrategy != "document" {
		t.Fatalf("UploadStrategy = %q, want document", strategy.UploadStrategy)
	}
}

func TestAutoStrategyFallsBackToDocument(t *testing.T) {
	config := DefaultUploadConfig()
	config.Strategy = "auto"
	resolver := NewUploadStrategyResolver(config)

	size := config.TypeLimits["photo"] + 1
	strategy, err := resolver.Resolve("photo.jpg", "image/jpeg", size)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}

	if strategy.TelegramType != "document" {
		t.Fatalf("TelegramType = %q, want document", strategy.TelegramType)
	}
	if strategy.UploadStrategy != "document" {
		t.Fatalf("UploadStrategy = %q, want document", strategy.UploadStrategy)
	}
	if strategy.Chunked {
		t.Fatal("Chunked = true, want false")
	}
}

func TestResolverChunksLargeDocument(t *testing.T) {
	resolver := NewUploadStrategyResolver(DefaultUploadConfig())
	config := DefaultUploadConfig()

	size := config.TypeLimits["document"] + 1
	strategy, err := resolver.Resolve("archive.bin", "application/octet-stream", size)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}

	if strategy.TelegramType != "document" {
		t.Fatalf("TelegramType = %q, want document", strategy.TelegramType)
	}
	if strategy.UploadStrategy != "chunked_document" {
		t.Fatalf("UploadStrategy = %q, want chunked_document", strategy.UploadStrategy)
	}
	if !strategy.Chunked {
		t.Fatal("Chunked = false, want true")
	}
	if strategy.ChunkSize != config.ChunkSize {
		t.Fatalf("ChunkSize = %d, want %d", strategy.ChunkSize, config.ChunkSize)
	}
}

func TestResolverUsesDefaultChunkSizeWhenConfiguredChunkSizeIsNonPositive(t *testing.T) {
	config := DefaultUploadConfig()
	config.ChunkSize = 0
	resolver := NewUploadStrategyResolver(config)

	size := config.TypeLimits["document"] + 1
	strategy, err := resolver.Resolve("archive.bin", "application/octet-stream", size)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}

	if !strategy.Chunked {
		t.Fatal("Chunked = false, want true")
	}
	if strategy.ChunkSize != DefaultUploadConfig().ChunkSize {
		t.Fatalf("ChunkSize = %d, want %d", strategy.ChunkSize, DefaultUploadConfig().ChunkSize)
	}
}

func TestResolverRejectsLargeFileWhenChunkingDisabled(t *testing.T) {
	config := DefaultUploadConfig()
	config.EnableChunking = false
	resolver := NewUploadStrategyResolver(config)

	_, err := resolver.Resolve("archive.bin", "application/octet-stream", config.TypeLimits["document"]+1)
	if err != ErrEntityTooLarge {
		t.Fatalf("err = %v, want %v", err, ErrEntityTooLarge)
	}
}

func TestResolverRejectsFileOverMaxFileSize(t *testing.T) {
	config := DefaultUploadConfig()
	config.Strategy = "auto"
	config.MaxFileSize = 1024
	resolver := NewUploadStrategyResolver(config)

	_, err := resolver.Resolve("archive.bin", "application/octet-stream", 1025)
	if err != ErrEntityTooLarge {
		t.Fatalf("err = %v, want %v", err, ErrEntityTooLarge)
	}
}
