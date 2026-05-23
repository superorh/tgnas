package metadata

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type SQLiteStore struct {
	db *sql.DB
}

func OpenSQLite(path string) (*SQLiteStore, error) {
	store, err := openSQLite(path)
	if err != nil {
		return nil, err
	}
	if err := store.migrate(); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

func OpenSQLiteReadOnly(path string) (*SQLiteStore, error) {
	return openSQLite(sqliteReadOnlyDSN(path))
}

func openSQLite(dataSourceName string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", dataSourceName)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &SQLiteStore{db: db}, nil
}

func sqliteReadOnlyDSN(path string) string {
	u := url.URL{Scheme: "file", Path: path}
	query := u.Query()
	query.Set("mode", "ro")
	u.RawQuery = query.Encode()
	return u.String()
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func (s *SQLiteStore) UpsertBucket(ctx context.Context, bucket Bucket) error {
	_, err := s.db.ExecContext(ctx, `
	INSERT INTO buckets (name, chat_id, created_at, enabled)
	VALUES (?, ?, ?, ?)
	ON CONFLICT(name) DO UPDATE SET
		chat_id = excluded.chat_id,
		created_at = excluded.created_at,
		enabled = excluded.enabled
	`, bucket.Name, bucket.ChatID, bucket.CreatedAt.Unix(), boolToInt(bucket.Enabled))
	return err
}

func (s *SQLiteStore) GetBucket(ctx context.Context, name string) (Bucket, error) {
	row := s.db.QueryRowContext(ctx, `
	SELECT name, chat_id, created_at, enabled
	FROM buckets
	WHERE name = ?
	`, name)

	var bucket Bucket
	var createdAt int64
	var enabled int
	if err := row.Scan(&bucket.Name, &bucket.ChatID, &createdAt, &enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Bucket{}, ErrNotFound
		}
		return Bucket{}, err
	}

	bucket.CreatedAt = unixSeconds(createdAt)
	bucket.Enabled = enabled != 0
	return bucket, nil
}

func (s *SQLiteStore) ListBuckets(ctx context.Context) ([]Bucket, error) {
	rows, err := s.db.QueryContext(ctx, `
	SELECT name, chat_id, created_at, enabled
	FROM buckets
	WHERE enabled = 1
	ORDER BY name ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var buckets []Bucket
	for rows.Next() {
		var bucket Bucket
		var createdAt int64
		var enabled int
		if err := rows.Scan(&bucket.Name, &bucket.ChatID, &createdAt, &enabled); err != nil {
			return nil, err
		}
		bucket.CreatedAt = unixSeconds(createdAt)
		bucket.Enabled = enabled != 0
		buckets = append(buckets, bucket)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return buckets, nil
}

func (s *SQLiteStore) PutObject(ctx context.Context, object Object, chunks []Chunk) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM object_chunks WHERE bucket = ? AND key = ?`, object.Bucket, object.Key); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
	INSERT INTO objects (bucket, key, size, content_type, etag, sha256, last_modified, chunk_count, telegram_type, upload_strategy)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(bucket, key) DO UPDATE SET
		size = excluded.size,
		content_type = excluded.content_type,
		etag = excluded.etag,
		sha256 = excluded.sha256,
		last_modified = excluded.last_modified,
		chunk_count = excluded.chunk_count,
		telegram_type = excluded.telegram_type,
		upload_strategy = excluded.upload_strategy
	`, object.Bucket, object.Key, object.Size, object.ContentType, object.ETag, object.SHA256, object.LastModified.Unix(), object.ChunkCount, object.TelegramType, object.UploadStrategy); err != nil {
		return err
	}

	for _, chunk := range chunks {
		if _, err := tx.ExecContext(ctx, `
	INSERT INTO object_chunks (bucket, key, part_number, offset, size, telegram_type, telegram_file_id, telegram_message_id, telegram_file_unique_id, sha256)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, chunk.Bucket, chunk.Key, chunk.PartNumber, chunk.Offset, chunk.Size, chunk.TelegramType, chunk.TelegramFileID, chunk.TelegramMessageID, chunk.TelegramFileUniqueID, chunk.SHA256); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *SQLiteStore) GetObject(ctx context.Context, bucket, key string) (Object, []Chunk, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Object{}, nil, err
	}
	defer tx.Rollback()

	object, err := scanObject(tx.QueryRowContext(ctx, `
	SELECT bucket, key, size, content_type, etag, sha256, last_modified, chunk_count, telegram_type, upload_strategy
	FROM objects
	WHERE bucket = ? AND key = ?
	`, bucket, key))
	if err != nil {
		return Object{}, nil, err
	}

	rows, err := tx.QueryContext(ctx, `
	SELECT bucket, key, part_number, offset, size, telegram_type, telegram_file_id, telegram_message_id, telegram_file_unique_id, sha256
	FROM object_chunks
	WHERE bucket = ? AND key = ?
	ORDER BY part_number ASC
	`, bucket, key)
	if err != nil {
		return Object{}, nil, err
	}
	defer rows.Close()

	var chunks []Chunk
	for rows.Next() {
		var chunk Chunk
		if err := rows.Scan(&chunk.Bucket, &chunk.Key, &chunk.PartNumber, &chunk.Offset, &chunk.Size, &chunk.TelegramType, &chunk.TelegramFileID, &chunk.TelegramMessageID, &chunk.TelegramFileUniqueID, &chunk.SHA256); err != nil {
			return Object{}, nil, err
		}
		chunks = append(chunks, chunk)
	}
	if err := rows.Err(); err != nil {
		return Object{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return Object{}, nil, err
	}

	return object, chunks, nil
}

func (s *SQLiteStore) HeadObject(ctx context.Context, bucket, key string) (Object, error) {
	object, err := scanObject(s.db.QueryRowContext(ctx, `
	SELECT bucket, key, size, content_type, etag, sha256, last_modified, chunk_count, telegram_type, upload_strategy
	FROM objects
	WHERE bucket = ? AND key = ?
	`, bucket, key))
	if err != nil {
		return Object{}, err
	}
	return object, nil
}

func (s *SQLiteStore) ListObjects(ctx context.Context, query ListQuery) ([]Object, error) {
	limit := query.Limit
	if limit <= 0 {
		limit = 1000
	}

	rows, err := s.db.QueryContext(ctx, `
	SELECT bucket, key, size, content_type, etag, sha256, last_modified, chunk_count, telegram_type, upload_strategy
	FROM objects
	WHERE bucket = ?
	  AND key LIKE ? ESCAPE '\'
	  AND key > ?
	ORDER BY key ASC
	LIMIT ?
	`, query.Bucket, escapeSQLiteLikePattern(query.Prefix)+"%", query.AfterKey, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var objects []Object
	for rows.Next() {
		object, err := scanObject(rows)
		if err != nil {
			return nil, err
		}
		objects = append(objects, object)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return objects, nil
}

func (s *SQLiteStore) DeleteObject(ctx context.Context, bucket, key string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM object_chunks WHERE bucket = ? AND key = ?`, bucket, key); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM objects WHERE bucket = ? AND key = ?`, bucket, key); err != nil {
		return err
	}

	return tx.Commit()
}

func (s *SQLiteStore) migrate() error {
	_, err := s.db.Exec(`
	CREATE TABLE IF NOT EXISTS buckets (
		name TEXT PRIMARY KEY,
		chat_id TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		enabled INTEGER NOT NULL
	);

	CREATE TABLE IF NOT EXISTS objects (
		bucket TEXT NOT NULL,
		key TEXT NOT NULL,
		size INTEGER NOT NULL,
		content_type TEXT NOT NULL,
		etag TEXT NOT NULL,
		sha256 TEXT NOT NULL,
		last_modified INTEGER NOT NULL,
		chunk_count INTEGER NOT NULL,
		telegram_type TEXT NOT NULL,
		upload_strategy TEXT NOT NULL,
		PRIMARY KEY(bucket, key)
	);

	CREATE TABLE IF NOT EXISTS object_chunks (
		bucket TEXT NOT NULL,
		key TEXT NOT NULL,
		part_number INTEGER NOT NULL,
		offset INTEGER NOT NULL,
		size INTEGER NOT NULL,
		telegram_type TEXT NOT NULL,
		telegram_file_id TEXT NOT NULL,
		telegram_message_id INTEGER NOT NULL,
		telegram_file_unique_id TEXT NOT NULL,
		sha256 TEXT NOT NULL,
		PRIMARY KEY(bucket, key, part_number)
	);
	`)
	return err
}

type objectScanner interface {
	Scan(dest ...any) error
}

func scanObject(scanner objectScanner) (Object, error) {
	var object Object
	var lastModified int64
	if err := scanner.Scan(&object.Bucket, &object.Key, &object.Size, &object.ContentType, &object.ETag, &object.SHA256, &lastModified, &object.ChunkCount, &object.TelegramType, &object.UploadStrategy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Object{}, ErrNotFound
		}
		return Object{}, err
	}

	object.LastModified = unixSeconds(lastModified)
	return object, nil
}

func escapeSQLiteLikePattern(value string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`%`, `\%`,
		`_`, `\_`,
	)
	return replacer.Replace(value)
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func unixSeconds(value int64) time.Time {
	return time.Unix(value, 0)
}
