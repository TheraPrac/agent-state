package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestParseStatusZ pins the shared NUL tokenizer + rename pairing (I-1621) that
// both checkNonStateGate and ClearStagedNonState now consume. Each token is the
// porcelain -z form `XY<space>PATH`, NUL-terminated; rename/copy entries put the
// OLD path in the following NUL token.
func TestParseStatusZ(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []StatusEntry
	}{
		{
			name: "empty input yields no entries",
			in:   "",
			want: nil,
		},
		{
			name: "staged modify",
			in:   "M  file.go\x00",
			want: []StatusEntry{{Code: "M ", Path: "file.go"}},
		},
		{
			name: "unstaged (working-tree-only) modify",
			in:   " M file.go\x00",
			want: []StatusEntry{{Code: " M", Path: "file.go"}},
		},
		{
			name: "untracked",
			in:   "?? new.txt\x00",
			want: []StatusEntry{{Code: "??", Path: "new.txt"}},
		},
		{
			name: "staged add",
			in:   "A  added.go\x00",
			want: []StatusEntry{{Code: "A ", Path: "added.go"}},
		},
		{
			name: "staged deletion",
			in:   "D  gone.go\x00",
			want: []StatusEntry{{Code: "D ", Path: "gone.go"}},
		},
		{
			name: "rename pairs the old path from the next token",
			in:   "R  new.go\x00old.go\x00",
			want: []StatusEntry{{Code: "R ", Path: "new.go", IsRename: true, OldPath: "old.go"}},
		},
		{
			name: "copy pairs the source from the next token",
			in:   "C  copy.go\x00orig.go\x00",
			want: []StatusEntry{{Code: "C ", Path: "copy.go", IsRename: true, OldPath: "orig.go"}},
		},
		{
			name: "rename with modification (RM) is still a rename",
			in:   "RM new.go\x00old.go\x00",
			want: []StatusEntry{{Code: "RM", Path: "new.go", IsRename: true, OldPath: "old.go"}},
		},
		{
			name: "malformed <4-char token is skipped",
			in:   "M\x00",
			want: nil,
		},
		{
			name: "truncated rename (no old-path token) leaves OldPath empty",
			in:   "R  new.go\x00",
			want: []StatusEntry{{Code: "R ", Path: "new.go", IsRename: true, OldPath: ""}},
		},
		{
			name: "path containing ' -> ' is not mangled (code[0] is truth-of-rename)",
			in:   "M  weird -> name.go\x00",
			want: []StatusEntry{{Code: "M ", Path: "weird -> name.go"}},
		},
		{
			name: "multiple mixed entries preserve order and rename pairing",
			in:   "M  a.go\x00?? b.txt\x00R  d.go\x00c.go\x00 M e.go\x00",
			want: []StatusEntry{
				{Code: "M ", Path: "a.go"},
				{Code: "??", Path: "b.txt"},
				{Code: "R ", Path: "d.go", IsRename: true, OldPath: "c.go"},
				{Code: " M", Path: "e.go"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseStatusZ(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("ParseStatusZ(%q) = %d entries, want %d\n got: %+v", tc.in, len(got), len(tc.want), got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("entry %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestComputeItemsPrefix covers the gate's toplevel-relative prefix derivation
// (I-1621): nested layout yields a slash-suffixed lowercased prefix, flat layout
// (items root == toplevel) yields "", and a non-repo root fails open (ok=false).
func TestComputeItemsPrefix(t *testing.T) {
	t.Run("nested layout yields slash-suffixed prefix", func(t *testing.T) {
		top := t.TempDir()
		initGitRepo(t, top)
		itemsRoot := filepath.Join(top, "agent-state")
		if err := os.MkdirAll(itemsRoot, 0755); err != nil {
			t.Fatal(err)
		}
		prefix, ok := ComputeItemsPrefix(itemsRoot)
		if !ok {
			t.Fatalf("ComputeItemsPrefix(nested) ok=false, want true")
		}
		if prefix != "agent-state/" {
			t.Errorf("prefix = %q, want %q", prefix, "agent-state/")
		}
	})

	t.Run("flat layout (root == toplevel) yields empty prefix", func(t *testing.T) {
		top := t.TempDir()
		initGitRepo(t, top)
		prefix, ok := ComputeItemsPrefix(top)
		if !ok {
			t.Fatalf("ComputeItemsPrefix(flat) ok=false, want true")
		}
		if prefix != "" {
			t.Errorf("prefix = %q, want \"\" (flat layout)", prefix)
		}
	})

	t.Run("non-repo root fails open", func(t *testing.T) {
		dir := t.TempDir() // not a git repo
		if _, ok := ComputeItemsPrefix(dir); ok {
			t.Errorf("ComputeItemsPrefix(non-repo) ok=true, want false (fail-open)")
		}
	})
}
