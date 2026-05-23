package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aahl/tgs3/metadata"
	"github.com/aahl/tgs3/store"
	"github.com/aahl/tgs3/telegram"
)

func TestRunReturnsObjectStoreCreationFailure(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "123456:valid-token")
	t.Setenv("TGS3_SECRET_KEY", "secret")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, configPath, filepath.Join(t.TempDir(), "metadata.sqlite"))

	storeErr := errors.New("startup bucket snapshot: metadata unavailable")
	oldNewObjectStore := newObjectStore
	oldListenAndServe := listenAndServe
	newObjectStore = func(metadata.Store, telegram.Client, store.Options) (*store.ObjectStore, error) {
		return nil, storeErr
	}
	listenAndServe = func(string, http.Handler) error {
		t.Fatal("listenAndServe should not be called")
		return nil
	}
	t.Cleanup(func() {
		newObjectStore = oldNewObjectStore
		listenAndServe = oldListenAndServe
	})

	err := run(configPath)
	if err == nil {
		t.Fatal("expected run error")
	}
	if !strings.Contains(err.Error(), "create object store") || !strings.Contains(err.Error(), "metadata unavailable") {
		t.Fatalf("err = %v", err)
	}
}

func writeConfig(t *testing.T, path, sqlitePath string) {
	t.Helper()
	config := `server:
  listen: "127.0.0.1:0"
auth:
  region: "us-east-1"
  credentials:
    - access_key: "admin"
      secret_key_env: "TGS3_SECRET_KEY"
telegram:
  bot_token_env: "TELEGRAM_BOT_TOKEN"
metadata:
  sqlite_path: "` + sqlitePath + `"
buckets:
  photos:
    chat_id: "-100"
`
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
}
