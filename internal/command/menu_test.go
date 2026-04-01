package command

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// TestSelectMenuArrowKeys verifies arrow-key navigation + Enter confirms.
func TestSelectMenuArrowKeys(t *testing.T) {
	result := runMenuBinary(t, "select", []byte{
		0x1b, '[', 'B', // down arrow (move to option 2)
		0x1b, '[', 'B', // down arrow (move to option 3)
		'\r',           // Enter confirms option 3
	})
	if result != "a" {
		t.Errorf("arrow down×2 + Enter: got %q, want %q", result, "a")
	}
}

// TestSelectMenuHotkeyImmediate verifies single keypress selects immediately.
func TestSelectMenuHotkeyImmediate(t *testing.T) {
	result := runMenuBinary(t, "select", []byte{'s'})
	if result != "s" {
		t.Errorf("hotkey 's': got %q, want %q", result, "s")
	}
}

// TestSelectMenuHotkeyFirst verifies first option hotkey works.
func TestSelectMenuHotkeyFirst(t *testing.T) {
	result := runMenuBinary(t, "select", []byte{'c'})
	if result != "c" {
		t.Errorf("hotkey 'c': got %q, want %q", result, "c")
	}
}

// TestSelectMenuEnterDefault verifies Enter selects the default (first) option.
func TestSelectMenuEnterDefault(t *testing.T) {
	result := runMenuBinary(t, "select", []byte{'\r'})
	if result != "c" {
		t.Errorf("Enter default: got %q, want %q", result, "c")
	}
}

// TestSelectMenuArrowUpAtTop verifies up arrow at top stays at top.
func TestSelectMenuArrowUpAtTop(t *testing.T) {
	result := runMenuBinary(t, "select", []byte{
		0x1b, '[', 'A', // up arrow (already at top, stays)
		'\r',           // Enter confirms option 1
	})
	if result != "c" {
		t.Errorf("up at top + Enter: got %q, want %q", result, "c")
	}
}

// TestSelectMenuArrowDownThenUp verifies bidirectional navigation.
func TestSelectMenuArrowDownThenUp(t *testing.T) {
	result := runMenuBinary(t, "select", []byte{
		0x1b, '[', 'B', // down (option 2)
		0x1b, '[', 'B', // down (option 3)
		0x1b, '[', 'A', // up (back to option 2)
		'\r',           // Enter confirms option 2
	})
	if result != "s" {
		t.Errorf("down×2 up×1 + Enter: got %q, want %q", result, "s")
	}
}

// TestSelectMenuNumericKeys verifies numeric hotkeys for UAT menu.
func TestSelectMenuNumericKeys(t *testing.T) {
	result := runMenuBinary(t, "numeric", []byte{'2'})
	if result != "2" {
		t.Errorf("hotkey '2': got %q, want %q", result, "2")
	}
}

// TestConfirmPromptY verifies 'y' returns true.
func TestConfirmPromptY(t *testing.T) {
	result := runMenuBinary(t, "confirm", []byte{'y'})
	if result != "true" {
		t.Errorf("'y' confirm: got %q, want %q", result, "true")
	}
}

// TestConfirmPromptN verifies 'n' returns false.
func TestConfirmPromptN(t *testing.T) {
	result := runMenuBinary(t, "confirm", []byte{'n'})
	if result != "false" {
		t.Errorf("'n' confirm: got %q, want %q", result, "false")
	}
}

// TestConfirmPromptEnter verifies Enter defaults to false (N).
func TestConfirmPromptEnter(t *testing.T) {
	result := runMenuBinary(t, "confirm", []byte{'\r'})
	if result != "false" {
		t.Errorf("Enter confirm: got %q, want %q", result, "false")
	}
}

// runMenuBinary builds and runs a helper binary in a pty, sends keystrokes,
// and returns the JSON result printed to stdout.
func runMenuBinary(t *testing.T, mode string, keystrokes []byte) string {
	t.Helper()

	// Build the helper binary
	binPath := t.TempDir() + "/menu_helper"
	cmd := exec.Command("go", "build", "-o", binPath, "./internal/command/testdata/menu_helper.go")
	cmd.Dir = findModuleRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build helper: %v\n%s", err, out)
	}

	// Run in a pty
	helperCmd := exec.Command(binPath, mode)
	ptmx, err := pty.Start(helperCmd)
	if err != nil {
		t.Fatalf("pty.Start: %v", err)
	}
	defer ptmx.Close()

	// Give it a moment to render
	time.Sleep(100 * time.Millisecond)

	// Send keystrokes
	_, err = ptmx.Write(keystrokes)
	if err != nil {
		t.Fatalf("write keystrokes: %v", err)
	}

	// Read output (contains both terminal rendering and our JSON result)
	done := make(chan []byte, 1)
	go func() {
		var buf bytes.Buffer
		tmp := make([]byte, 4096)
		for {
			n, err := ptmx.Read(tmp)
			if n > 0 {
				buf.Write(tmp[:n])
			}
			if err != nil {
				break
			}
		}
		done <- buf.Bytes()
	}()

	// Wait for process to exit
	helperCmd.Wait()
	ptmx.Close() // force the reader goroutine to finish

	var output []byte
	select {
	case output = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for helper output")
	}

	// Parse the JSON result from the output
	// The helper prints {"result":"..."} on stdout (which goes through the pty)
	outputStr := string(output)
	idx := strings.LastIndex(outputStr, `{"result":`)
	if idx < 0 {
		t.Fatalf("no JSON result in output: %q", outputStr)
	}
	jsonStr := outputStr[idx:]
	// Trim anything after the closing brace
	if end := strings.Index(jsonStr, "}"); end >= 0 {
		jsonStr = jsonStr[:end+1]
	}

	var result struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		t.Fatalf("parse result JSON %q: %v", jsonStr, err)
	}
	return result.Result
}

func findModuleRoot(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(dir + "/go.mod"); err == nil {
			return dir
		}
		parent := dir[:strings.LastIndex(dir, "/")]
		if parent == dir {
			t.Fatal("could not find module root")
		}
		dir = parent
	}
}
