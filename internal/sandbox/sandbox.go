// Package sandbox builds the isolated tree every mutant runs in and runs one
// oracle test inside it under a process-group timeout.
//
// THE ORIGINAL FILE IS NEVER WRITTEN. Mutants are written only into a copy of
// the tree under the run's work directory. A changeling process killed at any
// instant (SIGKILL included) can therefore leave at worst a stale copy behind,
// never a mutated source file. Stale copies are reaped by the next run (their
// owner pid is recorded and checked).
package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/unlicensedthought/diff-mutation/internal/gitx"
)

// Box is one isolated copy of the repository.
type Box struct {
	Root string // the copied tree
	Tmp  string // TMPDIR handed to tests
}

const ownerFile = ".changeling-owner"

// Own stamps dir (a run directory) with this process as its owner.
func Own(dir string) error {
	return os.WriteFile(filepath.Join(dir, ownerFile), []byte(strconv.Itoa(os.Getpid())), 0o644)
}

// ReapStale removes run directories under work whose owner process is gone.
func ReapStale(work string) []string {
	var reaped []string
	ents, err := os.ReadDir(work)
	if err != nil {
		return nil
	}
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "run-") {
			continue
		}
		dir := filepath.Join(work, e.Name())
		b, err := os.ReadFile(filepath.Join(dir, ownerFile))
		pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
		if err == nil && perr == nil && pid > 0 && syscall.Kill(pid, 0) == nil {
			continue
		}
		if os.RemoveAll(dir) == nil {
			reaped = append(reaped, dir)
		}
	}
	return reaped
}

// New copies every tracked and untracked-not-ignored file of repo into a fresh
// directory under work and commits it as a throwaway git repository, so tests
// that call git against their own checkout keep working. node_modules, when
// present, is linked (read-only use) rather than copied.
func New(repo, work string, id int) (*Box, error) {
	dir, err := os.MkdirTemp(work, fmt.Sprintf("box-%d-", id))
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, ownerFile), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return nil, err
	}
	root := filepath.Join(dir, "tree")
	tmp := filepath.Join(dir, "tmp")
	for _, d := range []string{root, tmp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	list, err := gitx.Run(repo, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	for _, rel := range strings.Split(list, "\x00") {
		if rel == "" {
			continue
		}
		if err := copyEntry(filepath.Join(repo, rel), filepath.Join(root, rel)); err != nil {
			return nil, fmt.Errorf("copy %s: %w", rel, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(repo, "node_modules")); err == nil && fi.IsDir() {
		if err := os.Symlink(filepath.Join(repo, "node_modules"), filepath.Join(root, "node_modules")); err != nil {
			return nil, err
		}
	}
	env := []string{"GIT_AUTHOR_NAME=changeling", "GIT_AUTHOR_EMAIL=changeling@localhost", "GIT_COMMITTER_NAME=changeling", "GIT_COMMITTER_EMAIL=changeling@localhost"}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "-A"},
		{"-c", "core.hooksPath=/dev/null", "commit", "-q", "--no-verify", "-m", "changeling sandbox"},
	} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = gitx.Env(env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("sandbox git %v: %w: %s", args, err, out)
		}
	}
	return &Box{Root: root, Tmp: tmp}, nil
}

func copyEntry(src, dst string) error {
	fi, err := os.Lstat(src)
	if errors.Is(err, os.ErrNotExist) {
		return nil // deleted in the working tree but still in the index
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(t, dst)
	}
	if !fi.Mode().IsRegular() {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Clean returns the box tree to its committed state when a test wrote into
// it, and reports what it had to undo (empty = nothing).
func (b *Box) Clean() (string, error) {
	st, err := gitx.Run(b.Root, "status", "--porcelain", "--untracked-files=all", "--ignored=no")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(st) == "" {
		return "", nil
	}
	if _, err := gitx.Run(b.Root, "checkout", "-q", "--", "."); err != nil {
		return st, err
	}
	if _, err := gitx.Run(b.Root, "clean", "-fdq", "-e", "node_modules"); err != nil {
		return st, err
	}
	return st, nil
}

// Hash is the sha256 of b, hex.
func Hash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Write replaces rel inside the box with content, keeping its mode.
func (b *Box) Write(rel string, content []byte) error {
	p := filepath.Join(b.Root, rel)
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	tmp := p + ".changeling-tmp"
	if err := os.WriteFile(tmp, content, fi.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Chmod(tmp, fi.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Verify checks rel inside the box hashes to want.
func (b *Box) Verify(rel, want string) error {
	got, err := os.ReadFile(filepath.Join(b.Root, rel))
	if err != nil {
		return err
	}
	if Hash(got) != want {
		return fmt.Errorf("sandbox copy of %s is not pristine (sha256 %s, want %s)", rel, Hash(got)[:12], want[:12])
	}
	return nil
}

// Result of one test execution.
type Result struct {
	Exit     int           `json:"exit"`
	TimedOut bool          `json:"timedOut"`
	Duration time.Duration `json:"durationNs"`
	Tail     string        `json:"tail,omitempty"`
	StartErr string        `json:"startErr,omitempty"`
}

// Passed reports a clean exit.
func (r Result) Passed() bool { return r.StartErr == "" && !r.TimedOut && r.Exit == 0 }

// Run executes runner... test inside the box with cwd = box root. The child
// gets its own process group; on timeout the whole group is TERMed, then
// KILLed after a grace period. Variables ending in _UNDER_TEST are removed:
// the harness tests honour them to point at an alternate script, and an
// inherited one would silently test something other than the mutant.
func (b *Box) Run(ctx context.Context, runner []string, test string, timeout time.Duration, extraEnv ...string) Result {
	args := append(append([]string{}, runner[1:]...), test)
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.Command(runner[0], args...)
	cmd.Dir = b.Root
	var env []string
	for _, kv := range gitx.Env() {
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		if strings.HasSuffix(k, "_UNDER_TEST") || k == "TMPDIR" {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(append(env, "TMPDIR="+b.Tmp, "CHANGELING_SANDBOX=1"), extraEnv...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{Exit: -1, StartErr: err.Error()}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var werr error
	timedOut := false
	select {
	case werr = <-done:
	case <-cctx.Done():
		timedOut = ctx.Err() == nil // parent cancel is not a test timeout
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case werr = <-done:
		case <-time.After(5 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			werr = <-done
		}
	}
	// Reap anything the test leaked into its group.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	r := Result{Duration: time.Since(start), TimedOut: timedOut}
	if werr != nil {
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			r.Exit = ee.ExitCode()
		} else {
			r.Exit = -1
		}
	}
	if ctx.Err() != nil {
		r.StartErr = "cancelled"
	}
	out := buf.Bytes()
	if len(out) > 2000 {
		out = out[len(out)-2000:]
	}
	r.Tail = string(out)
	// Clear the test's scratch so one mutant's leftovers cannot feed the next.
	if ents, err := os.ReadDir(b.Tmp); err == nil {
		for _, e := range ents {
			_ = os.RemoveAll(filepath.Join(b.Tmp, e.Name()))
		}
	}
	return r
}
