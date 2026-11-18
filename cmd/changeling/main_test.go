package main

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/unlicensedthought/diff-mutation/internal/gitx"
	"github.com/unlicensedthought/diff-mutation/internal/report"
	"github.com/unlicensedthought/diff-mutation/internal/sandbox"
)

// The two-way self-check: strong oracle -> killed (0), gutted -> survivors
// (1), no oracle -> 2, the diff-mode gate both ways, the load refusal.
// Bug named: an instrument that reports the same reading for a good and a
// gutted suite (it would pass every PR and prove nothing).
func TestSelftest(t *testing.T) {
	if fails := runSelftest(t.Logf); len(fails) > 0 {
		t.Fatalf("selftest failures:\n%s", strings.Join(fails, "\n"))
	}
}

// waitMutantLive polls root for a file ending in rel whose sha256 differs from
// origHash, i.e. a mutant written into a sandbox copy. It returns false on
// timeout.
func waitMutantLive(root, rel, origHash string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		live := false
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || live || d.IsDir() || !strings.HasSuffix(p, "/"+rel) {
				return nil
			}
			if got, rerr := os.ReadFile(p); rerr == nil && sandbox.Hash(got) != origHash {
				live = true
			}
			return nil
		})
		if live {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// The original file is never left mutated, even when changeling is SIGKILLed
// mid-mutant (no handler runs), and the stale sandbox is reaped next run.
// Bug named: an engine that mutates the file in place and restores it in a
// defer; a SIGKILL between write and restore ships the mutant.
func TestSIGKILLLeavesOriginalPristine(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "changeling")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	repo := t.TempDir()
	if err := materialize(repo); err != nil {
		t.Fatal(err)
	}
	// A slow oracle so the kill lands while a mutant is live in a sandbox.
	oracle := filepath.Join(repo, "tests", "admit.oracle.sh")
	b, _ := os.ReadFile(oracle)
	slow := strings.Replace(string(b), "fails=0\n", "fails=0\nsleep 3\n", 1)
	if err := os.WriteFile(oracle, []byte(slow), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := commitAll(repo, "slow oracle", false); err != nil {
		t.Fatal(err)
	}
	subject := filepath.Join(repo, "scripts", "admit.sh")
	before, _ := os.ReadFile(subject)
	work := filepath.Join(t.TempDir(), "work")
	cmd := exec.Command(bin, "run", "--repo", repo, "--config", "changeling.json", "--out", filepath.Join(repo, ".changeling"), "--work", work, "--jobs", "2", "--target", targetFns)
	cmd.Env = append(os.Environ(), "CHANGELING_LOAD1=0")
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sawMutants := make(chan bool, 1)
	covered := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		sent, covSent := false, false
		for sc.Scan() {
			// The coverage pass inserts probes into the sandbox copy; only
			// after it logs "coverage N/N" is a differing copy a real mutant.
			if !covSent && strings.Contains(sc.Text(), ": coverage ") {
				covered <- true
				covSent = true
			}
			if !sent && strings.Contains(sc.Text(), "mutants") {
				sawMutants <- true
				sent = true
			}
		}
	}()
	select {
	case <-sawMutants:
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("never reached the mutant phase")
	}
	select {
	case <-covered:
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("coverage pass never finished")
	}
	// Land the kill the instant a mutant is live in a sandbox copy: poll the
	// work tree for a copy of the subject whose bytes differ from the original.
	// Event-driven, no fixed sleep; the slow oracle keeps the mutant live for
	// seconds after it appears, so the kill cannot miss the window.
	if !waitMutantLive(work, "scripts/admit.sh", sandbox.Hash(before), 60*time.Second) {
		_ = cmd.Process.Kill()
		t.Fatal("no mutant ever went live in a sandbox")
	}
	_ = syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Wait()
	after, _ := os.ReadFile(subject)
	if sandbox.Hash(before) != sandbox.Hash(after) {
		t.Fatal("ORIGINAL MUTATED after SIGKILL")
	}
	st, err := gitx.Run(repo, "status", "--porcelain", "--untracked-files=no")
	if err != nil || strings.TrimSpace(st) != "" {
		t.Fatalf("fixture repo dirty after SIGKILL: %q %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".changeling", "changeling-report.json")); err == nil {
		t.Fatal("a killed run left a report that a gate would read as its result")
	}
	// The slow oracle only existed to hold a mutant live for the kill; the reap
	// check below needs the stale sandbox, not a slow oracle, so restore it.
	if err := os.WriteFile(oracle, b, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := commitAll(repo, "fast oracle", false); err != nil {
		t.Fatal(err)
	}
	// The killed run's sandbox is still there; the next run must reap it.
	again := exec.Command(bin, "run", "--repo", repo, "--config", "changeling.json", "--out", filepath.Join(repo, ".changeling"), "--work", work, "--jobs", "1", "--target", "scripts/admit.sh:label")
	again.Env = append(os.Environ(), "CHANGELING_LOAD1=0")
	out, _ := again.CombinedOutput()
	if !strings.Contains(string(out), "reaped stale sandbox") {
		t.Fatalf("stale sandbox of the killed run was not reaped:\n%s", out)
	}
}

// The budget is a WALL bound. A deadline that passes during the coverage pass
// cuts the run there (COULD-NOT-RUN "budget cut ... during the coverage
// pass"), it does not let every slow oracle test finish first and then mark
// the mutants not-run. Bug named: a budget that only the between-mutants check
// reads, so a slow suite overruns it by the length of the coverage pass.
func TestBudgetIsAWallBoundOverTheCoveragePass(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "changeling")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	repo := t.TempDir()
	if err := materialize(repo); err != nil {
		t.Fatal(err)
	}
	// A slow oracle: the coverage pass alone outlives a 1s budget many times over.
	oracle := filepath.Join(repo, "tests", "admit.oracle.sh")
	b, _ := os.ReadFile(oracle)
	slow := strings.Replace(string(b), "fails=0\n", "fails=0\nsleep 4\n", 1)
	if err := os.WriteFile(oracle, []byte(slow), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := commitAll(repo, "slow oracle", false); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "work")
	cmd := exec.Command(bin, "run", "--repo", repo, "--config", "changeling.json", "--out", filepath.Join(repo, ".changeling"), "--work", work, "--jobs", "2", "--budget", "1s", "--target", targetFns)
	cmd.Env = append(os.Environ(), "CHANGELING_LOAD1=0")
	t0 := time.Now()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(t0)
	rc := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		rc = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if rc != 2 {
		t.Fatalf("exit %d, want 2 (COULD-NOT-RUN):\n%s", rc, out)
	}
	rep, err := report.Read(filepath.Join(repo, ".changeling", "changeling-report.json"))
	if err != nil {
		t.Fatalf("report: %v\n%s", err, out)
	}
	reasons := strings.Join(rep.CouldNotRun, "\n")
	if !strings.Contains(reasons, "budget cut") || !strings.Contains(reasons, "during the coverage pass") {
		t.Fatalf("the budget was not reported as cut during the coverage pass; couldNotRun=%q\n%s", rep.CouldNotRun, out)
	}
	for _, m := range rep.Mutants {
		switch m.Status {
		case report.Killed, report.Survived, report.Timeout:
			t.Fatalf("mutant %s was evaluated (%s) after a 1s budget expired", m.ID, m.Status)
		}
	}
	// 1s budget + the TERM grace on the in-flight tests. Far under the ~8s+
	// a full coverage pass of two 4s oracles takes, with headroom for a loaded host.
	if elapsed > 7*time.Second {
		t.Fatalf("the run outlived its 1s budget by %s: the deadline is not a wall bound", elapsed-time.Second)
	}
}
