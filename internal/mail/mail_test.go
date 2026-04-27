package mail

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
)

func setupMailTestCfg(t *testing.T) *config.Config {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".as"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

func TestSendCreatesFileAndReturnsID(t *testing.T) {
	cfg := setupMailTestCfg(t)
	id, err := Send(cfg, Message{
		From: "agent-a-1",
		To:   "agent-b-1",
		Kind: KindWarning,
		Body: "auth middleware will conflict with your branch",
		Item: "T-300",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if id == "" {
		t.Fatal("Send returned empty id")
	}
	if !strings.Contains(id, "from-agent-a-1") || !strings.Contains(id, string(KindWarning)) {
		t.Errorf("id missing expected components: %q", id)
	}
	path := filepath.Join(MailboxDir(cfg, "agent-b-1"), id+".yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written message: %v", err)
	}
	for _, want := range []string{
		"from: agent-a-1",
		"to: agent-b-1",
		"kind: warning",
		"item: T-300",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("file missing %q:\n%s", want, body)
		}
	}
}

func TestSendValidation(t *testing.T) {
	cfg := setupMailTestCfg(t)
	cases := []struct {
		name string
		msg  Message
		want string
	}{
		{"missing To", Message{From: "a", Kind: KindWarning, Body: "x"}, "To required"},
		{"missing From", Message{To: "b", Kind: KindWarning, Body: "x"}, "From required"},
		{"missing Body", Message{From: "a", To: "b", Kind: KindWarning}, "Body required"},
		{"unknown Kind", Message{From: "a", To: "b", Kind: "shout", Body: "x"}, "unknown kind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Send(cfg, tc.msg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestList(t *testing.T) {
	cfg := setupMailTestCfg(t)
	for i, kind := range []Kind{KindWarning, KindNeedHelp, KindAlert} {
		_, err := Send(cfg, Message{
			From: "agent-a-1",
			To:   "agent-b-1",
			Kind: kind,
			Body: "msg" + string(rune('0'+i)),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	msgs, err := List(cfg, "agent-b-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	// Sorted by id (timestamp-prefixed) — verify monotonic ordering.
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	sortedCopy := append([]string(nil), ids...)
	sort.Strings(sortedCopy)
	for i := range ids {
		if ids[i] != sortedCopy[i] {
			t.Errorf("List not sorted: %v", ids)
			break
		}
	}
	// Empty mailbox returns empty slice + no error.
	none, err := List(cfg, "agent-nobody")
	if err != nil {
		t.Errorf("List on empty mailbox should not error: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("expected no messages for unknown agent, got %d", len(none))
	}
}

func TestArchive(t *testing.T) {
	cfg := setupMailTestCfg(t)
	id, err := Send(cfg, Message{From: "a", To: "b", Kind: KindWarning, Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Archive(cfg, "b", id); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	// Source gone, archive copy exists.
	if _, err := os.Stat(filepath.Join(MailboxDir(cfg, "b"), id+".yaml")); !os.IsNotExist(err) {
		t.Errorf("source file should be gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ArchiveDir(cfg, "b"), id+".yaml")); err != nil {
		t.Errorf("archive copy missing: %v", err)
	}
	// List skips the archive folder.
	msgs, _ := List(cfg, "b")
	if len(msgs) != 0 {
		t.Errorf("List should exclude archive, got %d", len(msgs))
	}
	// Archive of missing id surfaces a helpful error.
	if err := Archive(cfg, "b", "nope"); err == nil {
		t.Error("expected error archiving missing id, got nil")
	}
}

func TestShow(t *testing.T) {
	cfg := setupMailTestCfg(t)
	id, _ := Send(cfg, Message{From: "a", To: "b", Kind: KindRequest, Body: "review please"})

	// Pending.
	got, err := Show(cfg, "b", id)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Body != "review please" || got.Kind != KindRequest {
		t.Errorf("Show pending lost fields: %+v", got)
	}

	// Show does NOT consume.
	if _, err := os.Stat(filepath.Join(MailboxDir(cfg, "b"), id+".yaml")); err != nil {
		t.Errorf("Show should not move file: %v", err)
	}

	// Archive then show — must still be findable.
	if err := Archive(cfg, "b", id); err != nil {
		t.Fatal(err)
	}
	got, err = Show(cfg, "b", id)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Body != "review please" {
		t.Errorf("Show archived lost fields: %+v", got)
	}

	// Unknown id → (nil, nil), not an error.
	got, err = Show(cfg, "b", "nope")
	if err != nil {
		t.Errorf("missing id should not error: %v", err)
	}
	if got != nil {
		t.Errorf("missing id should return nil, got %+v", got)
	}
}

// Poll returns pending messages and archives them as a side effect —
// the consume-on-display rule. A subsequent Poll on the same mailbox
// returns nothing.
func TestPollConsumes(t *testing.T) {
	cfg := setupMailTestCfg(t)
	for i := 0; i < 3; i++ {
		_, _ = Send(cfg, Message{From: "a", To: "b", Kind: KindWarning, Body: "x" + string(rune('0'+i))})
	}
	first, err := Poll(cfg, "b")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 {
		t.Errorf("first poll should return 3, got %d", len(first))
	}
	second, err := Poll(cfg, "b")
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Errorf("second poll should be empty, got %d", len(second))
	}
	// All three should be in archive now.
	entries, _ := os.ReadDir(ArchiveDir(cfg, "b"))
	count := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".yaml") {
			count++
		}
	}
	if count != 3 {
		t.Errorf("expected 3 files in archive, got %d", count)
	}
}

func TestKindIsBlocking(t *testing.T) {
	for _, k := range []Kind{KindAlert, KindPause} {
		if !k.IsBlocking() {
			t.Errorf("%s should be blocking", k)
		}
	}
	for _, k := range []Kind{KindWarning, KindNeedHelp, KindRequest, KindResume} {
		if k.IsBlocking() {
			t.Errorf("%s should not be blocking", k)
		}
	}
}

func TestQuoteAndUnquoteRoundtrip(t *testing.T) {
	cfg := setupMailTestCfg(t)
	tricky := `body with: colon, "quotes", and a # hash`
	id, err := Send(cfg, Message{From: "a", To: "b", Kind: KindWarning, Body: tricky})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Show(cfg, "b", id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != tricky {
		t.Errorf("body lost in round-trip:\nwant %q\ngot  %q", tricky, got.Body)
	}
}
