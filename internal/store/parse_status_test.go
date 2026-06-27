package store

import (
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
