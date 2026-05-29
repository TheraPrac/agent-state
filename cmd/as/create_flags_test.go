package main

import (
	"strings"
	"testing"
)

// TestCreateHelpShowsSBARAndNoValidateFlags verifies that the --sbar-* flags
// and --no-validate are registered on the create command.
//
// I-908: these flag names are used by security-scan-on-push.sh (T-433).
// If they drift, the hook's `st create` calls fail silently with
// "unknown flag" errors — this test keeps the contract explicit.
func TestCreateHelpShowsSBARAndNoValidateFlags(t *testing.T) {
	app := newApp("")
	createCmd := findCommand(app, "create")
	if createCmd == nil {
		t.Fatal("could not locate `st create` command in the cobra tree")
	}

	requiredFlags := []string{
		"sbar-situation",
		"sbar-background",
		"sbar-assessment",
		"sbar-recommendation",
		"no-validate",
	}

	for _, flagName := range requiredFlags {
		f := createCmd.Flags().Lookup(flagName)
		if f == nil {
			t.Errorf("flag --%s not registered on `st create` (required by I-908 / T-433)", flagName)
			continue
		}
		// Verify the usage string is not empty.
		if strings.TrimSpace(f.Usage) == "" {
			t.Errorf("flag --%s has an empty usage string", flagName)
		}
	}
}
