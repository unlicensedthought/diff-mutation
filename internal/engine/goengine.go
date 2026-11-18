package engine

import (
	"context"
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/unlicensedthought/diff-mutation/internal/config"
	"github.com/unlicensedthought/diff-mutation/internal/diffscope"
	"github.com/unlicensedthought/diff-mutation/internal/gitx"
	"github.com/unlicensedthought/diff-mutation/internal/report"
)

// GoTarget is one changed Go file.
type GoTarget struct {
	Path    string
	Changes []diffscope.Range
	Whole   bool
}

// GoRun configures the Gremlins shell-out.
type GoRun struct {
	Repo        string
	Base        string
	Cfg         *config.Config
	Log         func(string, ...any)
	Equivalents map[string]config.Equivalent
	Deadline    time.Time
}

// GremlinsPath resolves the pinned gremlins binary: $CHANGELING_GREMLINS, else
// ~/.cache/changeling/bin/gremlins. Its identity is read from the binary's own
// build info (module path + version), never from a --version string, which
// `go install` builds report as "dev".
func GremlinsPath(cfg *config.Config) (string, error) {
	p := os.Getenv("CHANGELING_GREMLINS")
	if p == "" {
		home, _ := os.UserHomeDir()
		p = filepath.Join(home, ".cache", "changeling", "bin", "gremlins")
	}
	bi, err := buildinfo.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("gremlins not at %s (%v); install the pin: GOBIN=$(dirname %s) go install %s/cmd/gremlins@%s", p, err, p, cfg.Gremlins.Module, cfg.Gremlins.Version)
	}
	if bi.Main.Path != cfg.Gremlins.Module || bi.Main.Version != cfg.Gremlins.Version {
		return "", fmt.Errorf("%s is %s@%s, the pin is %s@%s; refusing a namesake or a drifted version", p, bi.Main.Path, bi.Main.Version, cfg.Gremlins.Module, cfg.Gremlins.Version)
	}
	return p, nil
}

type gremlinsOut struct {
	GoModule string `json:"go_module"`
	Files    []struct {
		FileName  string `json:"file_name"`
		Mutations []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
			Line   int    `json:"line"`
			Column int    `json:"column"`
		} `json:"mutations"`
	} `json:"files"`
}

var gremlinsStatus = map[string]string{
	"KILLED": report.Killed, "LIVED": report.Survived, "NOT COVERED": report.NoCoverage,
	"TIMED OUT": report.Timeout, "NOT VIABLE": report.Invalid, "SKIPPED": report.Ignored, "RUNNABLE": report.NotRun,
}

func moduleRoot(repo, rel string) string {
	for d := filepath.Dir(filepath.Join(repo, rel)); strings.HasPrefix(d, repo); d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		if d == repo {
			break
		}
	}
	return ""
}

// RunGo runs gremlins once per changed package, scoped by --diff in diff mode.
func RunGo(ctx context.Context, gr GoRun, targets []GoTarget, rep *report.Report) {
	if len(targets) == 0 {
		return
	}
	bin, err := GremlinsPath(gr.Cfg)
	if err != nil {
		rep.CouldNotRun = append(rep.CouldNotRun, "go engine: "+err.Error())
		return
	}
	type pkgKey struct{ mod, pkg string }
	groups := map[pkgKey][]GoTarget{}
	for _, t := range targets {
		mod := moduleRoot(gr.Repo, t.Path)
		if mod == "" {
			rep.CouldNotRun = append(rep.CouldNotRun, t.Path+": no go.mod above it")
			continue
		}
		pkg, _ := filepath.Rel(mod, filepath.Dir(filepath.Join(gr.Repo, t.Path)))
		k := pkgKey{mod, pkg}
		groups[k] = append(groups[k], t)
	}
	var keys []pkgKey
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].mod+keys[i].pkg < keys[j].mod+keys[j].pkg })
	for _, k := range keys {
		runPkg(ctx, gr, bin, k.mod, k.pkg, groups[k], rep)
	}
}

func runPkg(ctx context.Context, gr GoRun, bin, mod, pkg string, ts []GoTarget, rep *report.Report) {
	label := filepath.Join(mod, pkg)
	fail := func(msg string) { rep.CouldNotRun = append(rep.CouldNotRun, label+": "+msg) }
	outf, err := os.CreateTemp("", "changeling-gremlins-*.json")
	if err != nil {
		fail(err.Error())
		return
	}
	outPath := outf.Name()
	outf.Close()
	_ = os.Remove(outPath)
	defer os.Remove(outPath)
	args := []string{"unleash", "-o", outPath}
	diffMode := false
	for _, t := range ts {
		if !t.Whole {
			diffMode = true
		}
	}
	if diffMode && gr.Base != "" {
		mb, err := gitx.Run(gr.Repo, "merge-base", gr.Base, "HEAD")
		if err != nil {
			fail(err.Error())
			return
		}
		args = append(args, "--diff", strings.TrimSpace(mb))
	}
	args = append(args, "./"+pkg)
	gr.Log("%s: gremlins %v", label, args)
	cctx := ctx
	if !gr.Deadline.IsZero() {
		var cancel context.CancelFunc
		cctx, cancel = context.WithDeadline(ctx, gr.Deadline)
		defer cancel()
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = mod
	cmd.Env = gitx.Env()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var buf strings.Builder
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		fail(err.Error())
		return
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-cctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		fail("gremlins cut off by the budget/signal")
		return
	}
	if err != nil {
		fail(fmt.Sprintf("gremlins exited %v: %s", err, lastLines(buf.String(), 5)))
		return
	}
	b, err := os.ReadFile(outPath)
	if err != nil {
		fail("gremlins exited 0 but wrote no report: " + lastLines(buf.String(), 5))
		return
	}
	var g gremlinsOut
	if err := json.Unmarshal(b, &g); err != nil {
		fail("gremlins report unparseable: " + err.Error())
		return
	}
	want := map[string]GoTarget{}
	for _, t := range ts {
		want[filepath.Base(t.Path)] = t
	}
	seen := map[string]*report.File{}
	for _, f := range g.Files {
		t, ok := want[filepath.Base(f.FileName)]
		if !ok {
			continue
		}
		fr := seen[t.Path]
		if fr == nil {
			fr = &report.File{Path: t.Path, Engine: "go", Functions: []report.Function{}}
			seen[t.Path] = fr
		}
		fnIdx := map[string]int{}
		for i, m := range f.Mutations {
			st, ok := gremlinsStatus[m.Status]
			if !ok {
				st = report.NotRun
			}
			fn := "<file>"
			if !t.Whole {
				fn = "<outside-range>"
				for _, r := range t.Changes {
					if r.Contains(m.Line) {
						fn = rangeName(r)
					}
				}
			}
			idx, ok := fnIdx[fn]
			if !ok {
				fr.Functions = append(fr.Functions, report.Function{Name: fn, Changed: !t.Whole, KillingTests: []string{}, CoveringTests: []string{}})
				idx = len(fr.Functions) - 1
				fnIdx[fn] = idx
			}
			fr.Functions[idx].MutantsInScope++
			rm := report.Mutant{ID: fmt.Sprintf("gremlins-%d-%d-%s-%d", m.Line, m.Column, m.Type, i), Engine: "go", File: t.Path, Function: fn, Line: m.Line, Col: m.Column, Operator: m.Type, Status: st}
			if eq, ok := gr.Equivalents[t.Path+"#"+rm.ID]; ok {
				rm.Status, rm.IgnoreReason = report.Ignored, eq.Reason
			}
			if st == report.Killed || st == report.Timeout {
				// gremlins does not name the killing test; record the package suite.
				rm.KilledBy = "go test ./" + pkg
				if !contains(fr.Functions[idx].KillingTests, rm.KilledBy) {
					fr.Functions[idx].KillingTests = append(fr.Functions[idx].KillingTests, rm.KilledBy)
				}
			}
			rep.Mutants = append(rep.Mutants, rm)
		}
	}
	for _, t := range ts {
		if fr := seen[t.Path]; fr != nil {
			rep.Files = append(rep.Files, *fr)
		} else {
			rep.Files = append(rep.Files, report.File{Path: t.Path, Engine: "go", Functions: []report.Function{}})
			rep.Notes = append(rep.Notes, t.Path+": gremlins found no mutants in the changed lines")
		}
	}
}
