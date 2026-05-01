package evidence

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- Factory ---

func TestNewLocal(t *testing.T) {
	b, err := New(Config{Backend: "local", LocalDir: "/tmp/test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lb, ok := b.(*LocalBackend)
	if !ok {
		t.Fatal("expected LocalBackend")
	}
	if lb.Dir != "/tmp/test" {
		t.Errorf("Dir = %q", lb.Dir)
	}
}

func TestNewLocalDefault(t *testing.T) {
	b, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lb, ok := b.(*LocalBackend)
	if !ok {
		t.Fatal("expected LocalBackend")
	}
	if lb.Dir != ".evidence" {
		t.Errorf("Dir = %q, want .evidence", lb.Dir)
	}
}

func TestNewS3(t *testing.T) {
	b, err := New(Config{Backend: "s3", S3Bucket: "my-bucket", S3Region: "us-east-1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sb, ok := b.(*S3Backend)
	if !ok {
		t.Fatal("expected S3Backend")
	}
	if sb.Bucket != "my-bucket" || sb.Region != "us-east-1" {
		t.Errorf("S3Backend = %+v", sb)
	}
}

func TestNewS3NoBucket(t *testing.T) {
	_, err := New(Config{Backend: "s3"})
	if err == nil {
		t.Error("expected error when s3_bucket missing")
	}
}

func TestNewUnknown(t *testing.T) {
	_, err := New(Config{Backend: "gcs"})
	if err == nil {
		t.Error("expected error for unknown backend")
	}
}

// --- Local Backend ---

func TestLocalUploadDownload(t *testing.T) {
	dir := t.TempDir()
	b := &LocalBackend{Dir: dir}

	content := "test log output\nline 2\n"
	uri, err := b.Upload("T-001/api_unit/abc1234/20260326T100000/log.txt", strings.NewReader(content))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if !strings.HasPrefix(uri, "file://") {
		t.Errorf("URI = %q, want file:// prefix", uri)
	}

	// Verify file exists on disk
	path := filepath.Join(dir, "T-001", "api_unit", "abc1234", "20260326T100000", "log.txt")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != content {
		t.Errorf("content = %q", string(data))
	}

	// Download
	var buf bytes.Buffer
	if err := b.Download("T-001/api_unit/abc1234/20260326T100000/log.txt", &buf); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if buf.String() != content {
		t.Errorf("downloaded = %q", buf.String())
	}
}

func TestLocalList(t *testing.T) {
	dir := t.TempDir()
	b := &LocalBackend{Dir: dir}

	// Upload several files
	for _, name := range []string{"log.txt", "summary.json"} {
		key := fmt.Sprintf("T-001/api_unit/abc1234/20260326T100000/%s", name)
		_, err := b.Upload(key, strings.NewReader("content"))
		if err != nil {
			t.Fatalf("Upload %s: %v", name, err)
		}
	}

	// List all under T-001/api_unit/
	keys, err := b.List("T-001/api_unit/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("keys = %v, want 2", keys)
	}
	for _, k := range keys {
		if !strings.HasPrefix(k, "T-001/api_unit/") {
			t.Errorf("key %q doesn't have expected prefix", k)
		}
	}
}

func TestLocalListEmpty(t *testing.T) {
	dir := t.TempDir()
	b := &LocalBackend{Dir: dir}

	keys, err := b.List("T-999/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("keys = %v, want empty", keys)
	}
}

func TestLocalDownloadNotFound(t *testing.T) {
	dir := t.TempDir()
	b := &LocalBackend{Dir: dir}

	var buf bytes.Buffer
	err := b.Download("nonexistent/key", &buf)
	if err == nil {
		t.Error("expected error for nonexistent key")
	}
}

func TestLocalURI(t *testing.T) {
	b := &LocalBackend{Dir: "/data/evidence"}
	uri := b.URI("T-001/api_unit/abc/ts/log.txt")
	if !strings.Contains(uri, "file://") || !strings.Contains(uri, "T-001") {
		t.Errorf("URI = %q", uri)
	}
}

// --- S3 Backend (mocked) ---

func TestS3Upload(t *testing.T) {
	var capturedArgs []string
	b := &S3Backend{
		Bucket: "my-bucket",
		Region: "us-west-2",
		Prefix: "evidence",
		RunCmd: func(args ...string) (string, error) {
			capturedArgs = args
			return "", nil
		},
	}

	uri, err := b.Upload("T-001/api_unit/abc/ts/log.txt", strings.NewReader("log content"))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if uri != "s3://my-bucket/evidence/T-001/api_unit/abc/ts/log.txt" {
		t.Errorf("URI = %q", uri)
	}

	// Verify aws CLI args
	if len(capturedArgs) < 4 {
		t.Fatalf("args = %v", capturedArgs)
	}
	if capturedArgs[0] != "s3" || capturedArgs[1] != "cp" {
		t.Errorf("args[0:2] = %v", capturedArgs[:2])
	}
	// args[2] is temp file path
	if capturedArgs[3] != "s3://my-bucket/evidence/T-001/api_unit/abc/ts/log.txt" {
		t.Errorf("s3 dest = %q", capturedArgs[3])
	}
	// Check region flag
	found := false
	for i, a := range capturedArgs {
		if a == "--region" && i+1 < len(capturedArgs) && capturedArgs[i+1] == "us-west-2" {
			found = true
		}
	}
	if !found {
		t.Errorf("missing --region flag in %v", capturedArgs)
	}
}

func TestS3UploadNoPrefix(t *testing.T) {
	var capturedDest string
	b := &S3Backend{
		Bucket: "my-bucket",
		RunCmd: func(args ...string) (string, error) {
			if len(args) >= 4 {
				capturedDest = args[3]
			}
			return "", nil
		},
	}

	_, err := b.Upload("T-001/log.txt", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if capturedDest != "s3://my-bucket/T-001/log.txt" {
		t.Errorf("dest = %q, want no prefix", capturedDest)
	}
}

func TestS3UploadError(t *testing.T) {
	b := &S3Backend{
		Bucket: "my-bucket",
		RunCmd: func(args ...string) (string, error) {
			return "", fmt.Errorf("access denied")
		},
	}

	_, err := b.Upload("key", strings.NewReader("x"))
	if err == nil {
		t.Error("expected error")
	}
}

func TestS3Download(t *testing.T) {
	b := &S3Backend{
		Bucket: "my-bucket",
		RunCmd: func(args ...string) (string, error) {
			// Simulate download by writing to the temp file destination
			if len(args) >= 4 && args[1] == "cp" {
				dest := args[3]
				os.WriteFile(dest, []byte("downloaded content"), 0644)
			}
			return "", nil
		},
	}

	var buf bytes.Buffer
	err := b.Download("T-001/log.txt", &buf)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if buf.String() != "downloaded content" {
		t.Errorf("content = %q", buf.String())
	}
}

func TestS3List(t *testing.T) {
	b := &S3Backend{
		Bucket: "my-bucket",
		Prefix: "ev",
		RunCmd: func(args ...string) (string, error) {
			return "ev/T-001/api_unit/abc/log.txt\nev/T-001/api_unit/abc/summary.json\n", nil
		},
	}

	keys, err := b.List("T-001/api_unit/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("keys = %v", keys)
	}
	// Prefix should be stripped
	for _, k := range keys {
		if strings.HasPrefix(k, "ev/") {
			t.Errorf("key %q still has prefix", k)
		}
	}
}

func TestS3ListEmpty(t *testing.T) {
	b := &S3Backend{
		Bucket: "my-bucket",
		RunCmd: func(args ...string) (string, error) {
			return "None\n", nil
		},
	}

	keys, err := b.List("T-999/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("keys = %v, want empty", keys)
	}
}

func TestS3URI(t *testing.T) {
	b := &S3Backend{Bucket: "my-bucket", Prefix: "evidence"}
	uri := b.URI("T-001/log.txt")
	if uri != "s3://my-bucket/evidence/T-001/log.txt" {
		t.Errorf("URI = %q", uri)
	}
}

func TestS3DownloadError(t *testing.T) {
	b := &S3Backend{
		Bucket: "my-bucket",
		RunCmd: func(args ...string) (string, error) {
			return "", fmt.Errorf("not found")
		},
	}
	var buf bytes.Buffer
	err := b.Download("key", &buf)
	if err == nil {
		t.Error("expected error")
	}
}

func TestS3ListError(t *testing.T) {
	b := &S3Backend{
		Bucket: "my-bucket",
		RunCmd: func(args ...string) (string, error) {
			return "", fmt.Errorf("access denied")
		},
	}
	_, err := b.List("prefix/")
	if err == nil {
		t.Error("expected error")
	}
}

func TestLocalUploadBadDir(t *testing.T) {
	// Use a path that can't be created
	b := &LocalBackend{Dir: "/dev/null/impossible"}
	_, err := b.Upload("key", strings.NewReader("x"))
	if err == nil {
		t.Error("expected error for impossible dir")
	}
}

func TestS3NoRegion(t *testing.T) {
	var capturedArgs []string
	b := &S3Backend{
		Bucket: "my-bucket",
		RunCmd: func(args ...string) (string, error) {
			capturedArgs = args
			return "", nil
		},
	}
	b.Upload("key", strings.NewReader("x"))
	// Should NOT have --region flag
	for _, a := range capturedArgs {
		if a == "--region" {
			t.Error("should not have --region when Region is empty")
		}
	}
}

// runAWSCLI is a documented coverage exception — it's a thin exec.Command
// wrapper that can't be unit tested without the aws CLI installed.

// --- Key helpers ---

func TestEvidenceKey(t *testing.T) {
	key := EvidenceKey("T-001", "api_unit", "abc1234", "log.txt")
	parts := strings.Split(key, string(filepath.Separator))
	if len(parts) != 5 {
		t.Fatalf("key = %q, want 5 parts", key)
	}
	if parts[0] != "T-001" || parts[1] != "api_unit" || parts[2] != "abc1234" || parts[4] != "log.txt" {
		t.Errorf("key = %q", key)
	}
	// parts[3] is timestamp — verify format
	if len(parts[3]) != 15 { // 20060102T150405
		t.Errorf("timestamp = %q, want 15 chars", parts[3])
	}
}

func TestEvidencePrefix(t *testing.T) {
	prefix := EvidencePrefix("T-001", "api_unit")
	if prefix != "T-001/api_unit/" {
		t.Errorf("prefix = %q", prefix)
	}
}

// I-507 (review fix): when agent env-var creds are present
// (AWS_ACCESS_KEY_ID set), appendCommonFlags must NOT forward
// --profile. The AWS CLI honours --profile above the env-var
// override, which would silently defeat the env-clearing.
func TestS3AppendCommonFlags_SkipsProfileWhenAgentCreds(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIA...")
	b := &S3Backend{Region: "us-east-1", Profile: "operator-profile"}
	var args []string
	b.appendCommonFlags(&args)
	for i, a := range args {
		if a == "--profile" {
			t.Errorf("--profile should be suppressed when agent creds present, got args=%v at index %d", args, i)
		}
	}
}

// I-507 (review fix): without agent creds (developer flow), the
// existing --profile passthrough behavior is preserved.
func TestS3AppendCommonFlags_PassesProfileWhenNoAgentCreds(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	b := &S3Backend{Region: "us-east-1", Profile: "operator-profile"}
	var args []string
	b.appendCommonFlags(&args)
	found := false
	for i, a := range args {
		if a == "--profile" && i+1 < len(args) && args[i+1] == "operator-profile" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected --profile operator-profile in developer flow, got %v", args)
	}
}
