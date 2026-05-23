package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

func TestParseGlobalFlagsDefaultsToDataConfig(t *testing.T) {
	configPath, debug, rest, err := parseGlobalFlags(nil, io.Discard)
	if err != nil {
		t.Fatalf("parseGlobalFlags returned error: %v", err)
	}
	if configPath != "data/config.yaml" {
		t.Fatalf("configPath = %q", configPath)
	}
	if debug {
		t.Fatal("debug = true, want false")
	}
	if len(rest) != 0 {
		t.Fatalf("rest = %v", rest)
	}
}

func TestParseGlobalFlagsAcceptsConfigAliases(t *testing.T) {
	configPath, debug, rest, err := parseGlobalFlags([]string{"-config", "config-a.yaml", "ls", "photos"}, io.Discard)
	if err != nil {
		t.Fatalf("parseGlobalFlags returned error: %v", err)
	}
	if configPath != "config-a.yaml" {
		t.Fatalf("configPath = %q", configPath)
	}
	if debug {
		t.Fatal("debug = true, want false")
	}
	if strings.Join(rest, " ") != "ls photos" {
		t.Fatalf("rest = %v", rest)
	}

	configPath, debug, rest, err = parseGlobalFlags([]string{"-c", "config-b.yaml", "lsd"}, io.Discard)
	if err != nil {
		t.Fatalf("parseGlobalFlags returned error: %v", err)
	}
	if configPath != "config-b.yaml" {
		t.Fatalf("configPath = %q", configPath)
	}
	if debug {
		t.Fatal("debug = true, want false")
	}
	if strings.Join(rest, " ") != "lsd" {
		t.Fatalf("rest = %v", rest)
	}
}

func TestParseGlobalFlagsAcceptsDebugFlag(t *testing.T) {
	configPath, debug, rest, err := parseGlobalFlags([]string{"-debug", "ls", "photos"}, io.Discard)
	if err != nil {
		t.Fatalf("parseGlobalFlags returned error: %v", err)
	}
	if configPath != "data/config.yaml" {
		t.Fatalf("configPath = %q", configPath)
	}
	if !debug {
		t.Fatal("debug = false, want true")
	}
	if strings.Join(rest, " ") != "ls photos" {
		t.Fatalf("rest = %v", rest)
	}
}

func TestRunMainStartsServerWithoutSubcommand(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "123456:valid-token")
	t.Setenv("TGS3_SECRET_KEY", "secret")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, configPath, filepath.Join(t.TempDir(), "metadata.sqlite"))

	oldListenAndServe := listenAndServe
	listenAndServe = func(addr string, handler http.Handler) error {
		if addr != "127.0.0.1:0" {
			t.Fatalf("addr = %q, want 127.0.0.1:0", addr)
		}
		return errors.New("server stopped")
	}
	t.Cleanup(func() { listenAndServe = oldListenAndServe })

	err := runMain([]string{"-c", configPath}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "server stopped") {
		t.Fatalf("err = %v, want server stopped", err)
	}
}

func TestRunMainRejectsBothConfigAliases(t *testing.T) {
	err := runMain([]string{"-config", "a.yaml", "-c", "b.yaml"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "-config and -c cannot both be set") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunMainRejectsDebugFlagAfterSubcommand(t *testing.T) {
	err := runMain([]string{"ls", "-debug", "photos"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunMainDebugLogsServiceModeToStderr(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "123456:valid-token")
	t.Setenv("TGS3_SECRET_KEY", "secret")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	sqlitePath := filepath.Join(t.TempDir(), "metadata.sqlite")
	writeConfig(t, configPath, sqlitePath)

	oldListenAndServe := listenAndServe
	listenAndServe = func(string, http.Handler) error { return nil }
	t.Cleanup(func() { listenAndServe = oldListenAndServe })

	var stderr strings.Builder
	err := runMain([]string{"-debug", "-c", configPath}, io.Discard, &stderr)
	if err != nil {
		t.Fatalf("runMain returned error: %v", err)
	}
	for _, want := range []string{"debug mode=service", "config_path=" + strconv.Quote(configPath), "sqlite_path=" + strconv.Quote(sqlitePath), "listen_addr=" + strconv.Quote("127.0.0.1:0"), "bucket=" + strconv.Quote("photos")} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want substring %q", stderr.String(), want)
		}
	}
}

func TestRunMainDebugLogsLSModeToStderrWithoutChangingStdout(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "metadata.sqlite")
	configPath := writeConfigWithPath(t, sqlitePath)
	meta, err := metadata.OpenSQLite(sqlitePath)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	seedBucket(t, meta, "photos", true)
	seedObject(t, meta, "photos", "2026/a.txt")
	seedObject(t, meta, "photos", "2026/b.txt")

	var stdout strings.Builder
	var stderr strings.Builder
	if err := runMain([]string{"-debug", "-c", configPath, "ls", "-n", "1", "photos/2026/"}, &stdout, &stderr); err != nil {
		t.Fatalf("runMain returned error: %v", err)
	}
	if got, want := stdout.String(), "2026/a.txt\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	for _, want := range []string{"debug mode=ls", "config_path=" + strconv.Quote(configPath), "sqlite_path=" + strconv.Quote(sqlitePath), "bucket=" + strconv.Quote("photos"), "prefix=" + strconv.Quote("2026/"), "limit=1", "page_after=", "page_limit=1", "rows=1"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want substring %q", stderr.String(), want)
		}
	}
}

func TestRunMainDebugLogsQuoteUserControlledFields(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "metadata.sqlite")
	configPath := writeConfigWithPath(t, sqlitePath)
	meta, err := metadata.OpenSQLite(sqlitePath)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	seedBucket(t, meta, "photos", true)
	seedObject(t, meta, "photos", "2026/line\nbreak.txt")

	var stdout strings.Builder
	var stderr strings.Builder
	if err := runMain([]string{"-debug", "-c", configPath, "ls", "photos/2026/line\n"}, &stdout, &stderr); err != nil {
		t.Fatalf("runMain returned error: %v", err)
	}
	if got, want := stdout.String(), "2026/line\nbreak.txt\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	output := stderr.String()
	if !strings.Contains(output, `prefix="2026/line\n"`) || strings.Contains(output, "prefix=2026/line\n") {
		t.Fatalf("stderr did not quote prefix safely: %q", output)
	}
}

func TestRunMainDebugLogsLSDModeToStderr(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "metadata.sqlite")
	configPath := writeConfigWithPath(t, sqlitePath)
	meta, err := metadata.OpenSQLite(sqlitePath)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	seedBucket(t, meta, "photos", true)
	seedObject(t, meta, "photos", "2026/jan/a.txt")

	var stdout strings.Builder
	var stderr strings.Builder
	if err := runMain([]string{"-debug", "-c", configPath, "lsd", "photos/2026/"}, &stdout, &stderr); err != nil {
		t.Fatalf("runMain returned error: %v", err)
	}
	if got, want := stdout.String(), "2026/jan/\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	for _, want := range []string{"debug mode=lsd", "config_path=" + strconv.Quote(configPath), "sqlite_path=" + strconv.Quote(sqlitePath), "bucket=" + strconv.Quote("photos"), "prefix=" + strconv.Quote("2026/"), "page_after=", "page_limit=1000", "rows=1"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want substring %q", stderr.String(), want)
		}
	}
}

func TestRunMainDebugLogsQuoteLSDPageCursor(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "metadata.sqlite")
	configPath := writeConfigWithPath(t, sqlitePath)
	meta, err := metadata.OpenSQLite(sqlitePath)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	seedBucket(t, meta, "photos", true)
	for i := 0; i < 999; i++ {
		seedObject(t, meta, "photos", fmt.Sprintf("2026/a%03d/file.txt", i))
	}
	cursorKey := "2026/a999\nbreak/file.txt"
	seedObject(t, meta, "photos", cursorKey)
	seedObject(t, meta, "photos", "2026/zoo/file.txt")

	var stdout strings.Builder
	var stderr strings.Builder
	if err := runMain([]string{"-debug", "-c", configPath, "lsd", "photos/2026/"}, &stdout, &stderr); err != nil {
		t.Fatalf("runMain returned error: %v", err)
	}
	output := stderr.String()
	if !strings.Contains(output, "page_after="+strconv.Quote(cursorKey)) || strings.Contains(output, "page_after="+cursorKey) {
		t.Fatalf("stderr did not quote lsd cursor safely: %q", output)
	}
}

func TestRunMainGlobalHelpSucceedsAndWritesUsage(t *testing.T) {
	var errOut strings.Builder
	err := runMain([]string{"-h"}, io.Discard, &errOut)
	if err != nil {
		t.Fatalf("runMain returned error: %v", err)
	}
	got := errOut.String()
	want := "Usage:\n  tgs3 [-debug] [-c|-config config.yaml]\n  tgs3 [-debug] [-c|-config config.yaml] ls [-n|-limit N] bucket[/prefix]\n  tgs3 [-debug] [-c|-config config.yaml] lsd [bucket[/prefix]]\n"
	if got != want {
		t.Fatalf("help output = %q, want %q", got, want)
	}
}

func TestRunMainLSHelpSucceedsAndWritesUsage(t *testing.T) {
	var errOut strings.Builder
	err := runMain([]string{"ls", "-h"}, io.Discard, &errOut)
	if err != nil {
		t.Fatalf("runMain returned error: %v", err)
	}
	got := errOut.String()
	for _, want := range []string{"Usage of ls:", "-limit int", "-n int", "default 1000"} {
		if !strings.Contains(got, want) {
			t.Fatalf("help output = %q, want substring %q", got, want)
		}
	}
}

func TestRunMainLSDHelpSucceedsAndWritesUsage(t *testing.T) {
	var errOut strings.Builder
	err := runMain([]string{"lsd", "-h"}, io.Discard, &errOut)
	if err != nil {
		t.Fatalf("runMain returned error: %v", err)
	}
	if got := errOut.String(); !strings.Contains(got, "Usage of lsd:") {
		t.Fatalf("help output = %q", got)
	}
}

func TestRunMainLocalCommandsDoNotStartServerPath(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "metadata.sqlite")
	configPath := writeConfigWithPath(t, sqlitePath)
	meta, err := metadata.OpenSQLite(sqlitePath)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	seedBucket(t, meta, "photos", true)
	if err := meta.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	oldNewObjectStore := newObjectStore
	oldListenAndServe := listenAndServe
	newObjectStore = func(metadata.Store, telegram.Client, store.Options) (*store.ObjectStore, error) {
		t.Fatal("newObjectStore should not be called")
		return nil, nil
	}
	listenAndServe = func(string, http.Handler) error {
		t.Fatal("listenAndServe should not be called")
		return nil
	}
	defer func() {
		newObjectStore = oldNewObjectStore
		listenAndServe = oldListenAndServe
	}()

	if err := runMain([]string{"-c", configPath, "ls", "photos"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("runMain ls returned error: %v", err)
	}
	var out strings.Builder
	if err := runMain([]string{"-c", configPath, "lsd"}, &out, io.Discard); err != nil {
		t.Fatalf("runMain lsd returned error: %v", err)
	}
	if out.String() != "photos\n" {
		t.Fatalf("lsd output = %q", out.String())
	}
}

func TestParseBucketPrefix(t *testing.T) {
	testCases := []struct {
		name       string
		input      string
		wantBucket string
		wantPrefix string
		wantErr    bool
	}{
		{name: "bucket only", input: "photos", wantBucket: "photos", wantPrefix: ""},
		{name: "bucket prefix", input: "photos/2026/", wantBucket: "photos", wantPrefix: "2026/"},
		{name: "empty", input: "", wantErr: true},
		{name: "empty bucket", input: "/2026/", wantErr: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			bucket, prefix, err := parseBucketPrefix(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBucketPrefix returned error: %v", err)
			}
			if bucket != tc.wantBucket || prefix != tc.wantPrefix {
				t.Fatalf("bucket=%q prefix=%q, want bucket=%q prefix=%q", bucket, prefix, tc.wantBucket, tc.wantPrefix)
			}
		})
	}
}

func TestOpenMetadataFromConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	sqlitePath := filepath.Join(t.TempDir(), "metadata.sqlite")
	writeConfig(t, configPath, sqlitePath)

	writableMeta, err := metadata.OpenSQLite(sqlitePath)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	if err := writableMeta.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	meta, sqlitePath, err := openMetadataFromConfig(configPath)
	if err != nil {
		t.Fatalf("openMetadataFromConfig returned error: %v", err)
	}
	defer meta.Close()
	if sqlitePath == "" {
		t.Fatal("sqlitePath = empty")
	}
}

func TestOpenMetadataFromConfigDoesNotCreateMissingDatabase(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "missing.sqlite")
	configPath := writeConfigWithPath(t, sqlitePath)

	err := runLS(configPath, []string{"photos"}, io.Discard, io.Discard, newDebugLogger(false, io.Discard))
	if err == nil || !strings.Contains(err.Error(), "open sqlite metadata") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(sqlitePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("sqlite path was created or stat failed unexpectedly: %v", statErr)
	}
}

func TestRunLSReadsMetadataWhenServiceConfigIsIncomplete(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "metadata.sqlite")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("metadata:\n  sqlite_path: \""+sqlitePath+"\"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	meta, err := metadata.OpenSQLite(sqlitePath)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	seedBucket(t, meta, "photos", true)
	seedObject(t, meta, "photos", "2026/a.txt")

	var out strings.Builder
	if err := runLS(configPath, []string{"photos/2026/"}, &out, io.Discard, newDebugLogger(false, io.Discard)); err != nil {
		t.Fatalf("runLS returned error: %v", err)
	}
	if got, want := out.String(), "2026/a.txt\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestRunLSRejectsUnknownLocalMetadataConfigField(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "metadata.sqlite")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configData := "metadata:\n  sqlite_pth: \"" + sqlitePath + "\"\n"
	if err := os.WriteFile(configPath, []byte(configData), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	err := runLS(configPath, []string{"photos"}, io.Discard, io.Discard, newDebugLogger(false, io.Discard))
	if err == nil || !strings.Contains(err.Error(), "field sqlite_pth not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunLSRejectsInvalidArguments(t *testing.T) {
	configPath := writeConfigWithPath(t, filepath.Join(t.TempDir(), "metadata.sqlite"))

	testCases := []struct {
		name string
		args []string
	}{
		{name: "missing path", args: nil},
		{name: "too many paths", args: []string{"photos", "extra"}},
		{name: "empty bucket", args: []string{"/prefix"}},
		{name: "negative limit", args: []string{"-limit", "-1", "photos"}},
		{name: "negative short limit", args: []string{"-n", "-1", "photos"}},
		{name: "both limits", args: []string{"-limit", "1", "-n", "2", "photos"}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if err := runLS(configPath, tc.args, io.Discard, io.Discard, newDebugLogger(false, io.Discard)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestRunLSListsObjectKeysWithPrefixAndLimit(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "metadata.sqlite")
	configPath := writeConfigWithPath(t, sqlitePath)
	meta, err := metadata.OpenSQLite(sqlitePath)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()

	seedBucket(t, meta, "photos", true)
	seedObject(t, meta, "photos", "2026/a.txt")
	seedObject(t, meta, "photos", "2026/b.txt")
	seedObject(t, meta, "photos", "2027/c.txt")

	var out strings.Builder
	if err := runLS(configPath, []string{"-n", "2", "photos/2026/"}, &out, io.Discard, newDebugLogger(false, io.Discard)); err != nil {
		t.Fatalf("runLS returned error: %v", err)
	}
	if got, want := out.String(), "2026/a.txt\n2026/b.txt\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestRunLSDefaultLimitIsOneThousand(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "metadata.sqlite")
	configPath := writeConfigWithPath(t, sqlitePath)
	meta, err := metadata.OpenSQLite(sqlitePath)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()

	seedBucket(t, meta, "photos", true)
	for i := 0; i < 1005; i++ {
		seedObject(t, meta, "photos", fmt.Sprintf("k%04d", i))
	}

	var out strings.Builder
	if err := runLS(configPath, []string{"photos"}, &out, io.Discard, newDebugLogger(false, io.Discard)); err != nil {
		t.Fatalf("runLS returned error: %v", err)
	}
	if got, want := len(strings.Split(strings.TrimSpace(out.String()), "\n")), 1000; got != want {
		t.Fatalf("line count = %d, want %d", got, want)
	}
}

func TestRunLSZeroLimitListsAllAcrossPages(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "metadata.sqlite")
	configPath := writeConfigWithPath(t, sqlitePath)
	meta, err := metadata.OpenSQLite(sqlitePath)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()

	seedBucket(t, meta, "photos", true)
	for i := 0; i < 1005; i++ {
		seedObject(t, meta, "photos", fmt.Sprintf("k%04d", i))
	}

	var out strings.Builder
	if err := runLS(configPath, []string{"-n", "0", "photos"}, &out, io.Discard, newDebugLogger(false, io.Discard)); err != nil {
		t.Fatalf("runLS returned error: %v", err)
	}
	if got, want := len(strings.Split(strings.TrimSpace(out.String()), "\n")), 1005; got != want {
		t.Fatalf("line count = %d, want %d", got, want)
	}
}

func TestRunLSRejectsMissingOrDisabledBucket(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "metadata.sqlite")
	configPath := writeConfigWithPath(t, sqlitePath)
	meta, err := metadata.OpenSQLite(sqlitePath)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()

	seedBucket(t, meta, "disabled", false)

	if err := runLS(configPath, []string{"missing"}, io.Discard, io.Discard, newDebugLogger(false, io.Discard)); err == nil || !strings.Contains(err.Error(), "bucket not found: missing") {
		t.Fatalf("err = %v", err)
	}
	if err := runLS(configPath, []string{"disabled"}, io.Discard, io.Discard, newDebugLogger(false, io.Discard)); err == nil || !strings.Contains(err.Error(), "bucket not found: disabled") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunLSDListsEnabledBuckets(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "metadata.sqlite")
	configPath := writeConfigWithPath(t, sqlitePath)
	meta, err := metadata.OpenSQLite(sqlitePath)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	seedBucket(t, meta, "backups", true)
	seedBucket(t, meta, "disabled", false)
	seedBucket(t, meta, "photos", true)

	var out strings.Builder
	if err := runLSD(configPath, nil, &out, io.Discard, newDebugLogger(false, io.Discard)); err != nil {
		t.Fatalf("runLSD returned error: %v", err)
	}
	if out.String() != "backups\nphotos\n" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestRunLSDListsDirectCommonPrefixes(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "metadata.sqlite")
	configPath := writeConfigWithPath(t, sqlitePath)
	meta, err := metadata.OpenSQLite(sqlitePath)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	seedBucket(t, meta, "photos", true)
	seedObject(t, meta, "photos", "2026/a.txt")
	seedObject(t, meta, "photos", "2026/jan/a.txt")
	seedObject(t, meta, "photos", "2026/jan/b.txt")
	seedObject(t, meta, "photos", "2026/feb/c.txt")
	seedObject(t, meta, "photos", "2027/root.txt")

	var out strings.Builder
	if err := runLSD(configPath, []string{"photos/2026/"}, &out, io.Discard, newDebugLogger(false, io.Discard)); err != nil {
		t.Fatalf("runLSD returned error: %v", err)
	}
	if out.String() != "2026/feb/\n2026/jan/\n" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestRunLSDRejectsInvalidArgumentsAndMissingBucket(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "metadata.sqlite")
	configPath := writeConfigWithPath(t, sqlitePath)
	meta, err := metadata.OpenSQLite(sqlitePath)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()

	seedBucket(t, meta, "disabled", false)

	if err := runLSD(configPath, []string{"photos", "extra"}, io.Discard, io.Discard, newDebugLogger(false, io.Discard)); err == nil {
		t.Fatal("expected too many args error")
	}
	if err := runLSD(configPath, []string{"-n", "1", "photos"}, io.Discard, io.Discard, newDebugLogger(false, io.Discard)); err == nil {
		t.Fatal("expected unsupported -n error")
	}
	if err := runLSD(configPath, []string{"-limit", "1", "photos"}, io.Discard, io.Discard, newDebugLogger(false, io.Discard)); err == nil {
		t.Fatal("expected unsupported -limit error")
	}
	if err := runLSD(configPath, []string{"/prefix"}, io.Discard, io.Discard, newDebugLogger(false, io.Discard)); err == nil {
		t.Fatal("expected empty bucket error")
	}
	if err := runLSD(configPath, []string{"missing"}, io.Discard, io.Discard, newDebugLogger(false, io.Discard)); err == nil || !strings.Contains(err.Error(), "bucket not found: missing") {
		t.Fatalf("err = %v", err)
	}
	if err := runLSD(configPath, []string{"disabled"}, io.Discard, io.Discard, newDebugLogger(false, io.Discard)); err == nil || !strings.Contains(err.Error(), "bucket not found: disabled") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunLSDDeduplicatesCommonPrefixesAcrossPageBoundary(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "metadata.sqlite")
	configPath := writeConfigWithPath(t, sqlitePath)
	meta, err := metadata.OpenSQLite(sqlitePath)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()

	seedBucket(t, meta, "photos", true)
	for i := 0; i < 1001; i++ {
		seedObject(t, meta, "photos", fmt.Sprintf("2026/jan/k%04d.txt", i))
	}
	seedObject(t, meta, "photos", "2026/zoo/a.txt")

	var out strings.Builder
	if err := runLSD(configPath, []string{"photos/2026/"}, &out, io.Discard, newDebugLogger(false, io.Discard)); err != nil {
		t.Fatalf("runLSD returned error: %v", err)
	}
	if got, want := out.String(), "2026/jan/\n2026/zoo/\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
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

func writeConfigWithPath(t *testing.T, sqlitePath string) string {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, configPath, sqlitePath)
	return configPath
}

func seedBucket(t *testing.T, meta metadata.Store, name string, enabled bool) {
	t.Helper()
	if err := meta.UpsertBucket(context.Background(), metadata.Bucket{Name: name, ChatID: "-100", CreatedAt: time.Now().UTC(), Enabled: enabled}); err != nil {
		t.Fatalf("UpsertBucket returned error: %v", err)
	}
}

func seedObject(t *testing.T, meta metadata.Store, bucket, key string) {
	t.Helper()
	object := metadata.Object{Bucket: bucket, Key: key, Size: int64(len(key)), ContentType: "text/plain", ETag: key + "-etag", SHA256: key + "-sha", LastModified: time.Now().UTC(), ChunkCount: 1, TelegramType: "document", UploadStrategy: "document"}
	chunk := metadata.Chunk{Bucket: bucket, Key: key, PartNumber: 1, Offset: 0, Size: object.Size, TelegramType: "document", TelegramFileID: key + "-file", TelegramMessageID: 1, TelegramFileUniqueID: key + "-unique", SHA256: object.SHA256}
	if err := meta.PutObject(context.Background(), object, []metadata.Chunk{chunk}); err != nil {
		t.Fatalf("PutObject(%s) returned error: %v", key, err)
	}
}
