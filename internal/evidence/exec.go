package evidence

import (
	"os/exec"
)

// runAWSCLI executes an aws CLI command and returns stdout.
func runAWSCLI(args ...string) (string, error) {
	cmd := exec.Command("aws", args...)
	out, err := cmd.Output()
	return string(out), err
}
