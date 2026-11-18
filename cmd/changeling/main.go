// Command changeling is a diff-scoped mutation conductor: one engine per
// language (bash: built in, on a real bash AST; TypeScript: the repo's
// Stryker; Go: pinned Gremlins), scoped to the functions a change touched.
//
// Exit codes, never conflated:
//
//	0  clean: every mutant in scope was evaluated and none survived
//	1  survivors: the run completed and at least one mutant survived
//	2  COULD-NOT-RUN: missing oracle, red oracle, missing tool, budget cut,
//	   poisoned sandbox, bad usage. Not a verdict about the code.
//
// Subcommands: run, select, score, gate, selftest, version.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/unlicensedthought/diff-mutation/internal/config"
	"github.com/unlicensedthought/diff-mutation/internal/diffscope"
	"github.com/unlicensedthought/diff-mutation/internal/engine"
	"github.com/unlicensedthought/diff-mutation/internal/gitx"
	"github.com/unlicensedthought/diff-mutation/internal/hostload"
	"github.com/unlicensedthought/diff-mutation/internal/report"
	"github.com/unlicensedthought/diff-mutation/internal/sandbox"
)

const version = "changeling 1.0.0"

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "changeling: "+format+"\n", a...)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var rc int
	switch os.Args[1] {
	case "run":
		rc = cmdRun(os.Args[2:])
	case "select":
		rc = cmdSelect(os.Args[2:])
	case "score":
		rc = cmdScore(os.Args[2:])
	case "gate":
		rc = cmdGate(os.Args[2:])
	case "selftest":
		rc = cmdSelftest(os.Args[2:])
	case "version", "--version":
		fmt.Println(version)
	case "-h", "--help", "help":
		usage()
	default:
		logf("unknown subcommand %q", os.Args[1])
		usage()
		rc = 2
	}
	os.Exit(rc)
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  changeling run    [--base REF | --target FILE[:fn1,fn2]...] [--engines bash,ts,go] [--out DIR] ...
  changeling select --base REF        exit 0 = something in scope, 1 = nothing, 2 = error
  changeling score  REPORT.json       recompute every count from the mutant list
  changeling gate   REPORT.json [--not-before RFC3339]
                                      exit 0 pass, 1 zero-coverage changed function, 2 could-not-run
  changeling selftest                 two-way self-check on the embedded fixture
`)
}

type targets []string

func (t *targets) String() string     { return strings.Join(*t, " ") }
func (t *targets) Set(v string) error { *t = append(*t, v); return nil }

type common struct {
	repo, cfgPath, base string
}

func (c *common) bind(fs *flag.FlagSet) {
	fs.StringVar(&c.repo, "repo", "", "repository root (default: git toplevel of cwd)")
	fs.StringVar(&c.cfgPath, "config", "changeling.json", "config, repo-relative")
	fs.StringVar(&c.base, "base", "", "diff base ref: mutate only what changed since its merge base")
}

func (c *common) resolve() (*config.Config, error) {
	if c.repo == "" {
		out, err := gitx.Run(".", "rev-parse", "--show-toplevel")
		if err != nil {
			return nil, err
		}
		c.repo = strings.TrimSpace(out)
	}
	abs, err := filepath.Abs(c.repo)
	if err != nil {
		return nil, err
	}
	c.repo = abs
	return config.Load(c.repo, c.cfgPath)
}

type plan struct {
	bash []engine.BashTarget
	ts   []engine.TSTarget
	gov  []engine.GoTarget
}

func (p plan) empty() bool { return len(p.bash)+len(p.ts)+len(p.gov) == 0 }

func isTest(rel string) bool {
	return strings.Contains(rel, "__tests__/") || strings.HasSuffix(rel, ".test.ts") || strings.HasSuffix(rel, "_test.go") || strings.HasSuffix(rel, ".test.sh")
}

func diffPlan(repo string, cfg *config.Config, base string, engines map[string]bool) (plan, error) {
	var p plan
	ch, err := diffscope.Compute(repo, base)
	if err != nil {
		return p, err
	}
	for rel, rs := range ch {
		if isTest(rel) {
			continue
		}
		if _, err := os.Stat(filepath.Join(repo, rel)); err != nil {
			continue // deleted
		}
		switch {
		case engines["bash"] && config.InScope(rel, cfg.Scopes.Bash):
			p.bash = append(p.bash, engine.BashTarget{Path: rel, Changes: rs, Diff: true})
		case engines["ts"] && strings.HasSuffix(rel, ".ts") && !strings.HasSuffix(rel, ".d.ts") && config.InScope(rel, cfg.Scopes.TS):
			p.ts = append(p.ts, engine.TSTarget{Path: rel, Changes: rs})
		case engines["go"] && strings.HasSuffix(rel, ".go") && config.InScope(rel, cfg.Scopes.Go):
			p.gov = append(p.gov, engine.GoTarget{Path: rel, Changes: rs})
		}
	}
	return p, nil
}

func explicitPlan(ts targets) plan {
	var p plan
	for _, t := range ts {
		file, fns, _ := strings.Cut(t, ":")
		var list []string
		if fns != "" {
			list = strings.Split(fns, ",")
		}
		switch {
		case strings.HasSuffix(file, ".ts"):
			p.ts = append(p.ts, engine.TSTarget{Path: file, Whole: true})
		case strings.HasSuffix(file, ".go"):
			p.gov = append(p.gov, engine.GoTarget{Path: file, Whole: true})
		default:
			p.bash = append(p.bash, engine.BashTarget{Path: file, Funcs: list})
		}
	}
	return p
}

func parseEngines(s string) map[string]bool {
	m := map[string]bool{}
	for _, e := range strings.Split(s, ",") {
		m[strings.TrimSpace(e)] = true
	}
	return m
}

func cmdSelect(args []string) int {
	fs := flag.NewFlagSet("select", flag.ContinueOnError)
	var c common
	c.bind(fs)
	eng := fs.String("engines", "bash,ts,go", "engines to consider")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := c.resolve()
	if err != nil || c.base == "" {
		logf("select: %v (needs --base)", err)
		return 2
	}
	p, err := diffPlan(c.repo, cfg, c.base, parseEngines(*eng))
	if err != nil {
		logf("select: %v", err)
		return 2
	}
	for _, t := range p.bash {
		fmt.Println("bash", t.Path)
	}
	for _, t := range p.ts {
		fmt.Println("ts", t.Path)
	}
	for _, t := range p.gov {
		fmt.Println("go", t.Path)
	}
	if p.empty() {
		return 1
	}
	return 0
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var c common
	c.bind(fs)
	var ts targets
	fs.Var(&ts, "target", "explicit target FILE[:fn1,fn2] (repeatable); bash functions; <top> names top-level code")
	eng := fs.String("engines", "bash,ts,go", "engines to run")
	out := fs.String("out", ".changeling", "output dir (report JSON + summary markdown), repo-relative or absolute")
	work := fs.String("work", "", "sandbox dir (default $TMPDIR/changeling-work)")
	jobs := fs.Int("jobs", defaultJobs(), "parallel sandboxes")
	budget := fs.Duration("budget", 0, "wall budget; mutants left when it expires are not-run (=> exit 2)")
	testTimeout := fs.Duration("test-timeout", 10*time.Minute, "limit for one pristine oracle test")
	factor := fs.Float64("mutant-timeout-factor", 3, "per-mutant test limit = factor x pristine duration")
	minT := fs.Duration("mutant-timeout-min", 30*time.Second, "floor for the per-mutant test limit")
	maxM := fs.Int("max-mutants", 0, "refuse (exit 2) a file with more mutants in scope than this; 0 = no cap")
	envLabel := fs.String("environment", "", "where this ran (recorded next to every number)")
	sample := fs.Int("sample", 0, "landing mode: evaluate at most N mutants per function (operator-diverse); the rest are not-sampled, listed, unscored")
	waitLoad := fs.Duration("wait-load", 0, "wait up to this long for load1 <= cores before refusing to start")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := c.resolve()
	if err != nil {
		logf("run: %v", err)
		return 2
	}
	eqs, err := config.LoadEquivalents(c.repo, cfg.Equivalents)
	if err != nil {
		logf("run: %v", err)
		return 2
	}
	outDir := *out
	if !filepath.IsAbs(outDir) {
		outDir = filepath.Join(c.repo, outDir)
	}
	reportPath := filepath.Join(outDir, "changeling-report.json")
	mdPath := filepath.Join(outDir, "changeling-summary.md")
	// Freshness: a report from an earlier run must never be read as this one's.
	_ = os.Remove(reportPath)
	_ = os.Remove(mdPath)
	start := time.Now().UTC()
	host, _ := os.Hostname()
	label := *envLabel
	if label == "" {
		label = fmt.Sprintf("%s %s/%s bash-oracle", host, runtime.GOOS, runtime.GOARCH)
	}
	rep := &report.Report{Schema: report.Schema, Tool: version, StartedAt: start.Format(time.RFC3339), Repo: c.repo, Base: c.base, Environment: label, CouldNotRun: []string{}, Files: []report.File{}, Mutants: []report.Mutant{}}
	if h, err := gitx.Run(c.repo, "rev-parse", "HEAD"); err == nil {
		rep.Head = strings.TrimSpace(h)
	}
	// Contention guard: refuse to start on an oversubscribed host. The run still
	// writes a COULD-NOT-RUN report so a gate reading it cannot see a stale one.
	err = hostload.Check(loadSource, runtime.NumCPU())
	for waitUntil := time.Now().Add(*waitLoad); err != nil && time.Now().Before(waitUntil); {
		logf("waiting for the host to drain before starting: %v", err)
		time.Sleep(15 * time.Second)
		err = hostload.Check(loadSource, runtime.NumCPU())
	}
	if err != nil {
		rep.CouldNotRun = append(rep.CouldNotRun, "REFUSED TO START: "+err.Error())
		return finish(rep, reportPath, mdPath)
	}
	var p plan
	switch {
	case len(ts) > 0 && c.base != "":
		logf("run: --target and --base are exclusive")
		return 2
	case len(ts) > 0:
		rep.Mode = "explicit"
		p = explicitPlan(ts)
	case c.base != "":
		rep.Mode = "diff"
		p, err = diffPlan(c.repo, cfg, c.base, parseEngines(*eng))
		if err != nil {
			rep.CouldNotRun = append(rep.CouldNotRun, "diff: "+err.Error())
		}
	default:
		logf("run: give --base REF or --target FILE")
		return 2
	}
	if p.empty() && len(rep.CouldNotRun) == 0 {
		rep.Notes = append(rep.Notes, "nothing in scope: no changed file matches the configured scopes")
	}
	wd := *work
	if wd == "" {
		wd = filepath.Join(os.TempDir(), "changeling-work")
	}
	if err := os.MkdirAll(wd, 0o755); err != nil {
		logf("run: %v", err)
		return 2
	}
	for _, r := range sandbox.ReapStale(wd) {
		logf("reaped stale sandbox %s (its owner is gone)", r)
	}
	runWork, err := os.MkdirTemp(wd, "run-")
	if err != nil {
		logf("run: %v", err)
		return 2
	}
	defer os.RemoveAll(runWork)
	if err := sandbox.Own(runWork); err != nil {
		logf("run: %v", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var deadline time.Time
	if *budget > 0 {
		deadline = start.Add(*budget)
	}
	br := engine.BashRun{Repo: c.repo, Work: runWork, Jobs: *jobs, TestTimeout: *testTimeout, MutantFactor: *factor, MutantMin: *minT, Deadline: deadline, Cfg: cfg, Equivalents: eqs, Log: logf, MaxMutants: *maxM, Sample: *sample}
	_ = engine.RunBash(ctx, br, p.bash, rep)
	engine.RunTS(ctx, engine.TSRun{Repo: c.repo, Work: runWork, Log: logf, Equivalents: eqs, Deadline: deadline}, p.ts, rep)
	engine.RunGo(ctx, engine.GoRun{Repo: c.repo, Base: c.base, Cfg: cfg, Log: logf, Equivalents: eqs, Deadline: deadline}, p.gov, rep)
	if ctx.Err() != nil {
		rep.CouldNotRun = append(rep.CouldNotRun, "interrupted by signal")
	}
	if c := report.Tally(rep.Mutants, nil); report.TimeoutContended(c) {
		rep.CouldNotRun = append(rep.CouldNotRun, fmt.Sprintf("CONTENDED: %d of %d executed mutants timed out (> %.0f%%); a contended run is not a verdict", c.Timeout, c.Killed+c.Timeout+c.Survived, 100*report.MaxTimeoutRatio))
	}
	return finish(rep, reportPath, mdPath)
}

// loadSource reads load1; CHANGELING_LOAD1 injects a value (the guard's own
// negative proof sets it above the core count).
func loadSource() (float64, error) {
	if v := os.Getenv("CHANGELING_LOAD1"); v != "" {
		return strconv.ParseFloat(v, 64)
	}
	return hostload.Load1()
}

func finish(rep *report.Report, reportPath, mdPath string) int {
	rep.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	rep.Verdict = rep.DeriveVerdict()
	if err := rep.Write(reportPath); err != nil {
		logf("run: cannot write report: %v", err)
		return 2
	}
	md := report.Markdown(rep)
	if err := os.WriteFile(mdPath, []byte(md), 0o644); err != nil {
		logf("run: cannot write summary: %v", err)
		return 2
	}
	fmt.Print(md)
	logf("report %s (verdict %s)", reportPath, rep.Verdict)
	return report.ExitCode(rep.Verdict)
}

func defaultJobs() int {
	n := runtime.NumCPU() / 2
	if n < 1 {
		n = 1
	}
	if n > 6 {
		n = 6
	}
	return n
}

func cmdScore(args []string) int {
	if len(args) != 1 {
		logf("score: give one report path")
		return 2
	}
	r, err := report.Read(args[0])
	if err != nil {
		logf("score: %v", err)
		return 2
	}
	type fileScore struct {
		File      string                   `json:"file"`
		Counts    report.Counts            `json:"counts"`
		Functions map[string]report.Counts `json:"functions"`
	}
	out := struct {
		Verdict string        `json:"verdict"`
		All     report.Counts `json:"all"`
		Files   []fileScore   `json:"files"`
	}{Verdict: r.DeriveVerdict(), All: report.Tally(r.Mutants, nil)}
	for _, f := range r.Files {
		fsc := fileScore{File: f.Path, Counts: report.Tally(r.Mutants, func(m report.Mutant) bool { return m.File == f.Path }), Functions: map[string]report.Counts{}}
		for _, fn := range f.Functions {
			fsc.Functions[fn.Name] = report.Tally(r.Mutants, func(m report.Mutant) bool { return m.File == f.Path && m.Function == fn.Name })
		}
		out.Files = append(out.Files, fsc)
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	if r.Verdict != out.Verdict {
		logf("score: the report's stored verdict %q disagrees with the recomputed %q; trust the recomputation", r.Verdict, out.Verdict)
	}
	return 0
}

func cmdGate(args []string) int {
	fs := flag.NewFlagSet("gate", flag.ContinueOnError)
	nb := fs.String("not-before", "", "RFC3339: refuse a report started before this instant")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		logf("gate: give one report path")
		return 2
	}
	r, err := report.Read(fs.Arg(0))
	if err != nil {
		logf("gate: COULD-NOT-RUN: %v", err)
		return 2
	}
	var t time.Time
	if *nb != "" {
		t, err = time.Parse(time.RFC3339, *nb)
		if err != nil {
			logf("gate: --not-before: %v", err)
			return 2
		}
	}
	g := report.Gate(r, t)
	for _, reason := range g.Reasons {
		logf("gate: %s", reason)
	}
	switch g.Exit {
	case 0:
		c := report.Tally(r.Mutants, nil)
		logf("gate: PASS (survivors=%d are review evidence, not a gate)", c.Survived+c.NoCoverage)
	case 1:
		logf("gate: FAIL")
	default:
		logf("gate: COULD-NOT-RUN")
	}
	return g.Exit
}
