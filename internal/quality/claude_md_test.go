package quality

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

func hasFinding(findings []ClaudeMdFinding, pattern string) bool {
	for _, f := range findings {
		if f.Pattern == pattern {
			return true
		}
	}
	return false
}

func TestScanCLAUDEMd_OperatorQuote(t *testing.T) {
	p := writeFixture(t, "Some content.\nOperator 2026-05-20: stop doing that\nMore content.\n")
	findings := ScanCLAUDEMd(p, 150, 200)
	if !hasFinding(findings, "operator-quote") {
		t.Errorf("expected operator-quote finding, got: %+v", findings)
	}
}

func TestScanCLAUDEMd_LessonsLearned(t *testing.T) {
	p := writeFixture(t, "We got burned last quarter by a missing migration.\n")
	findings := ScanCLAUDEMd(p, 150, 200)
	if !hasFinding(findings, "lessons-learned") {
		t.Errorf("expected lessons-learned finding, got: %+v", findings)
	}
}

func TestScanCLAUDEMd_EmphaticQuote(t *testing.T) {
	p := writeFixture(t, "STOP FUCKING DOING the PR before the test\n")
	findings := ScanCLAUDEMd(p, 150, 200)
	if !hasFinding(findings, "emphatic-quote") {
		t.Errorf("expected emphatic-quote finding, got: %+v", findings)
	}
}

func TestScanCLAUDEMd_ItemNarrative(t *testing.T) {
	p := writeFixture(t, "I-715: this burned three deploys in a row\n")
	findings := ScanCLAUDEMd(p, 150, 200)
	if !hasFinding(findings, "item-narrative") {
		t.Errorf("expected item-narrative finding, got: %+v", findings)
	}
}

func TestScanCLAUDEMd_BlockQuoteRun(t *testing.T) {
	p := writeFixture(t, "Lead-in line\n> quote line 1\n> quote line 2\n> quote line 3\n> quote line 4\nTrailing line\n")
	findings := ScanCLAUDEMd(p, 150, 200)
	if !hasFinding(findings, "block-quote-run") {
		t.Errorf("expected block-quote-run finding, got: %+v", findings)
	}
}

func TestScanCLAUDEMd_BlockQuoteShort_NoFinding(t *testing.T) {
	// Two quoted lines is below threshold.
	p := writeFixture(t, "Lead-in\n> quote 1\n> quote 2\nTail\n")
	findings := ScanCLAUDEMd(p, 150, 200)
	if hasFinding(findings, "block-quote-run") {
		t.Errorf("two-line quote should not trigger block-quote-run, got: %+v", findings)
	}
}

func TestScanCLAUDEMd_SizeCap(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 250; i++ {
		b.WriteString("line\n")
	}
	p := writeFixture(t, b.String())
	findings := ScanCLAUDEMd(p, 150, 200)
	if !hasFinding(findings, "size-cap") {
		t.Errorf("expected size-cap finding for 250-line file with cap 200, got: %+v", findings)
	}
}

func TestScanCLAUDEMd_SizeWarn(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 175; i++ {
		b.WriteString("line\n")
	}
	p := writeFixture(t, b.String())
	findings := ScanCLAUDEMd(p, 150, 200)
	if !hasFinding(findings, "size-warn") {
		t.Errorf("expected size-warn finding for 175-line file with target 150, got: %+v", findings)
	}
	if hasFinding(findings, "size-cap") {
		t.Errorf("should not have size-cap finding under cap 200, got: %+v", findings)
	}
}

func TestScanCLAUDEMd_CleanFixture(t *testing.T) {
	// 100 lines of plain content matching no pattern.
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString("Plain content line.\n")
	}
	p := writeFixture(t, b.String())
	findings := ScanCLAUDEMd(p, 150, 200)
	if len(findings) != 0 {
		t.Errorf("clean fixture should yield zero findings, got: %+v", findings)
	}
}

func TestScanCLAUDEMd_MissingFile(t *testing.T) {
	findings := ScanCLAUDEMd("/nonexistent/path/CLAUDE.md", 150, 200)
	if findings != nil {
		t.Errorf("missing file should return nil, got: %+v", findings)
	}
}

func TestScanCLAUDEMd_EnvOverride(t *testing.T) {
	t.Setenv("CLAUDE_MD_AUDIT_CAP", "50")
	var b strings.Builder
	for i := 0; i < 60; i++ {
		b.WriteString("line\n")
	}
	p := writeFixture(t, b.String())
	findings := ScanCLAUDEMd(p, 30, 100) // explicit args ignored when env set
	if !hasFinding(findings, "size-cap") {
		t.Errorf("env override CLAUDE_MD_AUDIT_CAP=50 should trigger size-cap on 60-line file, got: %+v", findings)
	}
}

func TestScanCLAUDEMd_OperatorWithoutDate_NoFinding(t *testing.T) {
	p := writeFixture(t, "The operator confirmed this approach.\n")
	findings := ScanCLAUDEMd(p, 150, 200)
	if hasFinding(findings, "operator-quote") {
		t.Errorf("'operator' without YYYY-MM-DD timestamp should not trigger operator-quote, got: %+v", findings)
	}
}
