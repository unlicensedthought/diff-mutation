package engine

import (
	"context"
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

// TSTarget is one changed TypeScript file.
type TSTarget struct {
	Path    string
	Changes []diffscope.Range
	Whole   bool
}

// TSRun configures the Stryker shell-out.
type TSRun struct {
	Repo        string
	Work        string
	Log         func(string, ...any)
	Equivalents map[string]config.Equivalent
	Deadline    time.Time
}

// surfaceOf returns the Stryker surface of rel: the nearest ancestor directory
// (below the repo root or at it) that holds a package.json. "" means none.
func surfaceOf(repo, rel string) string {
	for d := filepath.Dir(rel); ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(repo, d, "package.json")); err == nil {
			return filepath.ToSlash(d)
		}
		if d == "." || d == string(filepath.Separator) {
			return ""
		}
	}
}

// packageManagerOf names the package manager Stryker should assume, from the
// lockfile next to the surface or at the repo root.
func packageManagerOf(repo, dir string) string {
	for _, d := range []string{dir, repo} {
		for _, l := range [][2]string{{"pnpm-lock.yaml", "pnpm"}, {"yarn.lock", "yarn"}} {
			if _, err := os.Stat(filepath.Join(d, l[0])); err == nil {
				return l[1]
			}
		}
	}
	return "npm"
}

// strykerBin resolves @stryker-mutator/core from dir upward to repo, by path,
// never through npx (which can resolve a stale namesake).
func strykerBin(repo, dir string) (string, string, error) {
	for d := dir; ; d = filepath.Dir(d) {
		pj := filepath.Join(d, "node_modules", "@stryker-mutator", "core", "package.json")
		if b, err := os.ReadFile(pj); err == nil {
			var p struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			}
			if json.Unmarshal(b, &p) == nil && p.Name == "@stryker-mutator/core" {
				// The package manager's own shim in the SAME node_modules: under
				// pnpm's global virtual store, running bin/stryker.js directly
				// cannot resolve its peer `typescript` (ERR_MODULE_NOT_FOUND,
				// measured 2026-10-29); the shim sets the resolution paths.
				shim := filepath.Join(d, "node_modules", ".bin", "stryker")
				if _, err := os.Stat(shim); err == nil {
					return shim, p.Version, nil
				}
				return "", "", fmt.Errorf("@stryker-mutator/core %s is installed under %s but %s is missing; reinstall", p.Version, d, shim)
			}
		}
		if d == repo || d == filepath.Dir(d) {
			break
		}
	}
	return "", "", fmt.Errorf("@stryker-mutator/core is not installed under %s or any parent up to the repo root (npm ci first); refusing to fall through to npx", dir)
}

// pluginEntry resolves a Stryker plugin package from dir upward to repo and
// returns the absolute path of its ESM entry. Stryker resolves BARE plugin names
// relative to @stryker-mutator/core's own location; under pnpm's global virtual
// store that location holds only core's declared deps, so a bare
// "@stryker-mutator/vitest-runner" fails with "no TestRunner plugins were
// loaded" (measured 2026-10-29). An absolute path is imported as a file URL.
func pluginEntry(repo, dir, pkg string) (string, error) {
	for d := dir; ; d = filepath.Dir(d) {
		root := filepath.Join(d, "node_modules", filepath.FromSlash(pkg))
		if b, err := os.ReadFile(filepath.Join(root, "package.json")); err == nil {
			var p struct {
				Main    string          `json:"main"`
				Exports json.RawMessage `json:"exports"`
			}
			if err := json.Unmarshal(b, &p); err != nil {
				return "", fmt.Errorf("%s/package.json unparseable: %w", root, err)
			}
			entry := p.Main
			var ex map[string]json.RawMessage
			if len(p.Exports) > 0 && json.Unmarshal(p.Exports, &ex) == nil {
				var dot string
				if raw, ok := ex["."]; ok && json.Unmarshal(raw, &dot) == nil && dot != "" {
					entry = dot
				}
			}
			if entry == "" {
				return "", fmt.Errorf("%s declares neither exports[\".\"] nor main", root)
			}
			abs, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(entry)))
			if err != nil {
				return "", fmt.Errorf("%s entry %s: %w", pkg, entry, err)
			}
			return abs, nil
		}
		if d == repo || d == filepath.Dir(d) {
			break
		}
	}
	return "", fmt.Errorf("%s is not installed under %s or any parent up to the repo root", pkg, dir)
}

// strykerReport is the subset of mutation-testing-report-schema we read.
type strykerReport struct {
	Files map[string]struct {
		Mutants []struct {
			ID          string   `json:"id"`
			MutatorName string   `json:"mutatorName"`
			Replacement string   `json:"replacement"`
			Status      string   `json:"status"`
			KilledBy    []string `json:"killedBy"`
			Location    struct {
				Start struct{ Line, Column int } `json:"start"`
			} `json:"location"`
		} `json:"mutants"`
	} `json:"files"`
	TestFiles map[string]struct {
		Tests []struct{ ID, Name string } `json:"tests"`
	} `json:"testFiles"`
}

var strykerStatus = map[string]string{
	"Killed": report.Killed, "Timeout": report.Timeout, "Survived": report.Survived,
	"NoCoverage": report.NoCoverage, "Ignored": report.Ignored, "CompileError": report.Invalid,
	"RuntimeError": report.Invalid, "Pending": report.NotRun,
}

// RunTS shells out to Stryker once per surface with mutate restricted to the
// changed files and line ranges, incremental on.
func RunTS(ctx context.Context, tr TSRun, targets []TSTarget, rep *report.Report) {
	bySurface := map[string][]TSTarget{}
	for _, t := range targets {
		s := surfaceOf(tr.Repo, t.Path)
		if s == "" {
			rep.CouldNotRun = append(rep.CouldNotRun, t.Path+": no package.json above it; no stryker surface")
			continue
		}
		bySurface[s] = append(bySurface[s], t)
	}
	var surfaces []string
	for s := range bySurface {
		surfaces = append(surfaces, s)
	}
	sort.Strings(surfaces)
	for _, s := range surfaces {
		runSurface(ctx, tr, s, bySurface[s], rep)
	}
}

func rangeName(r diffscope.Range) string { return fmt.Sprintf("L%d-%d", r.Start, r.End) }

func runSurface(ctx context.Context, tr TSRun, surface string, ts []TSTarget, rep *report.Report) {
	dir := filepath.Join(tr.Repo, surface)
	fail := func(msg string) { rep.CouldNotRun = append(rep.CouldNotRun, surface+": "+msg) }
	bin, ver, err := strykerBin(tr.Repo, dir)
	if err != nil {
		fail(err.Error())
		return
	}
	runner, err := pluginEntry(tr.Repo, dir, "@stryker-mutator/vitest-runner")
	if err != nil {
		fail(err.Error())
		return
	}
	var cfgFile string
	for _, n := range []string{"vitest.mutation.config.ts", "vitest.unit.config.ts", "vitest.config.ts", "vitest.config.mts"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			cfgFile = filepath.Join(dir, n)
			break
		}
	}
	var mutate, files []string
	for _, t := range ts {
		rel, _ := filepath.Rel(dir, filepath.Join(tr.Repo, t.Path))
		files = append(files, rel)
		if t.Whole || len(t.Changes) == 0 {
			mutate = append(mutate, rel)
			continue
		}
		for _, r := range t.Changes {
			mutate = append(mutate, fmt.Sprintf("%s:%d-%d", rel, r.Start, r.End))
		}
	}
	tmp := filepath.Join(dir, ".stryker-tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		fail(err.Error())
		return
	}
	reportPath := filepath.Join(tmp, "changeling-report.json")
	_ = os.Remove(reportPath)
	vit := map[string]any{"related": true}
	if cfgFile != "" {
		vit["configFile"] = cfgFile
	}
	conf := map[string]any{
		"packageManager":    packageManagerOf(tr.Repo, dir),
		"testRunner":        "vitest",
		"coverageAnalysis":  "perTest",
		"plugins":           []string{runner},
		"vitest":            vit,
		"mutate":            mutate,
		"disableTypeChecks": "{" + strings.Join(files, ",") + "}",
		"incremental":       true,
		"incrementalFile":   filepath.Join(tmp, "changeling-incremental.json"),
		"reporters":         []string{"json"},
		"jsonReporter":      map[string]string{"fileName": reportPath},
		"thresholds":        map[string]any{"high": 80, "low": 60, "break": nil},
		"timeoutMS":         60000,
		"concurrency":       2,
	}
	cb, _ := json.MarshalIndent(conf, "", "  ")
	cfgPath := filepath.Join(tmp, "changeling.stryker.json")
	if err := os.WriteFile(cfgPath, cb, 0o644); err != nil {
		fail(err.Error())
		return
	}
	tr.Log("%s: stryker %s (%s), mutate %v", surface, ver, bin, mutate)
	cctx := ctx
	var cancel context.CancelFunc
	if !tr.Deadline.IsZero() {
		cctx, cancel = context.WithDeadline(ctx, tr.Deadline)
		defer cancel()
	}
	start := time.Now()
	cmd := exec.Command(bin, "run", cfgPath)
	cmd.Dir = dir
	cmd.Env = gitx.Env()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	logPath := filepath.Join(tmp, "changeling-stryker.log")
	lf, err := os.Create(logPath)
	if err != nil {
		fail(err.Error())
		return
	}
	cmd.Stdout, cmd.Stderr = lf, lf
	if err := cmd.Start(); err != nil {
		lf.Close()
		fail("stryker did not start: " + err.Error())
		return
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var werr error
	select {
	case werr = <-done:
	case <-cctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		lf.Close()
		fail("stryker cut off by the budget/signal; log " + logPath)
		return
	}
	lf.Close()
	if werr != nil {
		fail(fmt.Sprintf("stryker exited %v (an infra failure, not a score: break is null); log %s", werr, logPath))
		return
	}
	fi, err := os.Stat(reportPath)
	if err != nil || fi.ModTime().Before(start) {
		fail("stryker exited 0 but wrote no fresh report at " + reportPath)
		return
	}
	b, _ := os.ReadFile(reportPath)
	var sr strykerReport
	if err := json.Unmarshal(b, &sr); err != nil {
		fail("stryker report unparseable: " + err.Error())
		return
	}
	names := map[string]string{}
	for _, tf := range sr.TestFiles {
		for _, t := range tf.Tests {
			names[t.ID] = t.Name
		}
	}
	for _, t := range ts {
		rel, _ := filepath.Rel(dir, filepath.Join(tr.Repo, t.Path))
		fr := report.File{Path: t.Path, Engine: "ts"}
		fnIdx := map[string]*report.Function{}
		var order []string
		fnFor := func(line int) string {
			if t.Whole || len(t.Changes) == 0 {
				return "<file>"
			}
			for _, r := range t.Changes {
				if r.Contains(line) {
					return rangeName(r)
				}
			}
			return "<outside-range>"
		}
		f, ok := sr.Files[rel]
		if !ok {
			fr.CouldNotRun = append(fr.CouldNotRun, "stryker report has no entry for "+rel)
			rep.CouldNotRun = append(rep.CouldNotRun, t.Path+": stryker report has no entry for it")
		}
		for _, m := range f.Mutants {
			st, known := strykerStatus[m.Status]
			if !known {
				st = report.NotRun
			}
			fn := fnFor(m.Location.Start.Line)
			if _, ok := fnIdx[fn]; !ok {
				fnIdx[fn] = &report.Function{Name: fn, Changed: !t.Whole, KillingTests: []string{}, CoveringTests: []string{}}
				order = append(order, fn)
			}
			rm := report.Mutant{ID: "stryker-" + m.ID, Engine: "ts", File: t.Path, Function: fn, Line: m.Location.Start.Line, Col: m.Location.Start.Column, Operator: m.MutatorName, Replacement: m.Replacement, Status: st}
			if eq, ok := tr.Equivalents[t.Path+"#"+rm.ID]; ok {
				rm.Status, rm.IgnoreReason = report.Ignored, eq.Reason
			}
			if len(m.KilledBy) > 0 {
				rm.KilledBy = names[m.KilledBy[0]]
				if rm.KilledBy == "" {
					rm.KilledBy = m.KilledBy[0]
				}
			}
			fe := fnIdx[fn]
			fe.MutantsInScope++
			if (rm.Status == report.Killed || rm.Status == report.Timeout) && rm.KilledBy != "" && !contains(fe.KillingTests, rm.KilledBy) {
				fe.KillingTests = append(fe.KillingTests, rm.KilledBy)
			}
			rep.Mutants = append(rep.Mutants, rm)
		}
		for _, n := range order {
			fr.Functions = append(fr.Functions, *fnIdx[n])
		}
		rep.Files = append(rep.Files, fr)
	}
}
