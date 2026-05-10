package evidence

import (
	"sort"
	"strings"
	"testing"
)

// I-507: with agent env-var creds present, the spawned aws command
// must receive AWS_PROFILE="" (empty string) so the AWS SDK
// resolves credentials from env vars instead of the operator's
// possibly-expired profile.
func TestAWSCommandEnv_ClearsProfileWhenAccessKeyPresent(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"HOME=/tmp",
		"AWS_PROFILE=jfinlinson_admin",
		"AWS_REGION=us-east-1",
		"AWS_ACCESS_KEY_ID=AKIA...",
		"AWS_SECRET_ACCESS_KEY=secret",
		"AWS_SESSION_TOKEN=token",
	}
	got := awsCommandEnv(parent)

	regionPresent, secretPresent, tokenPresent := false, false, false
	for _, kv := range got {
		switch {
		case strings.HasPrefix(kv, "AWS_PROFILE="):
			t.Errorf("AWS_PROFILE should be ABSENT from child env (I-586), got %q — empty makes the aws CLI error \"config profile () could not be found\"", kv)
		case kv == "AWS_REGION=us-east-1":
			regionPresent = true
		case kv == "AWS_SECRET_ACCESS_KEY=secret":
			secretPresent = true
		case kv == "AWS_SESSION_TOKEN=token":
			tokenPresent = true
		}
	}
	if !regionPresent || !secretPresent || !tokenPresent {
		t.Errorf("other AWS env vars should pass through unchanged: region=%v secret=%v token=%v",
			regionPresent, secretPresent, tokenPresent)
	}
}

// I-507: without AWS_ACCESS_KEY_ID in env, the child inherits the
// parent unchanged so a developer running st test --run from their
// own shell keeps getting profile-based auth.
func TestAWSCommandEnv_NoOverrideWhenNoAccessKey(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"AWS_PROFILE=jfinlinson_admin",
	}
	got := awsCommandEnv(parent)

	gotSorted := append([]string{}, got...)
	parentSorted := append([]string{}, parent...)
	sort.Strings(gotSorted)
	sort.Strings(parentSorted)
	if len(gotSorted) != len(parentSorted) {
		t.Fatalf("env len changed: got %d, want %d (got=%v)", len(gotSorted), len(parentSorted), got)
	}
	for i := range gotSorted {
		if gotSorted[i] != parentSorted[i] {
			t.Errorf("env entry %d differs: got %q, want %q", i, gotSorted[i], parentSorted[i])
		}
	}
}

// I-507: an empty `AWS_ACCESS_KEY_ID=` (a manual unset that left
// the var defined to empty rather than removed) should NOT trigger
// the override. Otherwise developers who type `unset
// AWS_ACCESS_KEY_ID` get a confusing override they did not ask
// for.
func TestAWSCommandEnv_EmptyAccessKeyIsNoOverride(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"AWS_PROFILE=jfinlinson_admin",
		"AWS_ACCESS_KEY_ID=",
	}
	got := awsCommandEnv(parent)
	for _, kv := range got {
		if kv == "AWS_PROFILE=" {
			t.Error("empty AWS_ACCESS_KEY_ID should not have triggered the AWS_PROFILE override")
		}
	}
}

// I-507 (review fix): AWS_DEFAULT_PROFILE is the SDK fallback when
// AWS_PROFILE is empty/absent. Stripping only AWS_PROFILE would
// leave the operator's stale AWS_DEFAULT_PROFILE intact, defeating
// the override. Verify both are cleared.
func TestAWSCommandEnv_ClearsBothProfileVars(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"AWS_PROFILE=jfinlinson_admin",
		"AWS_DEFAULT_PROFILE=fallback",
		"AWS_ACCESS_KEY_ID=AKIA...",
	}
	got := awsCommandEnv(parent)
	// I-586 update: AWS_PROFILE / AWS_DEFAULT_PROFILE must be ABSENT
	// from the child env (not set to empty string). The aws CLI errors
	// on AWS_PROFILE="" with "config profile () could not be found",
	// which is exactly the symptom that drove the I-586 fix.
	for _, kv := range got {
		if strings.HasPrefix(kv, "AWS_PROFILE=") {
			t.Errorf("AWS_PROFILE should be absent from child env, got %q (empty or otherwise)", kv)
		}
		if strings.HasPrefix(kv, "AWS_DEFAULT_PROFILE=") {
			t.Errorf("AWS_DEFAULT_PROFILE should be absent from child env, got %q", kv)
		}
	}
}

// I-507 (review fix): HasAgentCredentials reports whether the
// process env carries an agent-minted access key. Used by callers
// outside this package (S3Backend.appendCommonFlags) to skip the
// --profile CLI flag — that flag wins over env-var creds in the
// AWS CLI resolution order and would defeat the env override.
func TestHasAgentCredentials(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	if HasAgentCredentials() {
		t.Error("empty AWS_ACCESS_KEY_ID should report false")
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIA...")
	if !HasAgentCredentials() {
		t.Error("populated AWS_ACCESS_KEY_ID should report true")
	}
}

// I-586: AWS_PROFILE / AWS_DEFAULT_PROFILE must be entirely ABSENT
// (not set to ""). Setting them to empty was the original I-507
// approach but the aws CLI errors with "config profile () could not
// be found" when it sees AWS_PROFILE="". Absence is what makes the
// env-var creds win.
func TestAWSCommandEnv_OmitsProfileWhenAbsent(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"AWS_ACCESS_KEY_ID=AKIA...",
	}
	got := awsCommandEnv(parent)
	for _, kv := range got {
		if strings.HasPrefix(kv, "AWS_PROFILE=") || strings.HasPrefix(kv, "AWS_DEFAULT_PROFILE=") {
			t.Errorf("AWS_PROFILE/AWS_DEFAULT_PROFILE should be absent (got %q) — empty string makes the aws CLI error", kv)
		}
	}
}
