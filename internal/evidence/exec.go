package evidence

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// runAWSCLI executes an aws CLI command and returns stdout.
//
// I-507: when the agent has its own per-session AWS credentials
// (AWS_ACCESS_KEY_ID set in the environment, typically by
// agent-aws-auth.sh's assume-role minting), force AWS_PROFILE="" on
// the child process. The AWS SDK's credential-resolution rules
// otherwise let a stale operator profile shadow valid env-var
// creds, producing the expired-session failure that motivated this
// fix. When AWS_ACCESS_KEY_ID is NOT set, the child inherits the
// operator's environment unchanged so a developer running
// `st test --run` from their own shell keeps getting profile-based
// auth.
func runAWSCLI(args ...string) (string, error) {
	cmd := exec.Command("aws", args...)
	cmd.Env = awsCommandEnv(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		// Surface the aws CLI's stderr so callers (e.g. the
		// preflight) can show a useful diagnostic instead of just
		// "exit status 1". *exec.ExitError carries the captured
		// stderr in .Stderr when Output() was called (the docs are
		// explicit about this).
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			if stderr != "" {
				return string(out), fmt.Errorf("%w: %s", err, stderr)
			}
		}
	}
	return string(out), err
}

// awsCommandEnv returns the env slice the spawned `aws` command
// should run under. Exposed (lowercase, package-internal) for
// testing.
//
// Both AWS_PROFILE and AWS_DEFAULT_PROFILE are stripped — the AWS
// SDK reads AWS_PROFILE first and falls back to AWS_DEFAULT_PROFILE,
// so leaving the latter intact would still let a stale profile
// shadow the env-var creds.
func awsCommandEnv(parent []string) []string {
	hasAccessKey := false
	for _, kv := range parent {
		if strings.HasPrefix(kv, "AWS_ACCESS_KEY_ID=") {
			// Reject empty value — agent-aws-auth.sh sets a real
			// key, but a defensive check keeps an
			// `AWS_ACCESS_KEY_ID=` holdover (manual unset that
			// left the var defined to empty) from forcing the
			// override and breaking the developer flow.
			if v := strings.TrimPrefix(kv, "AWS_ACCESS_KEY_ID="); v != "" {
				hasAccessKey = true
			}
			break
		}
	}
	if !hasAccessKey {
		return parent
	}
	// I-586: drop AWS_PROFILE / AWS_DEFAULT_PROFILE entirely rather
	// than re-adding them with an empty value. The aws CLI treats
	// `AWS_PROFILE=` (empty string) as "look up the profile named ''
	// in ~/.aws/config" and errors with "The config profile () could
	// not be found". Setting them to empty was the original I-507
	// approach but it surfaces this CLI behavior. Plain absence
	// makes the env-var creds win as intended.
	out := make([]string, 0, len(parent))
	for _, kv := range parent {
		if strings.HasPrefix(kv, "AWS_PROFILE=") || strings.HasPrefix(kv, "AWS_DEFAULT_PROFILE=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// HasAgentCredentials reports whether the parent environment carries
// agent-minted credentials (a non-empty AWS_ACCESS_KEY_ID). Callers
// outside this package use it to decide whether to suppress the
// `--profile` CLI flag — when env-var creds are present, `--profile
// name` would silently win over them in the AWS CLI's resolution
// order, defeating the env override that runAWSCLI just installed.
func HasAgentCredentials() bool {
	v := os.Getenv("AWS_ACCESS_KEY_ID")
	return v != ""
}
