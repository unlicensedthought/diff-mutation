// Package gitx runs git with the hook-exported GIT_* variables removed.
//
// git exports GIT_DIR / GIT_WORK_TREE / GIT_INDEX_FILE to hook subprocesses.
// A changeling run launched from a hook would otherwise operate on the hook's
// repository instead of the one named by -C, so every call scrubs them.
package gitx

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// scrubbed are the variables git leaks into hook children.
var scrubbed = []string{"GIT_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE=", "GIT_PREFIX=", "GIT_COMMON_DIR="}

// Env returns os.Environ() without the git hook variables, plus extra.
func Env(extra ...string) []string {
	var out []string
	for _, kv := range os.Environ() {
		drop := false
		for _, p := range scrubbed {
			if strings.HasPrefix(kv, p) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return append(out, extra...)
}

// Run executes `git -C dir args...` and returns stdout. A non-zero exit is an
// error carrying git's stderr, never an empty result.
func Run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = Env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
