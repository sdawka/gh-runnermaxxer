package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// candidateNames are the executable names Locate looks for on PATH and next
// to this binary.
var candidateNames = []string{"runnermaxxer.sh", "runnermaxxer"}

// executablePath is a seam over os.Executable for tests.
var executablePath = os.Executable

// Locate finds the runnermaxxer.sh script, in order: the --script flag, the
// RUNNERMAXXER_SCRIPT environment variable, a sibling of the running
// binary, then PATH.
func Locate(flagValue string) (string, error) {
	tried := make([]string, 0, 8)

	if flagValue != "" {
		if fileExists(flagValue) {
			return flagValue, nil
		}
		tried = append(tried, flagValue)
	}

	if env := os.Getenv("RUNNERMAXXER_SCRIPT"); env != "" {
		if fileExists(env) {
			return env, nil
		}
		tried = append(tried, env)
	}

	if exe, err := executablePath(); err == nil {
		dir := filepath.Dir(exe)
		for _, name := range candidateNames {
			candidate := filepath.Join(dir, name)
			if fileExists(candidate) {
				return candidate, nil
			}
			tried = append(tried, candidate)
		}
	}

	for _, name := range candidateNames {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
		tried = append(tried, name+" (PATH)")
	}

	return "", fmt.Errorf("runnermaxxer.sh not found (tried: %s); pass --script or set RUNNERMAXXER_SCRIPT", joinTried(tried))
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func joinTried(tried []string) string {
	out := ""
	for i, t := range tried {
		if i > 0 {
			out += ", "
		}
		out += t
	}
	return out
}
