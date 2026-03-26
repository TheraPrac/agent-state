package namegen

import (
	"strings"
	"testing"
)

func TestGenerateFormat(t *testing.T) {
	for i := 0; i < 50; i++ {
		id := Generate()
		parts := strings.Split(id, "-")
		if len(parts) != 3 {
			t.Fatalf("expected 3 parts, got %d: %q", len(parts), id)
		}
		for _, p := range parts {
			if p == "" {
				t.Fatalf("empty word in ID: %q", id)
			}
		}
	}
}

func TestGenerateDistribution(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		seen[Generate()] = true
	}
	if len(seen) < 180 {
		t.Errorf("poor distribution: only %d unique out of 200", len(seen))
	}
}

func TestGenerateUnique(t *testing.T) {
	existing := make([]string, 0, 50)
	for i := 0; i < 50; i++ {
		existing = append(existing, Generate())
	}

	id := GenerateUnique(existing)
	for _, e := range existing {
		if id == e {
			t.Fatalf("GenerateUnique returned existing ID: %s", id)
		}
	}
}

func TestGenerateUniqueEmptyExisting(t *testing.T) {
	id := GenerateUnique(nil)
	if id == "" {
		t.Fatal("expected non-empty ID")
	}
	parts := strings.Split(id, "-")
	if len(parts) != 3 {
		t.Fatalf("expected 3 parts, got %d: %q", len(parts), id)
	}
}
