// Package evidence provides pluggable storage backends for test execution
// evidence (logs, screenshots, coverage reports). The default local backend
// works with zero configuration. The S3 backend shells out to the aws CLI
// to avoid adding SDK dependencies.
package evidence

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Backend is the interface for evidence storage.
type Backend interface {
	// Upload stores a file at the given key. Returns the URI for retrieval.
	Upload(key string, r io.Reader) (uri string, err error)

	// Download retrieves a file by key.
	Download(key string, w io.Writer) error

	// List returns keys under a prefix.
	List(prefix string) ([]string, error)

	// URI returns the canonical URI for a key (without uploading).
	URI(key string) string

	// Delete removes a single key. Used for the preflight probe so
	// __health/preflight-* objects don't accumulate. Best-effort —
	// callers may ignore errors.
	Delete(key string) error
}

// Config holds evidence backend configuration.
type Config struct {
	Backend   string // "local" (default) or "s3"
	LocalDir  string // for local backend: directory path (default: .evidence)
	S3Bucket  string // for S3 backend: bucket name
	S3Region  string // for S3 backend: AWS region
	S3Prefix  string // for S3 backend: key prefix (optional)
	S3Profile string // for S3 backend: AWS CLI profile name (optional)
}

// New creates a Backend from configuration.
func New(cfg Config) (Backend, error) {
	switch cfg.Backend {
	case "", "local":
		dir := cfg.LocalDir
		if dir == "" {
			dir = ".evidence"
		}
		return &LocalBackend{Dir: dir}, nil
	case "s3":
		if cfg.S3Bucket == "" {
			return nil, fmt.Errorf("evidence.s3_bucket is required when backend is s3")
		}
		return &S3Backend{
			Bucket:  cfg.S3Bucket,
			Region:  cfg.S3Region,
			Prefix:  cfg.S3Prefix,
			Profile: cfg.S3Profile,
		}, nil
	default:
		return nil, fmt.Errorf("unknown evidence backend: %q (supported: local, s3)", cfg.Backend)
	}
}

// GzipUpload compresses data with gzip and uploads it with a .gz suffix.
// Use this for log files, JSON, and other compressible content.
func GzipUpload(b Backend, key string, data []byte) (string, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		return "", fmt.Errorf("gzip write: %w", err)
	}
	if err := gw.Close(); err != nil {
		return "", fmt.Errorf("gzip close: %w", err)
	}
	gzKey := key + ".gz"
	return b.Upload(gzKey, &buf)
}

// EvidenceKey builds the standard key for an evidence artifact.
// Pattern: <task-id>/<suite>/<sha>/<timestamp>/<filename>
func EvidenceKey(taskID, suite, sha string, filename string) string {
	ts := time.Now().Format("20060102T150405")
	return filepath.Join(taskID, suite, sha, ts, filename)
}

// EvidencePrefix returns the prefix for listing all evidence for a task+suite.
func EvidencePrefix(taskID, suite string) string {
	return taskID + "/" + suite + "/"
}

// --- Local Backend ---

// LocalBackend stores evidence on the local filesystem.
type LocalBackend struct {
	Dir string // root directory for evidence files
}

func (b *LocalBackend) Upload(key string, r io.Reader) (string, error) {
	path := filepath.Join(b.Dir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", fmt.Errorf("creating directory: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return "", fmt.Errorf("creating file: %w", err)
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return "", fmt.Errorf("writing file: %w", err)
	}
	return b.URI(key), nil
}

func (b *LocalBackend) Download(key string, w io.Writer) error {
	path := filepath.Join(b.Dir, filepath.FromSlash(key))
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

func (b *LocalBackend) List(prefix string) ([]string, error) {
	root := filepath.Join(b.Dir, filepath.FromSlash(prefix))
	var keys []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.SkipAll
			}
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(b.Dir, path)
		if err != nil {
			return err
		}
		keys = append(keys, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return keys, nil
}

func (b *LocalBackend) URI(key string) string {
	return "file://" + filepath.Join(b.Dir, filepath.FromSlash(key))
}

func (b *LocalBackend) Delete(key string) error {
	path := filepath.Join(b.Dir, filepath.FromSlash(key))
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// --- S3 Backend ---

// S3Backend stores evidence in an S3 bucket via the aws CLI.
// This avoids adding the AWS SDK as a dependency.
type S3Backend struct {
	Bucket  string
	Region  string
	Prefix  string
	Profile string // AWS CLI profile name (optional, passed as --profile)
	// RunCmd is injectable for testing. If nil, uses exec.Command.
	RunCmd func(args ...string) (string, error)
}

func (b *S3Backend) s3Key(key string) string {
	if b.Prefix != "" {
		return b.Prefix + "/" + key
	}
	return key
}

func (b *S3Backend) s3URI(key string) string {
	return fmt.Sprintf("s3://%s/%s", b.Bucket, b.s3Key(key))
}

func (b *S3Backend) Upload(key string, r io.Reader) (string, error) {
	// Write to temp file first (aws cli needs a file path)
	tmp, err := os.CreateTemp("", "st-evidence-*")
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	if _, err := io.Copy(tmp, r); err != nil {
		return "", fmt.Errorf("writing temp file: %w", err)
	}
	tmp.Close()

	args := []string{"s3", "cp", tmp.Name(), b.s3URI(key)}
	b.appendCommonFlags(&args)

	if _, err := b.run(args...); err != nil {
		return "", fmt.Errorf("aws s3 cp: %w", err)
	}
	return b.URI(key), nil
}

func (b *S3Backend) Download(key string, w io.Writer) error {
	tmp, err := os.CreateTemp("", "st-evidence-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	tmp.Close()

	args := []string{"s3", "cp", b.s3URI(key), tmp.Name()}
	b.appendCommonFlags(&args)

	if _, err := b.run(args...); err != nil {
		return fmt.Errorf("aws s3 cp: %w", err)
	}

	f, err := os.Open(tmp.Name())
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

func (b *S3Backend) List(prefix string) ([]string, error) {
	args := []string{"s3api", "list-objects-v2",
		"--bucket", b.Bucket,
		"--prefix", b.s3Key(prefix),
		"--query", "Contents[].Key",
		"--output", "text",
	}
	b.appendCommonFlags(&args)

	out, err := b.run(args...)
	if err != nil {
		return nil, fmt.Errorf("aws s3api list-objects-v2: %w", err)
	}

	var keys []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "None" {
			continue
		}
		// Strip prefix if configured
		if b.Prefix != "" {
			line = strings.TrimPrefix(line, b.Prefix+"/")
		}
		keys = append(keys, line)
	}
	return keys, nil
}

func (b *S3Backend) URI(key string) string {
	return b.s3URI(key)
}

func (b *S3Backend) Delete(key string) error {
	args := []string{"s3", "rm", b.s3URI(key)}
	b.appendCommonFlags(&args)
	if _, err := b.run(args...); err != nil {
		return fmt.Errorf("aws s3 rm: %w", err)
	}
	return nil
}

func (b *S3Backend) appendCommonFlags(args *[]string) {
	if b.Region != "" {
		*args = append(*args, "--region", b.Region)
	}
	// I-507: when the agent has its own per-session env-var
	// credentials, do NOT forward `--profile`. The AWS CLI honours
	// `--profile` above the env-var creds, which would silently
	// defeat the env-clearing override in runAWSCLI and re-attempt
	// the operator's stale profile. The Profile knob still applies
	// for developers running st test --run from their own shell
	// (no AWS_ACCESS_KEY_ID set).
	if b.Profile != "" && !HasAgentCredentials() {
		*args = append(*args, "--profile", b.Profile)
	}
}

func (b *S3Backend) run(args ...string) (string, error) {
	if b.RunCmd != nil {
		return b.RunCmd(args...)
	}
	return runAWSCLI(args...)
}
