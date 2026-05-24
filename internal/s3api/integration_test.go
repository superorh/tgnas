package s3api

import (
	"context"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/aahl/tgnas/internal/testutil"
	"github.com/aahl/tgnas/metadata"
	"github.com/aahl/tgnas/store"
)

func newAWSSDKTestClient(t *testing.T) (*s3.Client, *testutil.FakeTelegram) {
	t.Helper()
	ctx := context.Background()
	meta, err := metadata.OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	for name, chatID := range map[string]string{"photos": "-100", "backups": "-200"} {
		if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: name, ChatID: chatID, CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
			t.Fatalf("UpsertBucket(%s) returned error: %v", name, err)
		}
	}
	fake := testutil.NewFakeTelegram()
	objectStore, err := store.NewObjectStore(meta, fake, store.Options{Upload: store.DefaultUploadConfig()})
	if err != nil {
		t.Fatalf("NewObjectStore returned error: %v", err)
	}
	s3Server := NewServer(objectStore, Options{Region: "us-east-1", Credentials: map[string]string{"AKID": "SECRET"}, Ready: func() bool { return true }, SigV4MaxSkew: -1})
	httpServer := httptest.NewServer(s3Server)
	t.Cleanup(httpServer.Close)
	cfg := aws.Config{
		Region: "us-east-1",
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "AKID", SecretAccessKey: "SECRET"}, nil
		}),
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(httpServer.URL)
		o.UsePathStyle = true
	})
	return client, fake
}

func TestAWSSDKObjectLifecycleAgainstConfiguredBucket(t *testing.T) {
	client, _ := newAWSSDKTestClient(t)
	ctx := context.Background()
	_, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("photos"), Key: aws.String("hello.txt"), Body: strings.NewReader("hello"), ContentType: aws.String("text/plain")})
	if err != nil {
		t.Fatalf("PutObject returned error: %v", err)
	}
	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("photos"), Key: aws.String("hello.txt")})
	if err != nil || head.ContentLength == nil || *head.ContentLength != 5 {
		t.Fatalf("HeadObject = %+v err = %v", head, err)
	}
	get, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("photos"), Key: aws.String("hello.txt")})
	if err != nil {
		t.Fatalf("GetObject returned error: %v", err)
	}
	defer get.Body.Close()
	body, err := io.ReadAll(get.Body)
	if err != nil {
		t.Fatalf("ReadAll returned error: %v", err)
	}
	if string(body) != "hello" {
		t.Fatalf("body = %q", string(body))
	}
	_, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("photos"), Key: aws.String("hello.txt")})
	if err != nil {
		t.Fatalf("DeleteObject returned error: %v", err)
	}
	if _, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("photos"), Key: aws.String("hello.txt")}); err == nil {
		t.Fatal("HeadObject after DeleteObject error = nil, want non-nil")
	}
}

func TestAWSSDKListObjectsV2Pagination(t *testing.T) {
	client, _ := newAWSSDKTestClient(t)
	ctx := context.Background()
	for _, key := range []string{"a/1.txt", "a/2.txt", "a/3.txt"} {
		_, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("photos"), Key: aws.String(key), Body: strings.NewReader(key)})
		if err != nil {
			t.Fatalf("PutObject(%s): %v", key, err)
		}
	}
	first, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("photos"), Prefix: aws.String("a/"), MaxKeys: aws.Int32(1)})
	if err != nil || len(first.Contents) != 1 || first.NextContinuationToken == nil || !aws.ToBool(first.IsTruncated) || aws.ToString(first.Contents[0].Key) != "a/1.txt" {
		t.Fatalf("first page = %+v err = %v", first, err)
	}
	second, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("photos"), Prefix: aws.String("a/"), ContinuationToken: first.NextContinuationToken, MaxKeys: aws.Int32(2)})
	if err != nil || len(second.Contents) != 2 || aws.ToBool(second.IsTruncated) || aws.ToString(second.Contents[0].Key) != "a/2.txt" || aws.ToString(second.Contents[1].Key) != "a/3.txt" {
		t.Fatalf("second page = %+v err = %v", second, err)
	}
}

func TestAWSSDKRangeGetObject(t *testing.T) {
	client, _ := newAWSSDKTestClient(t)
	ctx := context.Background()
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("photos"), Key: aws.String("letters.txt"), Body: strings.NewReader("abcdefgh")}); err != nil {
		t.Fatalf("PutObject returned error: %v", err)
	}
	out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("photos"), Key: aws.String("letters.txt"), Range: aws.String("bytes=2-5")})
	if err != nil {
		t.Fatalf("GetObject returned error: %v", err)
	}
	defer out.Body.Close()
	body, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatalf("ReadAll returned error: %v", err)
	}
	if string(body) != "cdef" || out.ContentRange == nil || *out.ContentRange != "bytes 2-5/8" {
		t.Fatalf("body = %q contentRange = %v", string(body), out.ContentRange)
	}
}

func TestAWSSDKTwoBucketsUseDifferentChats(t *testing.T) {
	client, fake := newAWSSDKTestClient(t)
	ctx := context.Background()
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("photos"), Key: aws.String("p.txt"), Body: strings.NewReader("p")}); err != nil {
		t.Fatalf("PutObject photos returned error: %v", err)
	}
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("backups"), Key: aws.String("b.txt"), Body: strings.NewReader("b")}); err != nil {
		t.Fatalf("PutObject backups returned error: %v", err)
	}
	if len(fake.Uploads) != 2 || fake.Uploads[0].ChatID == fake.Uploads[1].ChatID {
		t.Fatalf("uploads = %+v", fake.Uploads)
	}
}

func TestMinIOSmokeCheckSkippedWhenUnavailable(t *testing.T) {
	_, mcErr := exec.LookPath("mc")
	_, rcloneErr := exec.LookPath("rclone")
	if mcErr != nil && rcloneErr != nil {
		t.Skip("skipping S3 client smoke check: neither mc nor rclone is installed; AWS SDK integration tests remain the automated compatibility gate")
	}
	if _, err := os.Stat(filepath.Join("..", "..", "cmd", "tgnas")); err != nil {
		t.Skipf("skipping S3 client smoke check: no cmd/tgnas service entrypoint or fake/local Telegram endpoint is available yet: %v", err)
	}
	t.Skip("skipping S3 client smoke check: local fake Telegram smoke harness is not available in this test package")
}
