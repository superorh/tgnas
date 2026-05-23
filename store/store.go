package store

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/aahl/tgs3/metadata"
	"github.com/aahl/tgs3/telegram"
)

type ObjectStore struct {
	meta           metadata.Store
	tg             telegram.Client
	options        Options
	resolver       *UploadStrategyResolver
	locker         *KeyedLocker
	uploads        chan struct{}
	downloads      chan struct{}
	telegramSem    chan struct{}
	logger         *log.Logger
	startupBuckets map[string]metadata.Bucket
}

type uploadRecord struct {
	FileID    string
	MessageID int64
}

func NewObjectStore(meta metadata.Store, tg telegram.Client, options Options) (*ObjectStore, error) {
	upload := options.Upload
	if upload.Strategy == "" && upload.MaxFileSize == 0 && upload.ChunkSize == 0 && upload.TypeLimits == nil && upload.PutBufferSize == 0 {
		upload = DefaultUploadConfig()
	} else {
		defaults := DefaultUploadConfig()
		if upload.Strategy == "" {
			upload.Strategy = defaults.Strategy
		}
		if upload.MaxFileSize == 0 {
			upload.MaxFileSize = defaults.MaxFileSize
		}
		if upload.ChunkSize == 0 {
			upload.ChunkSize = defaults.ChunkSize
		}
		if upload.TypeLimits == nil {
			upload.TypeLimits = defaults.TypeLimits
		}
		if upload.PutBufferSize == 0 {
			upload.PutBufferSize = defaults.PutBufferSize
		}
	}
	options.Upload = upload

	store := &ObjectStore{
		meta:           meta,
		tg:             tg,
		options:        options,
		resolver:       NewUploadStrategyResolver(upload),
		locker:         NewKeyedLocker(),
		logger:         options.Logger,
		startupBuckets: map[string]metadata.Bucket{},
	}
	if store.logger == nil {
		store.logger = log.New(io.Discard, "", 0)
	}
	if options.MaxUploads > 0 {
		store.uploads = make(chan struct{}, options.MaxUploads)
	}
	if options.MaxDownloads > 0 {
		store.downloads = make(chan struct{}, options.MaxDownloads)
	}
	if options.MaxTelegramCalls > 0 {
		store.telegramSem = make(chan struct{}, options.MaxTelegramCalls)
	}
	if buckets, err := meta.ListBuckets(context.Background()); err == nil {
		for _, bucket := range buckets {
			if !bucket.Enabled {
				continue
			}
			store.startupBuckets[bucket.Name] = bucket
		}
	} else {
		return nil, fmt.Errorf("startup bucket snapshot: %s", sanitizeLogError(err))
	}
	return store, nil
}

func (s *ObjectStore) ListBuckets(ctx context.Context) ([]metadata.Bucket, error) {
	return s.meta.ListBuckets(ctx)
}

func (s *ObjectStore) HeadBucket(ctx context.Context, name string) error {
	if _, ok := s.startupBuckets[name]; !ok {
		return ErrNoSuchBucket
	}
	return nil
}

func (s *ObjectStore) PutObject(ctx context.Context, input PutObjectInput) (PutObjectResult, error) {
	if input.Size < 0 {
		return PutObjectResult{}, ErrMissingContentLength
	}
	if err := s.HeadBucket(ctx, input.Bucket); err != nil {
		return PutObjectResult{}, err
	}
	if input.Body == nil {
		input.Body = strings.NewReader("")
	}

	releaseUpload := s.acquire(ctx, s.uploads)
	if releaseUpload == nil {
		return PutObjectResult{}, ctx.Err()
	}
	defer releaseUpload()

	releaseLock := s.locker.Lock(input.Bucket, input.Key)
	defer releaseLock()

	strategy, err := s.resolver.Resolve(path.Base(input.Key), input.ContentType, input.Size)
	if err != nil {
		return PutObjectResult{}, err
	}

	if !strategy.Chunked {
		limit := s.options.Upload.TypeLimits[strategy.TelegramType]
		if limit > 0 && input.Size > limit {
			return PutObjectResult{}, ErrEntityTooLarge
		}
		return s.putSingle(ctx, input, strategy)
	}
	return s.putChunked(ctx, input, strategy)
}

func (s *ObjectStore) HeadObject(ctx context.Context, bucket, key string) (ObjectInfo, error) {
	if err := s.HeadBucket(ctx, bucket); err != nil {
		return ObjectInfo{}, err
	}
	object, err := s.meta.HeadObject(ctx, bucket, key)
	if err != nil {
		if err == metadata.ErrNotFound {
			return ObjectInfo{}, ErrNoSuchKey
		}
		return ObjectInfo{}, err
	}
	return objectInfoFromMetadata(object), nil
}

func (s *ObjectStore) GetObject(ctx context.Context, input GetObjectInput) (io.ReadCloser, ObjectInfo, error) {
	if err := s.HeadBucket(ctx, input.Bucket); err != nil {
		return nil, ObjectInfo{}, err
	}
	ctx, cancel := context.WithCancel(ctx)
	object, chunks, err := s.meta.GetObject(ctx, input.Bucket, input.Key)
	if err != nil {
		cancel()
		if err == metadata.ErrNotFound {
			return nil, ObjectInfo{}, ErrNoSuchKey
		}
		return nil, ObjectInfo{}, err
	}
	info := objectInfoFromMetadata(object)

	selected := make([]SelectedChunk, 0, len(chunks))
	if input.Range == nil {
		selected = make([]SelectedChunk, 0, len(chunks))
		for _, chunk := range chunks {
			selected = append(selected, SelectedChunk{ChunkRef: ChunkRef{Part: chunk.PartNumber, FileID: chunk.TelegramFileID, Offset: chunk.Offset, Size: chunk.Size}, Take: chunk.Size})
		}
	} else {
		selected = SelectChunksForRange(chunkRefsFromMetadata(chunks), *input.Range)
	}

	releaseDownload := s.acquire(ctx, s.downloads)
	if releaseDownload == nil {
		cancel()
		return nil, ObjectInfo{}, ctx.Err()
	}

	pr, pw := io.Pipe()
	go func() {
		defer cancel()
		defer releaseDownload()
		defer pw.Close()
		for _, chunk := range selected {
			reader, err := s.downloadChunk(ctx, chunk.FileID)
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}

			var source io.Reader = reader
			if chunk.Skip > 0 {
				source = io.LimitReader(source, chunk.Skip)
				if _, err := io.Copy(io.Discard, source); err != nil {
					_ = reader.Close()
					_ = pw.CloseWithError(err)
					return
				}
				source = reader
			}
			if chunk.Take >= 0 {
				source = io.LimitReader(source, chunk.Take)
			}
			if _, err := io.Copy(pw, source); err != nil {
				_ = reader.Close()
				_ = pw.CloseWithError(err)
				return
			}
			if err := reader.Close(); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
	}()

	return &cancelReadCloser{ReadCloser: pr, cancel: cancel}, info, nil
}

func (s *ObjectStore) ListObjects(ctx context.Context, input ListObjectsInput) (ListObjectsResult, error) {
	if err := s.HeadBucket(ctx, input.Bucket); err != nil {
		return ListObjectsResult{}, err
	}
	limit := input.Limit
	if limit < 0 {
		limit = 1000
	}

	result := ListObjectsResult{}
	seenPrefixes := map[string]struct{}{}
	afterKey := input.AfterKey
	truncated := false
	stopListing := false

	for len(result.Objects)+len(result.CommonPrefixes) < limit {
		remaining := limit - (len(result.Objects) + len(result.CommonPrefixes))
		queryLimit := remaining*4 + 1
		if queryLimit < 2 {
			queryLimit = 2
		}
		objects, err := s.meta.ListObjects(ctx, metadata.ListQuery{Bucket: input.Bucket, Prefix: input.Prefix, AfterKey: afterKey, Limit: queryLimit})
		if err != nil {
			return ListObjectsResult{}, err
		}
		if len(objects) == 0 {
			break
		}

		pageFull := false
		for _, object := range objects {
			emitCount := len(result.Objects) + len(result.CommonPrefixes)
			if input.Delimiter == "" {
				if emitCount >= limit {
					pageFull = true
					break
				}
				result.Objects = append(result.Objects, objectInfoFromMetadata(object))
				result.NextContinuationAfter = object.Key
				afterKey = object.Key
				if len(result.Objects)+len(result.CommonPrefixes) >= limit {
					pageFull = true
					break
				}
				continue
			}

			remainder := strings.TrimPrefix(object.Key, input.Prefix)
			idx := strings.Index(remainder, input.Delimiter)
			if idx < 0 {
				if emitCount >= limit {
					pageFull = true
					break
				}
				result.Objects = append(result.Objects, objectInfoFromMetadata(object))
				result.NextContinuationAfter = object.Key
				afterKey = object.Key
				if len(result.Objects)+len(result.CommonPrefixes) >= limit {
					pageFull = true
					break
				}
				continue
			}

			prefix := input.Prefix + remainder[:idx] + input.Delimiter
			if _, ok := seenPrefixes[prefix]; ok {
				result.NextContinuationAfter = object.Key
				afterKey = object.Key
				continue
			}
			if emitCount >= limit {
				pageFull = true
				break
			}
			seenPrefixes[prefix] = struct{}{}
			result.CommonPrefixes = append(result.CommonPrefixes, prefix)
			result.NextContinuationAfter = object.Key
			afterKey = object.Key
			if len(result.Objects)+len(result.CommonPrefixes) >= limit {
				lastKey, hasMore, err := s.advanceContinuationPastPrefix(ctx, input, prefix, afterKey)
				if err != nil {
					return ListObjectsResult{}, err
				}
				result.NextContinuationAfter = lastKey
				afterKey = lastKey
				truncated = hasMore
				stopListing = true
				break
			}
		}

		if stopListing {
			break
		}
		if pageFull {
			break
		}
		if len(objects) < queryLimit {
			break
		}
	}

	if result.NextContinuationAfter != "" {
		if !truncated {
			next, err := s.meta.ListObjects(ctx, metadata.ListQuery{Bucket: input.Bucket, Prefix: input.Prefix, AfterKey: result.NextContinuationAfter, Limit: 1})
			if err != nil {
				return ListObjectsResult{}, err
			}
			truncated = len(next) > 0
		}
		result.IsTruncated = truncated
	}

	return result, nil
}

func (s *ObjectStore) advanceContinuationPastPrefix(ctx context.Context, input ListObjectsInput, prefix, afterKey string) (string, bool, error) {
	cursor := afterKey
	for {
		objects, err := s.meta.ListObjects(ctx, metadata.ListQuery{Bucket: input.Bucket, Prefix: input.Prefix, AfterKey: cursor, Limit: 32})
		if err != nil {
			return "", false, err
		}
		if len(objects) == 0 {
			return cursor, false, nil
		}
		for _, object := range objects {
			remainder := strings.TrimPrefix(object.Key, input.Prefix)
			idx := strings.Index(remainder, input.Delimiter)
			if idx < 0 {
				return cursor, true, nil
			}
			currentPrefix := input.Prefix + remainder[:idx] + input.Delimiter
			if currentPrefix != prefix {
				return cursor, true, nil
			}
			cursor = object.Key
		}
		if len(objects) < 32 {
			return cursor, false, nil
		}
	}
}

func (s *ObjectStore) DeleteObject(ctx context.Context, bucket, key string) error {
	if err := s.HeadBucket(ctx, bucket); err != nil {
		return err
	}
	if err := s.meta.DeleteObject(ctx, bucket, key); err != nil {
		if err == metadata.ErrNotFound {
			return nil
		}
		return err
	}
	return nil
}

func HumanSize(size int64) string {
	if size < 1024 {
		return fmt.Sprintf("%d B", size)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	value := float64(size)
	for _, unit := range units {
		value /= 1024
		if value < 1024 {
			return fmt.Sprintf("%.1f %s", value, unit)
		}
	}
	return fmt.Sprintf("%.1f PiB", value/1024)
}

func (s *ObjectStore) putSingle(ctx context.Context, input PutObjectInput, strategy UploadStrategy) (PutObjectResult, error) {
	type uploadResult struct {
		uploaded telegram.UploadedFile
		err      error
	}

	now := time.Now().UTC()
	md5Hash := md5.New()
	shaHash := sha256.New()
	pipeReader, pipeWriter := io.Pipe()
	resultCh := make(chan uploadResult, 1)

	caption := s.renderCaption(input, 1, 1)
	go func() {
		uploaded, err := s.uploadTelegram(ctx, telegram.UploadRequest{
			Type:     strategy.TelegramType,
			ChatID:   s.bucketChatID(input.Bucket),
			Reader:   pipeReader,
			Filename: path.Base(input.Key),
			MIMEType: input.ContentType,
			Caption:  caption,
		})
		if err != nil {
			_ = pipeReader.CloseWithError(err)
			resultCh <- uploadResult{err: err}
			return
		}
		_ = pipeReader.Close()
		resultCh <- uploadResult{uploaded: uploaded}
	}()

	tee := io.TeeReader(input.Body, io.MultiWriter(md5Hash, shaHash, pipeWriter))
	bufferSize := s.options.Upload.PutBufferSize
	if bufferSize <= 0 {
		bufferSize = DefaultUploadConfig().PutBufferSize
	}
	buf := make([]byte, bufferSize)
	written, copyErr := io.CopyBuffer(io.Discard, tee, buf)
	if closeErr := pipeWriter.Close(); copyErr == nil && closeErr != nil {
		copyErr = closeErr
	}
	if written != input.Size && copyErr == nil {
		copyErr = fmt.Errorf("copied %d bytes, want %d", written, input.Size)
	}
	if copyErr != nil {
		if errors.Is(copyErr, io.ErrClosedPipe) {
			result := <-resultCh
			if result.err != nil {
				return PutObjectResult{}, result.err
			}
			return PutObjectResult{}, copyErr
		}
		_ = pipeWriter.CloseWithError(copyErr)
		select {
		case result := <-resultCh:
			if result.err != nil {
				return PutObjectResult{}, result.err
			}
		default:
		}
		return PutObjectResult{}, copyErr
	}

	result := <-resultCh
	if result.err != nil {
		return PutObjectResult{}, result.err
	}
	uploaded := result.uploaded

	etag := hex.EncodeToString(md5Hash.Sum(nil))
	shaSum := hex.EncodeToString(shaHash.Sum(nil))
	object := metadata.Object{
		Bucket:         input.Bucket,
		Key:            input.Key,
		Size:           input.Size,
		ContentType:    input.ContentType,
		ETag:           etag,
		SHA256:         shaSum,
		LastModified:   now,
		ChunkCount:     1,
		TelegramType:   uploaded.Type,
		UploadStrategy: strategy.UploadStrategy,
	}
	chunks := []metadata.Chunk{{
		Bucket:               input.Bucket,
		Key:                  input.Key,
		PartNumber:           1,
		Offset:               0,
		Size:                 input.Size,
		TelegramType:         uploaded.Type,
		TelegramFileID:       uploaded.FileID,
		TelegramMessageID:    uploaded.MessageID,
		TelegramFileUniqueID: uploaded.FileUniqueID,
		SHA256:               shaSum,
	}}
	if err := s.meta.PutObject(ctx, object, chunks); err != nil {
		s.logOrphanUpload(input.Bucket, input.Key, []uploadRecord{{FileID: uploaded.FileID, MessageID: uploaded.MessageID}}, err)
		return PutObjectResult{}, err
	}
	return PutObjectResult{ETag: etag}, nil
}

func (s *ObjectStore) putChunked(ctx context.Context, input PutObjectInput, strategy UploadStrategy) (PutObjectResult, error) {
	wholeMD5 := md5.New()
	wholeSHA := sha256.New()
	now := time.Now().UTC()
	chunkSize := strategy.ChunkSize
	if chunkSize <= 0 {
		chunkSize = s.options.Upload.ChunkSize
	}
	parts := int((input.Size + chunkSize - 1) / chunkSize)
	chunks := make([]metadata.Chunk, 0, parts)
	uploads := make([]uploadRecord, 0, parts)
	offset := int64(0)
	remaining := input.Size
	part := 1

	for remaining > 0 {
		thisChunkSize := chunkSize
		if remaining < thisChunkSize {
			thisChunkSize = remaining
		}
		data := make([]byte, thisChunkSize)
		read, err := io.ReadFull(input.Body, data)
		if err != nil {
			return PutObjectResult{}, err
		}
		if int64(read) != thisChunkSize {
			return PutObjectResult{}, fmt.Errorf("copied %d bytes, want %d", read, thisChunkSize)
		}
		partData := data[:read]
		_, _ = wholeMD5.Write(partData)
		_, _ = wholeSHA.Write(partData)
		chunkSHA := sha256.Sum256(partData)
		uploaded, err := s.uploadTelegram(ctx, telegram.UploadRequest{
			Type:     telegram.TypeDocument,
			ChatID:   s.bucketChatID(input.Bucket),
			Reader:   strings.NewReader(string(partData)),
			Filename: path.Base(input.Key),
			MIMEType: input.ContentType,
			Caption:  s.renderCaption(input, part, parts),
		})
		if err != nil {
			if len(uploads) > 0 {
				s.logOrphanUpload(input.Bucket, input.Key, uploads, err)
			}
			return PutObjectResult{}, err
		}
		chunks = append(chunks, metadata.Chunk{
			Bucket:               input.Bucket,
			Key:                  input.Key,
			PartNumber:           part,
			Offset:               offset,
			Size:                 int64(len(partData)),
			TelegramType:         uploaded.Type,
			TelegramFileID:       uploaded.FileID,
			TelegramMessageID:    uploaded.MessageID,
			TelegramFileUniqueID: uploaded.FileUniqueID,
			SHA256:               hex.EncodeToString(chunkSHA[:]),
		})
		uploads = append(uploads, uploadRecord{FileID: uploaded.FileID, MessageID: uploaded.MessageID})
		offset += int64(len(partData))
		remaining -= int64(len(partData))
		part++
	}
	if extra, err := io.ReadAll(input.Body); err != nil {
		return PutObjectResult{}, err
	} else if len(extra) > 0 {
		return PutObjectResult{}, fmt.Errorf("copied %d bytes, want %d", input.Size+int64(len(extra)), input.Size)
	}
	etag := hex.EncodeToString(wholeMD5.Sum(nil))
	shaSum := hex.EncodeToString(wholeSHA.Sum(nil))
	object := metadata.Object{
		Bucket:         input.Bucket,
		Key:            input.Key,
		Size:           input.Size,
		ContentType:    input.ContentType,
		ETag:           etag,
		SHA256:         shaSum,
		LastModified:   now,
		ChunkCount:     len(chunks),
		TelegramType:   telegram.TypeDocument,
		UploadStrategy: strategy.UploadStrategy,
	}
	if err := s.meta.PutObject(ctx, object, chunks); err != nil {
		s.logOrphanUpload(input.Bucket, input.Key, uploads, err)
		return PutObjectResult{}, err
	}
	return PutObjectResult{ETag: etag}, nil
}

func (s *ObjectStore) renderCaption(input PutObjectInput, part, parts int) string {
	if s.options.Caption == nil {
		return ""
	}
	return s.options.Caption.Render(telegram.CaptionData{
		Bucket: input.Bucket,
		Key:    input.Key,
		Name:   path.Base(input.Key),
		Size:   HumanSize(input.Size),
		Bytes:  input.Size,
		Part:   part,
		Parts:  parts,
	})
}

func (s *ObjectStore) uploadTelegram(ctx context.Context, request telegram.UploadRequest) (telegram.UploadedFile, error) {
	release := s.acquire(ctx, s.telegramSem)
	if release == nil {
		return telegram.UploadedFile{}, ctx.Err()
	}
	defer release()
	return s.tg.Upload(ctx, request)
}

func (s *ObjectStore) downloadChunk(ctx context.Context, fileID string) (io.ReadCloser, error) {
	release := s.acquire(ctx, s.telegramSem)
	if release == nil {
		return nil, ctx.Err()
	}
	reader, err := s.tg.Download(ctx, fileID)
	if err != nil {
		release()
		return nil, err
	}
	return &releaseReadCloser{ReadCloser: reader, release: release}, nil
}

func (s *ObjectStore) acquire(ctx context.Context, sem chan struct{}) func() {
	if sem == nil {
		return func() {}
	}
	select {
	case sem <- struct{}{}:
		return func() { <-sem }
	case <-ctx.Done():
		return nil
	}
}

func (s *ObjectStore) logOrphanUpload(bucket, key string, uploads []uploadRecord, err error) {
	if s.logger == nil {
		return
	}
	parts := make([]string, 0, len(uploads))
	for _, upload := range uploads {
		parts = append(parts, fmt.Sprintf("file_id=%s message_id=%d", upload.FileID, upload.MessageID))
	}
	s.logger.Printf("event=orphan_upload bucket=%q key=%q uploads=[%s] error=%q", bucket, key, strings.Join(parts, " "), sanitizeLogError(err))
}

var sensitiveAssignmentPattern = regexp.MustCompile(`(?i)\b(bot_token|secret_key)\b\s*([=:])\s*([^\s,;]+)`)

func sanitizeLogError(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	return sensitiveAssignmentPattern.ReplaceAllString(message, `$1$2[REDACTED]`)
}

func objectInfoFromMetadata(object metadata.Object) ObjectInfo {
	return ObjectInfo{
		Bucket:       object.Bucket,
		Key:          object.Key,
		Size:         object.Size,
		ContentType:  object.ContentType,
		ETag:         object.ETag,
		SHA256:       object.SHA256,
		LastModified: object.LastModified,
	}
}

func chunkRefsFromMetadata(chunks []metadata.Chunk) []ChunkRef {
	refs := make([]ChunkRef, 0, len(chunks))
	for _, chunk := range chunks {
		refs = append(refs, ChunkRef{Part: chunk.PartNumber, FileID: chunk.TelegramFileID, Offset: chunk.Offset, Size: chunk.Size})
	}
	return refs
}

type releaseReadCloser struct {
	io.ReadCloser
	release func()
}

func (r *releaseReadCloser) Close() error {
	err := r.ReadCloser.Close()
	if r.release != nil {
		r.release()
		r.release = nil
	}
	return err
}

type cancelReadCloser struct {
	io.ReadCloser
	cancel func()
}

func (r *cancelReadCloser) Close() error {
	err := r.ReadCloser.Close()
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	return err
}

func (s *ObjectStore) bucketChatID(bucket string) string {
	item, ok := s.startupBuckets[bucket]
	if !ok {
		return ""
	}
	return item.ChatID
}
