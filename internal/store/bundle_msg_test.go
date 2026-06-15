package store

import (
	"strings"
	"testing"
)

func TestMatchesItemID(t *testing.T) {
	cases := []struct {
		base string
		id   string
		want bool
	}{
		{"T-123-foo-bar.md", "T-123", true},
		{"T-123.md", "T-123", true},
		{"I-456-long-title-with-words.md", "I-456", true},
		{"T-123-foo.md", "T-124", false},
		{"T-1234-foo.md", "T-123", false}, // prefix but different id
		{"T-123-foo.md", "T-12", false},   // partial match should not count
		{"config.yaml", "T-123", false},
		{"T-123-foo.md", "", false},
	}
	for _, c := range cases {
		got := matchesItemID(c.base, c.id)
		if got != c.want {
			t.Errorf("matchesItemID(%q, %q) = %v, want %v", c.base, c.id, got, c.want)
		}
	}
}

func TestSynthesizeBundleMessage_NoChange(t *testing.T) {
	// Only the expected item file is staged → message unchanged.
	cached := "tasks/T-100-alpha.md"
	msg := synthesizeBundleMessage("st update: T-100.sbar.situation", cached)
	if msg != "st update: T-100.sbar.situation" {
		t.Errorf("expected unchanged message, got %q", msg)
	}
}

func TestSynthesizeBundleMessage_ExtraItems(t *testing.T) {
	// Two unexpected item files staged alongside the expected one.
	cached := "tasks/T-100-alpha.md\nissues/I-200-beta.md\ntasks/T-300-gamma.md"
	msg := synthesizeBundleMessage("st update: T-100.sbar.situation", cached)

	if !strings.HasPrefix(msg, "st sync batch: ") {
		t.Errorf("expected bundle message prefix, got %q", msg)
	}
	// All three files must appear in the bundle message.
	for _, want := range []string{"T-100-alpha.md", "I-200-beta.md", "T-300-gamma.md"} {
		if !strings.Contains(msg, want) {
			t.Errorf("bundle message missing %q: %q", want, msg)
		}
	}
}

func TestSynthesizeBundleMessage_OnlyAutoStage(t *testing.T) {
	// Only auto-stage subdirs alongside the expected item — no cross-attribution.
	cached := "tasks/T-100-alpha.md\n.plans/I-594.md\n.changelog/2026-06-14.md"
	msg := synthesizeBundleMessage("st update: T-100.sbar.situation", cached)
	if msg != "st update: T-100.sbar.situation" {
		t.Errorf("auto-stage dirs should not trigger bundle, got %q", msg)
	}
}

func TestSynthesizeBundleMessage_NonUpdateMessage(t *testing.T) {
	// Non "st update:" message — function is a no-op.
	cached := "tasks/T-100-alpha.md\ntasks/T-200-beta.md"
	msg := synthesizeBundleMessage("st sync", cached)
	if msg != "st sync" {
		t.Errorf("non-update message should pass through unchanged, got %q", msg)
	}
}

func TestSynthesizeBundleMessage_SingleItemMultipleAutoStage(t *testing.T) {
	// Multiple .plans files staged with one item update — still fine.
	cached := "issues/I-594-foo.md\n.plans/I-594.md\n.as/sessions/abc.yaml"
	msg := synthesizeBundleMessage("st update: I-594.sbar.assessment", cached)
	if msg != "st update: I-594.sbar.assessment" {
		t.Errorf("auto-stage dirs should not trigger bundle, got %q", msg)
	}
}
