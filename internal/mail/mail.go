// Package mail implements the inter-agent mailbox: small YAML messages
// stored at .as/mailbox/<recipient>/<id>.yaml, consumed by moving to
// .as/mailbox/<recipient>/archive/. Mail communicates things that
// cannot be inferred from item state (warnings, requests, alerts) —
// item state is the source of truth for "what's the work". T-313.
package mail

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
)

// Kind enumerates the message taxonomy. Receiver behavior in st run's
// polling loop is keyed off this field — alert/pause stop the pipeline,
// warning/request/need_help/resume are banner-only.
type Kind string

const (
	KindWarning  Kind = "warning"   // informational FYI, may affect your work
	KindNeedHelp Kind = "need_help" // I'm blocked, someone pick up
	KindRequest  Kind = "request"   // code review, opinion, etc.
	KindAlert    Kind = "alert"     // stop everything, critical
	KindPause    Kind = "pause"     // stop touching this repo
	KindResume   Kind = "resume"    // OK to continue
)

// IsValidKind reports whether k is one of the recognized kinds.
func IsValidKind(k string) bool {
	switch Kind(k) {
	case KindWarning, KindNeedHelp, KindRequest, KindAlert, KindPause, KindResume:
		return true
	}
	return false
}

// IsBlocking reports whether a message of this kind should HALT the
// pipeline when surfaced by st run's poller. Non-blocking kinds print
// a banner and let the pipeline continue.
func (k Kind) IsBlocking() bool {
	return k == KindAlert || k == KindPause
}

// Message is one mailbox entry. The on-disk format is YAML; field
// names match the YAML keys (key `kind`, not `type`, to avoid the
// parser/CLAUDE.md collision that bit I-394).
type Message struct {
	ID   string `yaml:"-"` // filename stem (without .yaml), set on read
	From string `yaml:"from"`
	To   string `yaml:"to"`
	Kind Kind   `yaml:"kind"`
	At   string `yaml:"at"`             // RFC3339
	Body string `yaml:"body"`
	Item string `yaml:"item,omitempty"` // optional related item id
}

// MailboxDir returns the on-disk path to a recipient's pending mailbox
// (without the trailing /archive). Centralized so the layout is
// changed in one place if needed.
func MailboxDir(cfg *config.Config, recipient string) string {
	return filepath.Join(cfg.Root(), ".as", "mailbox", recipient)
}

// ArchiveDir returns the path where consumed messages move to.
func ArchiveDir(cfg *config.Config, recipient string) string {
	return filepath.Join(MailboxDir(cfg, recipient), "archive")
}

// Send writes a new message into recipient's mailbox. Returns the
// generated message id (the filename stem), or an error if the kind
// is unknown, the recipient is empty, or the disk write fails.
//
// Filename shape: <utc-timestamp>-from-<from>-<kind>.yaml. Sortable,
// human-readable, collision-free in practice (timestamp is to-the-
// nanosecond).
func Send(cfg *config.Config, msg Message) (string, error) {
	if msg.To == "" {
		return "", errors.New("mail.Send: To required")
	}
	if msg.From == "" {
		return "", errors.New("mail.Send: From required")
	}
	if !IsValidKind(string(msg.Kind)) {
		return "", fmt.Errorf("mail.Send: unknown kind %q", msg.Kind)
	}
	if msg.Body == "" {
		return "", errors.New("mail.Send: Body required")
	}
	if msg.At == "" {
		msg.At = time.Now().Format(time.RFC3339)
	}

	dir := MailboxDir(cfg, msg.To)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("mail.Send: mkdir %s: %w", dir, err)
	}

	// Use UTC + nanos to keep ids deterministic and globally sortable
	// regardless of which agent's clock writes. Sanitize the from-id
	// for filesystem safety.
	stamp := time.Now().UTC().Format("2006-01-02T15-04-05.000000000Z")
	id := fmt.Sprintf("%s-from-%s-%s", stamp, sanitizeForFilename(msg.From), msg.Kind)
	path := filepath.Join(dir, id+".yaml")

	if err := writeMessage(path, msg); err != nil {
		return "", fmt.Errorf("mail.Send: write %s: %w", path, err)
	}
	return id, nil
}

// List returns pending messages for recipient (newest last — sorted by
// id, which is timestamp-prefixed). The archive subdirectory is
// excluded so callers see only what's actionable.
func List(cfg *config.Config, recipient string) ([]Message, error) {
	dir := MailboxDir(cfg, recipient)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Message
	for _, e := range entries {
		if e.IsDir() {
			continue // archive/
		}
		if !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			fmt.Fprintf(os.Stderr, "mail.List: %s: %v\n", e.Name(), err)
			continue
		}
		msg, err := parseMessage(body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mail.List: parse %s: %v\n", e.Name(), err)
			continue
		}
		msg.ID = strings.TrimSuffix(e.Name(), ".yaml")
		out = append(out, *msg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Show loads a single message by id from recipient's pending mailbox
// or archive. Returns (nil, nil) when no file matches.
//
// `st mail show` does NOT consume the message — only Archive does.
func Show(cfg *config.Config, recipient, id string) (*Message, error) {
	candidates := []string{
		filepath.Join(MailboxDir(cfg, recipient), id+".yaml"),
		filepath.Join(ArchiveDir(cfg, recipient), id+".yaml"),
	}
	for _, path := range candidates {
		body, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		msg, err := parseMessage(body)
		if err != nil {
			return nil, err
		}
		msg.ID = id
		return msg, nil
	}
	return nil, nil
}

// Archive moves a pending message into the archive subdirectory. This
// is the read-receipt operation — used by st run's poller after
// surfacing a message and by `st mail archive` for manual consumption.
func Archive(cfg *config.Config, recipient, id string) error {
	src := filepath.Join(MailboxDir(cfg, recipient), id+".yaml")
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("mail.Archive: %s: not found", id)
		}
		return err
	}
	dst := filepath.Join(ArchiveDir(cfg, recipient), id+".yaml")
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("mail.Archive: mkdir: %w", err)
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("mail.Archive: rename: %w", err)
	}
	return nil
}

// Poll returns pending messages for recipient and archives each one
// as it's returned. Designed for st run's between-step poll: surface
// new messages once, then move them to archive so the next poll
// doesn't re-surface them. T-313's "consume on display" rule.
//
// Errors archiving an individual message are logged but don't drop
// the message from the returned slice — the operator still sees it.
func Poll(cfg *config.Config, recipient string) ([]Message, error) {
	msgs, err := List(cfg, recipient)
	if err != nil {
		return nil, err
	}
	for _, m := range msgs {
		if err := Archive(cfg, recipient, m.ID); err != nil {
			fmt.Fprintf(os.Stderr, "mail.Poll: archive %s: %v\n", m.ID, err)
		}
	}
	return msgs, nil
}

// --- serialization (hand-rolled YAML, stable schema) ---

func writeMessage(path string, msg Message) error {
	var b strings.Builder
	fmt.Fprintf(&b, "from: %s\n", msg.From)
	fmt.Fprintf(&b, "to: %s\n", msg.To)
	fmt.Fprintf(&b, "kind: %s\n", msg.Kind)
	fmt.Fprintf(&b, "at: %s\n", msg.At)
	fmt.Fprintf(&b, "body: %s\n", quoteScalar(msg.Body))
	if msg.Item != "" {
		fmt.Fprintf(&b, "item: %s\n", msg.Item)
	}
	return os.WriteFile(path, []byte(b.String()), 0644)
}

func parseMessage(body []byte) (*Message, error) {
	msg := &Message{}
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		val = unquoteScalar(val)
		switch key {
		case "from":
			msg.From = val
		case "to":
			msg.To = val
		case "kind":
			msg.Kind = Kind(val)
		case "at":
			msg.At = val
		case "body":
			msg.Body = val
		case "item":
			msg.Item = val
		}
	}
	if msg.From == "" || msg.To == "" || msg.Kind == "" {
		return nil, fmt.Errorf("mail.parseMessage: missing required field (from/to/kind)")
	}
	return msg, nil
}

// quoteScalar wraps a body in double quotes when it contains anything
// that would break a single-line YAML scalar (newline, leading dash,
// embedded quote). Keeps the simple case readable.
func quoteScalar(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, "\n:\"'#&*?|>!%@`") || strings.HasPrefix(s, "-") || strings.HasPrefix(s, " ") {
		return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
	}
	return s
}

func unquoteScalar(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		inner := s[1 : len(s)-1]
		inner = strings.ReplaceAll(inner, `\"`, `"`)
		inner = strings.ReplaceAll(inner, `\\`, `\`)
		return inner
	}
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return s[1 : len(s)-1]
	}
	return s
}

var sanitizeFilenameRE = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func sanitizeForFilename(s string) string {
	return sanitizeFilenameRE.ReplaceAllString(s, "_")
}
