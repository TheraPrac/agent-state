package main

import (
	"errors"
	"strings"
	"testing"
)

// I-1477(a): flagErrorWithHint appends a did-you-mean hint for known-wrong
// flags and passes unrelated errors through unchanged.
func TestFlagErrorWithHint(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantSub string // substring expected in the result ("" = unchanged)
	}{
		{"description", errors.New("unknown flag: --description"), "--sbar-situation"},
		{"bare-situation", errors.New("unknown flag: --situation"), "sbar-"},
		{"review-skip", errors.New("unknown flag: --review-skip"), "review_skips"},
		{"unrelated", errors.New("unknown flag: --frobnicate"), ""},
		{"nil", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := flagErrorWithHint(nil, tc.err)
			if tc.err == nil {
				if got != nil {
					t.Fatalf("expected nil, got %v", got)
				}
				return
			}
			if tc.wantSub == "" {
				if got.Error() != tc.err.Error() {
					t.Errorf("expected passthrough %q, got %q", tc.err.Error(), got.Error())
				}
				return
			}
			if !strings.Contains(got.Error(), tc.wantSub) {
				t.Errorf("expected hint containing %q, got %q", tc.wantSub, got.Error())
			}
			// The original error must still be wrapped/visible.
			if !strings.Contains(got.Error(), tc.err.Error()) {
				t.Errorf("expected original error preserved, got %q", got.Error())
			}
		})
	}
}
