// Package engine drives one language's mutants against its oracle.
package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/unlicensedthought/diff-mutation/internal/bashmut"
	"github.com/unlicensedthought/diff-mutation/internal/config"
	"github.com/unlicensedthought/diff-mutation/internal/diffscope"
	"github.com/unlicensedthought/diff-mutation/internal/report"
	"github.com/unlicensedthought/diff-mutation/internal/sandbox"
)

// BashTarget is one file to mutate and how to select inside it.
type BashTarget struct {
	Path    string            // repo-relative
	Funcs   []string          // explicit functions (explicit mode)
	Changes []diffscope.Range // diff mode: changed new-side ranges
	Diff    bool
}

// BashRun holds the knobs for a bash engine run.
type BashRun struct {
	Repo         string
	Work         string
	Jobs         int
	TestTimeout  time.Duration // pristine test limit
	MutantFactor float64       // per-mutant test limit = max(MutantMin, factor x pristine duration)
	MutantMin    time.Duration
	Deadline     time.Time // zero = none; past it, remaining mutants are not-run
	Cfg          *config.Config
	Equivalents  map[string]config.Equivalent
	Log          func(format string, a ...any)
	MaxMutants   int // 0 = unlimited; above it the file is could-not-run (an unbounded run is a lie about its own cost)
	Sample       int // 0 = every mutant; else at most this many per function (landing mode)
}

// Discover returns the oracle tests for rel: every test file matching the
// configured globs that names the file, adjusted by the override map.
func Discover(repo, rel string, cfg *config.Config) ([]string, error) {
	ov := cfg.Oracle.Overrides[rel]
	if len(ov.Only) > 0 {
		return append([]string{}, ov.Only...), nil
	}
	base := filepath.Base(rel)
	// The basename as a whole token: `deploy` must not match `deploy-foo`.
	tok := regexp.MustCompile(`(^|[^A-Za-z0-9_.-])` + regexp.QuoteMeta(base) + `($|[^A-Za-z0-9_-])`)
	excl := map[string]bool{}
	for _, e := range ov.Exclude {
		excl[e] = true
	}
	set := map[string]bool{}
	for _, g := range cfg.Oracle.TestGlobs {
		ms, err := filepath.Glob(filepath.Join(repo, g))
		if err != nil {
			return nil, err
		}
		for _, m := range ms {
			r, _ := filepath.Rel(repo, m)
			if excl[r] {
				continue
			}
			b, err := os.ReadFile(m)
			if err != nil {
				return nil, err
			}
			if strings.Contains(string(b), rel) || tok.Match(b) {
				set[r] = true
			}
		}
	}
	for _, a := range ov.Add {
		set[a] = true
	}
	var out []string
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out, nil
}

type pool struct {
	boxes chan *sandbox.Box
	all   []*sandbox.Box
}

func newPool(repo, work string, n int, log func(string, ...any)) (*pool, error) {
	p := &pool{boxes: make(chan *sandbox.Box, n)}
	// Built concurrently: each copy is I/O-bound (the whole tracked tree plus a
	// throwaway commit), and serially a 10-box pool took ~10 min to start.
	boxes := make([]*sandbox.Box, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			boxes[i], errs[i] = sandbox.New(repo, work, i)
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			return nil, errs[i]
		}
		p.all = append(p.all, boxes[i])
		p.boxes <- boxes[i]
	}
	log("sandbox pool: %d isolated copies under %s", n, work)
	return p, nil
}

// RunBash mutates every target and appends files + mutants to rep.
func RunBash(ctx context.Context, br BashRun, targets []BashTarget, rep *report.Report) error {
	if len(targets) == 0 {
		return nil
	}
	p, err := newPool(br.Repo, br.Work, br.Jobs, br.Log)
	if err != nil {
		rep.CouldNotRun = append(rep.CouldNotRun, "sandbox: "+err.Error())
		return nil
	}
	// The budget is a WALL bound, as it already is for the TS and Go engines:
	// every oracle test (coverage pass and mutant) runs under a context that
	// expires at the deadline. If only the between-mutants check read the
	// deadline, a coverage pass over a slow suite and the in-flight mutant
	// tests would run past it to their own timeouts.
	// Past the deadline, remaining mutants are not-run and the file is
	// COULD-NOT-RUN "budget cut", never a verdict. The parent ctx (signals)
	// stays distinct: an expired budget is not "interrupted by signal".
	if !br.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, br.Deadline)
		defer cancel()
	}
	for _, t := range targets {
		if ctx.Err() != nil {
			rep.CouldNotRun = append(rep.CouldNotRun, t.Path+": "+interruption(ctx, br.Deadline))
			continue
		}
		runFile(ctx, br, p, t, rep)
	}
	return nil
}

// interruption names why ctx is done: the budget's deadline passed, or the
// run was cancelled (a signal). A budget cut must read as a budget cut.
func interruption(ctx context.Context, deadline time.Time) string {
	if !deadline.IsZero() && time.Now().After(deadline) {
		return "budget cut: the deadline " + deadline.UTC().Format(time.RFC3339) + " passed"
	}
	return "interrupted"
}

func runFile(ctx context.Context, br BashRun, p *pool, t BashTarget, rep *report.Report) {
	fr := report.File{Path: t.Path, Engine: "bash"}
	defer func() { rep.Files = append(rep.Files, fr) }()
	fail := func(msg string) {
		fr.CouldNotRun = append(fr.CouldNotRun, msg)
		rep.CouldNotRun = append(rep.CouldNotRun, t.Path+": "+msg)
	}
	src, err := os.ReadFile(filepath.Join(br.Repo, t.Path))
	if err != nil {
		fail("cannot read: " + err.Error())
		return
	}
	pristine := sandbox.Hash(src)
	fr.PristineSha = pristine
	s, err := bashmut.Parse(t.Path, src)
	if err != nil {
		fail("does not parse as bash: " + err.Error())
		return
	}
	sel := bashmut.Selector{Funcs: map[string]bool{}}
	changed := map[string]bool{}
	if t.Diff {
		ov := func(a, b int) bool {
			for _, r := range t.Changes {
				if r.Overlaps(a, b) {
					return true
				}
			}
			return false
		}
		for _, f := range s.ChangedFuncs(ov) {
			sel.Funcs[f] = true
			changed[f] = true
		}
		sel.LineRange = func(l int) bool { return ov(l, l) }
	} else {
		known := map[string]bool{}
		for _, f := range s.Funcs {
			known[f.Name] = true
		}
		var lines []diffscope.Range
		for _, f := range t.Funcs {
			// `L<a>-<b>`: top-level code on those lines (a function name never
			// matches this shape).
			if m := lineSpecRE.FindStringSubmatch(f); m != nil {
				a, _ := strconv.Atoi(m[1])
				b, _ := strconv.Atoi(m[2])
				lines = append(lines, diffscope.Range{Start: a, End: b})
				continue
			}
			if f == "*" {
				sel.All = true
				continue
			}
			if !known[f] && f != bashmut.TopLevel {
				fail(fmt.Sprintf("function %q not found", f))
				return
			}
			sel.Funcs[f] = true
		}
		if len(lines) > 0 {
			sel.LineRange = func(l int) bool {
				for _, r := range lines {
					if r.Contains(l) {
						return true
					}
				}
				return false
			}
		}
		if len(t.Funcs) == 0 {
			sel.All = true
		}
	}
	muts := s.Mutants(sel, bashmut.Options{NumericSkipCommands: br.Cfg.NumericSkipCommands})
	if t.Diff {
		// Top-level sites selected by line are a changed pseudo-function.
		for _, m := range muts {
			if m.Function == bashmut.TopLevel {
				changed[bashmut.TopLevel] = true
			}
		}
	}
	if br.MaxMutants > 0 && len(muts) > br.MaxMutants {
		fail(fmt.Sprintf("%d mutants in scope exceeds the cap of %d; narrow the scope (--func) instead of running a sweep nobody will finish", len(muts), br.MaxMutants))
		return
	}
	scopeDesc := fmt.Sprintf("%d named functions", len(sel.Funcs))
	if sel.All {
		scopeDesc = fmt.Sprintf("every function (%d) plus <top>", len(s.Funcs))
	}
	if sel.LineRange != nil {
		scopeDesc += " + top-level line ranges"
	}
	br.Log("%s: %s in scope, %d mutants", t.Path, scopeDesc, len(muts))
	tests, err := Discover(br.Repo, t.Path, br.Cfg)
	if err != nil {
		fail("oracle discovery: " + err.Error())
		return
	}
	if len(tests) == 0 {
		fail("NO ORACLE: no test under " + strings.Join(br.Cfg.Oracle.TestGlobs, ", ") + " names this file and the override map adds none")
		return
	}
	for _, tt := range tests {
		if _, err := os.Stat(filepath.Join(br.Repo, tt)); err != nil {
			fail("oracle test " + tt + " is missing")
			return
		}
	}

	// ---- coverage pass ---------------------------------------------------
	type cov struct {
		ot  report.OracleTest
		dur time.Duration
	}
	covs := make([]cov, len(tests))
	var wg sync.WaitGroup
	allFuncs := []string{bashmut.TopLevel}
	for _, f := range s.Funcs {
		allFuncs = append(allFuncs, f.Name)
	}
	var covErr error
	var covMu sync.Mutex
	covDone := 0
	for i, tt := range tests {
		wg.Add(1)
		go func(i int, tt string) {
			defer wg.Done()
			b := <-p.boxes
			defer func() { p.boxes <- b }()
			trace := filepath.Join(filepath.Dir(b.Root), "trace")
			inst, err := s.Instrument(trace)
			if err != nil {
				covMu.Lock()
				covErr = err
				covMu.Unlock()
				return
			}
			_ = os.WriteFile(trace, nil, 0o644)
			ot := report.OracleTest{Path: tt}
			if err := b.Write(t.Path, inst); err != nil {
				covMu.Lock()
				covErr = err
				covMu.Unlock()
				return
			}
			if ctx.Err() != nil {
				// Budget already gone (or a signal): do not start an oracle
				// test that the context would cancel at once and that would
				// then read as RED on the pristine file.
				ot.Instrumented, ot.Pristine = "not-run", "not-run"
				ot.Note = "not probed: " + interruption(ctx, br.Deadline)
				covs[i] = cov{ot: ot}
				covMu.Lock()
				covDone++
				covMu.Unlock()
				return
			}
			ri := b.Run(ctx, br.Cfg.Oracle.Runner, tt, br.TestTimeout)
			ot.Instrumented = status(ri)
			tr, _ := os.ReadFile(trace)
			hit := map[string]bool{}
			for _, l := range strings.Split(string(tr), "\n") {
				if l != "" {
					hit[l] = true
				}
			}
			_ = b.Write(t.Path, src)
			_, _ = b.Clean()
			dur := ri.Duration
			if ri.StartErr == "cancelled" {
				// The context ended mid-test: a budget cut, not an oracle
				// verdict. Do not re-run it on the pristine file.
				ot.Pristine = "not-run"
				ot.Note = "cut mid-test: " + interruption(ctx, br.Deadline)
				hit = nil
			} else if !ri.Passed() {
				rp := b.Run(ctx, br.Cfg.Oracle.Runner, tt, br.TestTimeout)
				_, _ = b.Clean()
				ot.Pristine = status(rp)
				dur = rp.Duration
				if rp.Passed() {
					ot.Note = "passes on the pristine file but FAILS once coverage probes are inserted (no line moved): suspect an assertion over the script's source text; treated as covering every function"
					hit = map[string]bool{}
					for _, f := range allFuncs {
						hit[f] = true
					}
				} else {
					ot.Note = "RED on the pristine file (exit " + fmt.Sprint(rp.Exit) + "): not an oracle; excluded. Tail: " + lastLines(rp.Tail, 3)
					hit = nil
				}
			} else {
				ot.Pristine = "pass"
			}
			for f := range hit {
				ot.Covers = append(ot.Covers, f)
			}
			sort.Strings(ot.Covers)
			ot.DurationMs = dur.Milliseconds()
			covs[i] = cov{ot: ot, dur: dur}
			// One line per oracle test: a coverage pass over a large suite runs
			// for many minutes and must never look like a hang.
			covMu.Lock()
			covDone++
			br.Log("%s: coverage %d/%d %s pristine=%s probed=%s %s, covers %d", t.Path, covDone, len(tests), tt, ot.Pristine, ot.Instrumented, dur.Round(time.Second), len(ot.Covers))
			covMu.Unlock()
		}(i, tt)
	}
	wg.Wait()
	if covErr != nil {
		fail("coverage instrumentation: " + covErr.Error())
		return
	}
	if ctx.Err() != nil {
		probed := 0
		for _, c := range covs {
			if c.ot.Pristine != "not-run" {
				probed++
			}
		}
		fail(fmt.Sprintf("%s during the coverage pass (%d of %d oracle tests probed); no mutant was evaluated", interruption(ctx, br.Deadline), probed, len(tests)))
		return
	}
	green := 0
	coveredBy := map[string][]cov{}
	for _, c := range covs {
		fr.Oracle = append(fr.Oracle, c.ot)
		if c.ot.Pristine != "pass" {
			continue
		}
		green++
		for _, f := range c.ot.Covers {
			coveredBy[f] = append(coveredBy[f], c)
		}
	}
	if green == 0 {
		fail(fmt.Sprintf("NO GREEN ORACLE: all %d discovered tests fail on the pristine file", len(tests)))
		return
	}
	for f := range coveredBy {
		sort.SliceStable(coveredBy[f], func(i, j int) bool { return coveredBy[f][i].dur < coveredBy[f][j].dur })
	}

	// ---- functions in scope ----------------------------------------------
	fnIndex := map[string]*report.Function{}
	addFn := func(name string, a, b int) {
		if _, ok := fnIndex[name]; ok {
			return
		}
		rf := &report.Function{Name: name, StartLine: a, EndLine: b, Changed: changed[name], KillingTests: []string{}, CoveringTests: []string{}}
		for _, c := range coveredBy[name] {
			rf.CoveringTests = append(rf.CoveringTests, c.ot.Path)
		}
		fnIndex[name] = rf
	}
	for _, f := range s.Funcs {
		if sel.Funcs[f.Name] {
			addFn(f.Name, f.StartLine, f.EndLine)
		}
	}
	for _, m := range muts {
		addFn(m.Function, 0, 0)
	}

	// ---- mutants: round-robin across functions so a budget cut still
	// leaves every function with evidence -------------------------------------
	byFn := map[string][]bashmut.Mutant{}
	var order []string
	for _, m := range muts {
		if _, ok := byFn[m.Function]; !ok {
			order = append(order, m.Function)
		}
		byFn[m.Function] = append(byFn[m.Function], m)
	}
	// Sampling (landing mode): at most Sample mutants per function, chosen
	// round-robin over operator classes so the sample is diverse. The rest are
	// recorded as not-sampled, listed, and excluded from every score.
	var sampledOut []report.Mutant
	if br.Sample > 0 {
		for _, f := range order {
			keep, rest := sampleDiverse(byFn[f], br.Sample)
			byFn[f] = keep
			for _, m := range rest {
				sampledOut = append(sampledOut, report.Mutant{ID: m.ID, Engine: "bash", File: m.File, Function: m.Function, Line: m.Line, Col: m.Col, Operator: m.Operator, Original: m.Original, Replacement: m.Replacement, Status: report.NotSampled})
			}
		}
	}
	// Kill-order learning: within a function, the test that killed the most
	// mutants so far runs first, so a kill usually costs one test, not many.
	kills := map[string]map[string]int{}
	var queue []bashmut.Mutant
	for i := 0; ; i++ {
		any := false
		for _, f := range order {
			if i < len(byFn[f]) {
				queue = append(queue, byFn[f][i])
				any = true
			}
		}
		if !any {
			break
		}
	}
	results := make([]report.Mutant, len(queue))
	var done, mu = 0, sync.Mutex{}
	lastLog := time.Now()
	var poisoned error
	for i, m := range queue {
		rm := report.Mutant{ID: m.ID, Engine: "bash", File: m.File, Function: m.Function, Line: m.Line, Col: m.Col, Operator: m.Operator, Original: m.Original, Replacement: m.Replacement, Diff: s.Diff(m)}
		if eq, ok := br.Equivalents[m.File+"#"+m.ID]; ok {
			rm.Status, rm.IgnoreReason = report.Ignored, eq.Reason
			results[i] = rm
			continue
		}
		mutated := s.Apply(m)
		if !bashmut.Valid(m.File, mutated) {
			rm.Status = report.Invalid
			results[i] = rm
			continue
		}
		cs := coveredBy[m.Function]
		if len(cs) == 0 {
			rm.Status = report.NoCoverage
			results[i] = rm
			continue
		}
		if ctx.Err() != nil || (!br.Deadline.IsZero() && time.Now().After(br.Deadline)) {
			rm.Status = report.NotRun
			results[i] = rm
			continue
		}
		b := <-p.boxes
		wg.Add(1)
		go func(i int, rm report.Mutant, mutated []byte, b *sandbox.Box) {
			defer wg.Done()
			defer func() { p.boxes <- b }()
			if err := b.Verify(m.File, pristine); err != nil {
				mu.Lock()
				poisoned = err
				mu.Unlock()
				rm.Status = report.NotRun
				results[i] = rm
				return
			}
			start := time.Now()
			if err := b.Write(m.File, mutated); err != nil {
				rm.Status = report.NotRun
				results[i] = rm
				return
			}
			rm.Status = report.Survived
			mu.Lock()
			ordered := append([]cov{}, cs...)
			kf := kills[m.Function]
			sort.SliceStable(ordered, func(a, b int) bool { return kf[ordered[a].ot.Path] > kf[ordered[b].ot.Path] })
			mu.Unlock()
			for _, c := range ordered {
				lim := time.Duration(float64(c.dur) * br.MutantFactor)
				if lim < br.MutantMin {
					lim = br.MutantMin
				}
				r := b.Run(ctx, br.Cfg.Oracle.Runner, c.ot.Path, lim)
				rm.TestsRun++
				if r.StartErr == "cancelled" {
					rm.Status = report.NotRun
					break
				}
				if r.TimedOut {
					rm.Status, rm.KilledBy = report.Timeout, c.ot.Path
					break
				}
				if !r.Passed() {
					rm.Status, rm.KilledBy = report.Killed, c.ot.Path
					break
				}
			}
			rm.DurationMs = time.Since(start).Milliseconds()
			// Restore, then undo anything the test wrote into the tree.
			werr := b.Write(m.File, src)
			_, cerr := b.Clean()
			verr := b.Verify(m.File, pristine)
			mu.Lock()
			defer mu.Unlock()
			if werr != nil || cerr != nil || verr != nil {
				poisoned = fmt.Errorf("could not restore sandbox after mutant %s: %v %v %v", rm.ID, werr, cerr, verr)
			}
			results[i] = rm
			if rm.KilledBy != "" {
				if kills[m.Function] == nil {
					kills[m.Function] = map[string]int{}
				}
				kills[m.Function][rm.KilledBy]++
			}
			done++
			if time.Since(lastLog) > 30*time.Second {
				lastLog = time.Now()
				br.Log("%s: %d/%d mutants evaluated", t.Path, done, len(queue))
			}
		}(i, rm, mutated, b)
		mu.Lock()
		pz := poisoned
		mu.Unlock()
		if pz != nil {
			break
		}
	}
	wg.Wait()
	if poisoned != nil {
		fail("SANDBOX POISONED: " + poisoned.Error() + " (the run is discarded: one wrong verdict taints every verdict from the same box)")
	}
	for i := range results {
		if results[i].Status == "" {
			results[i] = report.Mutant{ID: queue[i].ID, Engine: "bash", File: queue[i].File, Function: queue[i].Function, Line: queue[i].Line, Operator: queue[i].Operator, Original: queue[i].Original, Replacement: queue[i].Replacement, Status: report.NotRun}
		}
		r := results[i]
		if fn := fnIndex[r.Function]; fn != nil {
			fn.MutantsInScope++
			if (r.Status == report.Killed || r.Status == report.Timeout) && !contains(fn.KillingTests, r.KilledBy) {
				fn.KillingTests = append(fn.KillingTests, r.KilledBy)
			}
		}
	}
	rep.Mutants = append(rep.Mutants, results...)
	rep.Mutants = append(rep.Mutants, sampledOut...)
	if len(sampledOut) > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf("%s: SAMPLED at most %d mutants per function; %d mutants not sampled (listed, excluded from every score)", t.Path, br.Sample, len(sampledOut)))
	}
	var names []string
	for n := range fnIndex {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		sort.Strings(fnIndex[n].KillingTests)
		fr.Functions = append(fr.Functions, *fnIndex[n])
	}
	// The original must be byte-identical to what we read: proof, not hope.
	now, err := os.ReadFile(filepath.Join(br.Repo, t.Path))
	if err != nil || sandbox.Hash(now) != pristine {
		fail("ORIGINAL CHANGED during the run (sha256 differs from the start): another writer touched the file; every verdict is void")
	}
}

// sampleDiverse keeps up to n mutants, taking one per operator class in turn
// (in source order within a class), and returns the kept and the rest.
func sampleDiverse(ms []bashmut.Mutant, n int) (keep, rest []bashmut.Mutant) {
	if len(ms) <= n {
		return ms, nil
	}
	byOp := map[string][]int{}
	var ops []string
	for i, m := range ms {
		if _, ok := byOp[m.Operator]; !ok {
			ops = append(ops, m.Operator)
		}
		byOp[m.Operator] = append(byOp[m.Operator], i)
	}
	taken := map[int]bool{}
	for round := 0; len(taken) < n; round++ {
		progressed := false
		for _, op := range ops {
			if round < len(byOp[op]) && len(taken) < n {
				taken[byOp[op][round]] = true
				progressed = true
			}
		}
		if !progressed {
			break
		}
	}
	for i, m := range ms {
		if taken[i] {
			keep = append(keep, m)
		} else {
			rest = append(rest, m)
		}
	}
	return keep, rest
}

func status(r sandbox.Result) string {
	switch {
	case r.TimedOut:
		return "timeout"
	case r.Passed():
		return "pass"
	default:
		return "fail"
	}
}

func lastLines(s string, n int) string {
	ls := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, " | ")
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// lineSpecRE matches an explicit top-level line range target (L120-180).
var lineSpecRE = regexp.MustCompile(`^L([0-9]+)-([0-9]+)$`)
