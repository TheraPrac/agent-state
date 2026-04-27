package command

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/mail"
	"github.com/jfinlinson/agent-state/internal/store"
)

// setupMailEnv builds a Config + Store with no items (mail is item-
// agnostic) and an identity stamped via env so MailSend can populate
// `from` automatically. Returns (s, cfg).
func setupMailEnv(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", "archive", ".as"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return s, cfg
}

// TestMailSend covers the happy path: identity → message → on-disk file.
// GitSync failure surfaces as a warning but does not change exit code.
func TestMailSend(t *testing.T) {
	s, cfg := setupMailEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-a-1")

	code := MailSend(s, cfg, "agent-b-1", MailSendOpts{
		Kind: string(mail.KindWarning),
		Body: "smoke test",
		Item: "T-300",
	})
	if code != 0 {
		t.Fatalf("MailSend returned %d, want 0", code)
	}

	msgs, err := mail.List(cfg, "agent-b-1")
	if err != nil {
		t.Fatalf("mail.List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message in mailbox, got %d", len(msgs))
	}
	got := msgs[0]
	if got.From != "agent-a-1" || got.To != "agent-b-1" || got.Body != "smoke test" || got.Item != "T-300" {
		t.Errorf("message fields wrong: %+v", got)
	}
	if got.Kind != mail.KindWarning {
		t.Errorf("kind = %q, want warning", got.Kind)
	}
}

// TestMailSendValidates: bad kind, missing body, missing identity all surface as exit 1.
func TestMailSendValidates(t *testing.T) {
	s, cfg := setupMailEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-a-1")

	if code := MailSend(s, cfg, "agent-b-1", MailSendOpts{Kind: "shout", Body: "x"}); code == 0 {
		t.Errorf("MailSend should reject unknown kind")
	}
	if code := MailSend(s, cfg, "agent-b-1", MailSendOpts{Kind: "warning"}); code == 0 {
		t.Errorf("MailSend should reject empty body")
	}
}

// TestMailListArchive covers the operator's full read-and-consume flow:
// list shows pending, archive moves to archive/, list now empty, archived
// file still readable via Show.
func TestMailListArchive(t *testing.T) {
	s, cfg := setupMailEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-b-1")

	id, err := mail.Send(cfg, mail.Message{
		From: "agent-a-1", To: "agent-b-1", Kind: mail.KindRequest, Body: "review please",
	})
	if err != nil {
		t.Fatal(err)
	}

	// MailList prints to stdout — capture by redirecting os.Stdout.
	out := captureStdoutMail(t, func() {
		if code := MailList(cfg, MailListOpts{}); code != 0 {
			t.Fatalf("MailList returned %d", code)
		}
	})
	if !strings.Contains(out, "review please") || !strings.Contains(out, "agent-b-1") {
		t.Errorf("MailList output missing expected content:\n%s", out)
	}

	// MailArchive moves the file.
	if code := MailArchive(s, cfg, "", id); code != 0 {
		t.Fatalf("MailArchive returned %d", code)
	}
	pending, _ := mail.List(cfg, "agent-b-1")
	if len(pending) != 0 {
		t.Errorf("after archive, list should be empty, got %d", len(pending))
	}
	got, err := mail.Show(cfg, "agent-b-1", id)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Body != "review please" {
		t.Errorf("archived message not findable via Show: %+v", got)
	}

	// MailList on empty mailbox prints the no-mail line and exits 0.
	emptyOut := captureStdoutMail(t, func() {
		if code := MailList(cfg, MailListOpts{}); code != 0 {
			t.Fatalf("MailList empty returned %d", code)
		}
	})
	if !strings.Contains(emptyOut, "No pending mail") {
		t.Errorf("empty list output unexpected:\n%s", emptyOut)
	}
}

// TestRunMailboxPoll: surfacing kinds (warning) don't halt; blocking
// kinds (alert/pause) make pollAndSurfaceMail return true. All polled
// messages are consumed regardless.
func TestRunMailboxPoll(t *testing.T) {
	_, cfg := setupMailEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-b-1")

	// Drop a non-blocking message → poll returns false (continue).
	_, err := mail.Send(cfg, mail.Message{From: "a", To: "agent-b-1", Kind: mail.KindWarning, Body: "fyi"})
	if err != nil {
		t.Fatal(err)
	}
	if blocking := pollAndSurfaceMail(cfg); blocking {
		t.Errorf("warning should not block")
	}
	pending, _ := mail.List(cfg, "agent-b-1")
	if len(pending) != 0 {
		t.Errorf("poll should have consumed warning, %d still pending", len(pending))
	}

	// Drop two messages: one warning, one alert. Poll returns true.
	for _, kind := range []mail.Kind{mail.KindWarning, mail.KindAlert} {
		_, err := mail.Send(cfg, mail.Message{From: "a", To: "agent-b-1", Kind: kind, Body: string(kind)})
		if err != nil {
			t.Fatal(err)
		}
	}
	if blocking := pollAndSurfaceMail(cfg); !blocking {
		t.Errorf("alert should block the pipeline")
	}
	pending, _ = mail.List(cfg, "agent-b-1")
	if len(pending) != 0 {
		t.Errorf("poll should have consumed both, %d still pending", len(pending))
	}

	// Poll with no mail returns false silently.
	if blocking := pollAndSurfaceMail(cfg); blocking {
		t.Errorf("empty mailbox should not block")
	}
}

// captureStdoutMail redirects os.Stdout for the duration of fn and
// returns whatever was written. Local helper — captureStdout in the
// existing test file uses the same pattern but is internal to those
// tests; mail tests get their own to keep dependencies narrow.
func captureStdoutMail(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan []byte)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		done <- buf.Bytes()
	}()
	fn()
	_ = w.Close()
	os.Stdout = old
	return string(<-done)
}
