package main

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/unlicensedthought/diff-mutation/internal/gitx"
	"github.com/unlicensedthought/diff-mutation/internal/report"
)

//go:embed selftest
var fixture embed.FS

// materialize writes the embedded fixture into a fresh git repository.
func materialize(dir string) error {
	err := fs.WalkDir(fixture, "selftest", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, "selftest"), "/")
		if rel == "" {
			return nil
		}
		dst := filepath.Join(dir, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := fixture.ReadFile(p)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(p, ".sh") {
			mode = 0o755
		}
		return os.WriteFile(dst, b, mode)
	})
	if err != nil {
		return err
	}
	return commitAll(dir, "init", true)
}

func commitAll(dir, msg string, init bool) error {
	steps := [][]string{{"add", "-A"}, {"-c", "core.hooksPath=/dev/null", "commit", "-q", "--no-verify", "--allow-empty", "-m", msg}}
	if init {
		steps = append([][]string{{"init", "-q", "-b", "main"}}, steps...)
	}
	for _, a := range steps {
		cmd := exec.Command("git", append([]string{"-C", dir}, a...)...)
		cmd.Env = gitx.Env("GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %v: %s", a, err, out)
		}
	}
	return nil
}

// selfCase is one direction of the self-check.
type selfCase struct {
	name     string
	prepare  func(repo string) error
	args     []string
	env      []string
	wantRC   int
	check    func(r *report.Report) error
	wantGate int // -1 = skip the gate
}

const targetFns = "scripts/admit.sh:admit,label,first_ok"

func swapGutted(repo string) error {
	b, err := os.ReadFile(filepath.Join(repo, "tests", "admit.gutted.sh"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(repo, "tests", "admit.oracle.sh"), b, 0o755); err != nil {
		return err
	}
	return commitAll(repo, "gut the oracle", false)
}

// branchEdit changes one line inside admit() on a branch off main, so diff
// mode selects exactly that function.
func branchEdit(repo string) error {
	cmd := exec.Command("git", "-C", repo, "checkout", "-q", "-b", "change")
	cmd.Env = gitx.Env()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	p := filepath.Join(repo, "scripts", "admit.sh")
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	nb := strings.Replace(string(b), `local n="$1" limit="$2"`, `local n="$1" limit="$2"   # changed`, 1)
	if nb == string(b) {
		return fmt.Errorf("fixture edit anchor not found")
	}
	if err := os.WriteFile(p, []byte(nb), 0o755); err != nil {
		return err
	}
	return commitAll(repo, "edit admit", false)
}

func countIs(field string, pred func(report.Counts) bool) func(*report.Report) error {
	return func(r *report.Report) error {
		c := report.Tally(r.Mutants, nil)
		if !pred(c) {
			return fmt.Errorf("%s: recomputed counts %+v", field, c)
		}
		return nil
	}
}

func selfCases() []selfCase {
	return []selfCase{
		{
			name: "strong oracle kills every mutant (exit 0)", args: []string{"--target", targetFns}, wantRC: 0, wantGate: 0,
			check: countIs("killed>0 && survived==0", func(c report.Counts) bool { return c.Killed+c.Timeout > 0 && c.Survived == 0 && c.NoCoverage == 0 }),
		},
		{
			name: "gutted oracle leaves survivors (exit 1)", prepare: swapGutted, args: []string{"--target", targetFns}, wantRC: 1, wantGate: -1,
			check: countIs("survived>0", func(c report.Counts) bool { return c.Survived > 0 }),
		},
		{
			name: "missing oracle is COULD-NOT-RUN (exit 2)",
			prepare: func(repo string) error {
				if err := os.Remove(filepath.Join(repo, "tests", "admit.oracle.sh")); err != nil {
					return err
				}
				return commitAll(repo, "drop the oracle", false)
			},
			args: []string{"--target", targetFns}, wantRC: 2, wantGate: 2,
			check: func(r *report.Report) error {
				if len(r.CouldNotRun) == 0 || !strings.Contains(strings.Join(r.CouldNotRun, " "), "NO ORACLE") {
					return fmt.Errorf("couldNotRun %q lacks NO ORACLE", r.CouldNotRun)
				}
				return nil
			},
		},
		{
			name: "diff mode, strong oracle: gate passes", prepare: branchEdit, args: []string{"--base", "main"}, wantRC: 0, wantGate: 0,
			check: func(r *report.Report) error {
				for _, f := range r.Files {
					for _, fn := range f.Functions {
						if fn.Changed && fn.Name != "admit" {
							return fmt.Errorf("diff mode marked %s changed; only admit was edited", fn.Name)
						}
					}
				}
				return countIs("killed>0", func(c report.Counts) bool { return c.Killed > 0 })(r)
			},
		},
		{
			name: "diff mode, gutted oracle: changed function has zero killing tests, gate FAILS",
			prepare: func(repo string) error {
				if err := branchEdit(repo); err != nil {
					return err
				}
				return swapGutted(repo)
			},
			args: []string{"--base", "main"}, wantRC: 1, wantGate: 1,
			check: countIs("killed==0", func(c report.Counts) bool { return c.Killed+c.Timeout == 0 && c.Survived > 0 }),
		},
		{
			name: "oversubscribed host refuses to start (exit 2)", env: []string{"CHANGELING_LOAD1=100000"},
			args: []string{"--target", targetFns}, wantRC: 2, wantGate: 2,
			check: func(r *report.Report) error {
				if len(r.Mutants) != 0 || !strings.Contains(strings.Join(r.CouldNotRun, " "), "REFUSED TO START") {
					return fmt.Errorf("expected a refusal with no mutants, got %d mutants, %q", len(r.Mutants), r.CouldNotRun)
				}
				return nil
			},
		},
	}
}

// runSelftest executes every case; it returns the failures.
func runSelftest(logw func(string, ...any)) []string {
	var failures []string
	for _, sc := range selfCases() {
		repo, err := os.MkdirTemp("", "changeling-selftest-")
		if err != nil {
			return append(failures, err.Error())
		}
		func() {
			defer os.RemoveAll(repo)
			if err := materialize(repo); err != nil {
				failures = append(failures, sc.name+": "+err.Error())
				return
			}
			if sc.prepare != nil {
				if err := sc.prepare(repo); err != nil {
					failures = append(failures, sc.name+": prepare: "+err.Error())
					return
				}
			}
			// The fixture's oracle runs in milliseconds against a 20 s floor, so
			// host load cannot flip its verdicts; the load guard is exercised by
			// its own case, which overrides this.
			env := append([]string{"CHANGELING_LOAD1=0"}, sc.env...)
			for _, kv := range env {
				k, v, _ := strings.Cut(kv, "=")
				old, had := os.LookupEnv(k)
				os.Setenv(k, v)
				defer func() {
					if had {
						os.Setenv(k, old)
					} else {
						os.Unsetenv(k)
					}
				}()
			}
			out := filepath.Join(repo, ".changeling")
			start := time.Now().UTC()
			args := append([]string{"--repo", repo, "--config", "changeling.json", "--out", out, "--jobs", "2", "--mutant-timeout-min", "20s", "--work", filepath.Join(repo, ".work"), "--environment", "selftest"}, sc.args...)
			rc := cmdRun(args)
			if rc != sc.wantRC {
				failures = append(failures, fmt.Sprintf("%s: exit %d, want %d", sc.name, rc, sc.wantRC))
				return
			}
			r, err := report.Read(filepath.Join(out, "changeling-report.json"))
			if err != nil {
				failures = append(failures, sc.name+": "+err.Error())
				return
			}
			if report.ExitCode(r.DeriveVerdict()) != rc {
				failures = append(failures, fmt.Sprintf("%s: exit %d disagrees with the verdict recomputed from the JSON (%s)", sc.name, rc, r.DeriveVerdict()))
			}
			if err := sc.check(r); err != nil {
				failures = append(failures, sc.name+": "+err.Error())
				return
			}
			if sc.wantGate >= 0 {
				if g := report.Gate(r, start); g.Exit != sc.wantGate {
					failures = append(failures, fmt.Sprintf("%s: gate exit %d, want %d (%v)", sc.name, g.Exit, sc.wantGate, g.Reasons))
					return
				}
			}
			c := report.Tally(r.Mutants, nil)
			logw("selftest PASS  %s  [mutants=%d killed=%d timeout=%d survived=%d]", sc.name, c.Total, c.Killed, c.Timeout, c.Survived)
		}()
	}
	return failures
}

func cmdSelftest(args []string) int {
	fails := runSelftest(logf)
	for _, f := range fails {
		logf("selftest FAIL  %s", f)
	}
	if len(fails) > 0 {
		return 1
	}
	logf("selftest: all %d cases behave in both directions", len(selfCases()))
	return 0
}
