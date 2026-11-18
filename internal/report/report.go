// Package report defines the changeling report JSON, recomputes every score
// from the mutant list (never from a stored number), renders the markdown
// summary, and implements the landing gate.
package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Schema identifies the report format.
const Schema = "changeling.report/v1"

// Mutant statuses.
const (
	Killed     = "killed"
	Timeout    = "timeout"
	Survived   = "survived"
	NoCoverage = "no-coverage"
	Ignored    = "ignored"
	Invalid    = "invalid"
	NotRun     = "not-run"
	// NotSampled: deliberately left out by --sample; excluded from scores and
	// from the not-run count (it is a declared scope, not a failure to run).
	NotSampled = "not-sampled"
)

// Mutant is one evaluated mutant.
type Mutant struct {
	ID           string `json:"id"`
	Engine       string `json:"engine"`
	File         string `json:"file"`
	Function     string `json:"function"`
	Line         int    `json:"line"`
	Col          int    `json:"col"`
	Operator     string `json:"operator"`
	Original     string `json:"original"`
	Replacement  string `json:"replacement"`
	Diff         string `json:"diff,omitempty"`
	Status       string `json:"status"`
	KilledBy     string `json:"killedBy,omitempty"`
	DurationMs   int64  `json:"durationMs,omitempty"`
	IgnoreReason string `json:"ignoreReason,omitempty"`
	TestsRun     int    `json:"testsRun,omitempty"`
}

// OracleTest is one oracle test's pristine behaviour.
type OracleTest struct {
	Path         string   `json:"path"`
	Pristine     string   `json:"pristine"` // pass | fail | timeout
	Instrumented string   `json:"instrumented,omitempty"`
	DurationMs   int64    `json:"durationMs"`
	Covers       []string `json:"covers"`
	Note         string   `json:"note,omitempty"`
}

// Function is one function in scope.
type Function struct {
	Name           string   `json:"name"`
	StartLine      int      `json:"startLine,omitempty"`
	EndLine        int      `json:"endLine,omitempty"`
	Changed        bool     `json:"changed"`
	CoveringTests  []string `json:"coveringTests"`
	KillingTests   []string `json:"killingTests"`
	MutantsInScope int      `json:"mutantsInScope"`
}

// File is one source file in scope.
type File struct {
	Path        string       `json:"path"`
	Engine      string       `json:"engine"`
	PristineSha string       `json:"pristineSha256,omitempty"`
	Functions   []Function   `json:"functions"`
	Oracle      []OracleTest `json:"oracle,omitempty"`
	CouldNotRun []string     `json:"couldNotRun,omitempty"`
}

// Report is the whole run.
type Report struct {
	Schema      string   `json:"schema"`
	Tool        string   `json:"tool"`
	StartedAt   string   `json:"startedAt"`
	FinishedAt  string   `json:"finishedAt"`
	Repo        string   `json:"repo"`
	Base        string   `json:"base,omitempty"`
	Head        string   `json:"head,omitempty"`
	Environment string   `json:"environment"`
	Mode        string   `json:"mode"` // diff | explicit
	Verdict     string   `json:"verdict"`
	CouldNotRun []string `json:"couldNotRun"`
	Notes       []string `json:"notes,omitempty"`
	Files       []File   `json:"files"`
	Mutants     []Mutant `json:"mutants"`
}

// Counts are recomputed from the mutant list.
type Counts struct {
	Total      int      `json:"total"`
	Killed     int      `json:"killed"`
	Timeout    int      `json:"timeout"`
	Survived   int      `json:"survived"`
	NoCoverage int      `json:"noCoverage"`
	Ignored    int      `json:"ignored"`
	Invalid    int      `json:"invalid"`
	NotRun     int      `json:"notRun"`
	NotSampled int      `json:"notSampled"`
	Score      *float64 `json:"score"` // (killed+timeout)/(killed+timeout+survived+noCoverage); null when that is 0
}

// Tally recomputes counts for the mutants keep() selects.
func Tally(ms []Mutant, keep func(Mutant) bool) Counts {
	var c Counts
	for _, m := range ms {
		if keep != nil && !keep(m) {
			continue
		}
		c.Total++
		switch m.Status {
		case Killed:
			c.Killed++
		case Timeout:
			c.Timeout++
		case Survived:
			c.Survived++
		case NoCoverage:
			c.NoCoverage++
		case Ignored:
			c.Ignored++
		case Invalid:
			c.Invalid++
		case NotSampled:
			c.NotSampled++
		default:
			c.NotRun++
		}
	}
	den := c.Killed + c.Timeout + c.Survived + c.NoCoverage
	if den > 0 {
		s := float64(int(10000*float64(c.Killed+c.Timeout)/float64(den)+0.5)) / 100
		c.Score = &s
	}
	return c
}

// MaxTimeoutRatio: above this share of evaluated mutants timing out, the run
// measured host contention, not the code, and is COULD-NOT-RUN.
const MaxTimeoutRatio = 0.10

// TimeoutContended reports whether timeouts exceed MaxTimeoutRatio of the
// mutants that were actually executed.
func TimeoutContended(c Counts) bool {
	ran := c.Killed + c.Timeout + c.Survived
	return ran > 0 && float64(c.Timeout) > MaxTimeoutRatio*float64(ran)
}

// DeriveVerdict derives the run verdict from the report contents.
func (r *Report) DeriveVerdict() string {
	c := Tally(r.Mutants, nil)
	switch {
	case len(r.CouldNotRun) > 0 || c.NotRun > 0 || TimeoutContended(c):
		return "could-not-run"
	case c.Survived > 0 || c.NoCoverage > 0:
		return "survivors"
	default:
		return "clean"
	}
}

// ExitCode maps a verdict: 0 clean, 1 survivors, 2 could-not-run.
func ExitCode(verdict string) int {
	switch verdict {
	case "clean":
		return 0
	case "survivors":
		return 1
	default:
		return 2
	}
}

// Write stores the report atomically (temp + rename) so a crashed run can
// never leave a half-written file that parses.
func (r *Report) Write(path string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Read loads and schema-checks a report.
func Read(path string) (*Report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if r.Schema != Schema {
		return nil, fmt.Errorf("%s: schema %q, want %q", path, r.Schema, Schema)
	}
	if _, err := time.Parse(time.RFC3339, r.StartedAt); err != nil {
		return nil, fmt.Errorf("%s: startedAt %q unparseable", path, r.StartedAt)
	}
	return &r, nil
}

// GateResult is the landing gate outcome.
type GateResult struct {
	Exit    int      `json:"exit"` // 0 pass, 1 fail, 2 could-not-run
	Reasons []string `json:"reasons"`
}

// Gate fails ONLY on could-not-run or on a CHANGED function that has mutants
// and zero killing tests (zero-coverage fail-closed). It never fails on a
// score: survivors are review evidence, not a verdict.
func Gate(r *Report, notBefore time.Time) GateResult {
	var g GateResult
	started, _ := time.Parse(time.RFC3339, r.StartedAt)
	if !notBefore.IsZero() && started.Before(notBefore.Add(-time.Second)) {
		g.Exit = 2
		g.Reasons = append(g.Reasons, fmt.Sprintf("report started %s, before this gate's run began (%s): a stale report is not this run's result", r.StartedAt, notBefore.UTC().Format(time.RFC3339)))
		return g
	}
	if v := r.DeriveVerdict(); v == "could-not-run" {
		g.Exit = 2
		g.Reasons = append(g.Reasons, r.CouldNotRun...)
		c := Tally(r.Mutants, nil)
		if c.NotRun > 0 {
			g.Reasons = append(g.Reasons, fmt.Sprintf("%d mutants were not run", c.NotRun))
		}
		if TimeoutContended(c) {
			g.Reasons = append(g.Reasons, fmt.Sprintf("CONTENDED: %d of %d executed mutants timed out (> %.0f%%): the host was oversubscribed, so no kill or survival here is a verdict", c.Timeout, c.Killed+c.Timeout+c.Survived, 100*MaxTimeoutRatio))
		}
		return g
	}
	for _, f := range r.Files {
		for _, fn := range f.Functions {
			if !fn.Changed {
				continue
			}
			c := Tally(r.Mutants, func(m Mutant) bool { return m.File == f.Path && m.Function == fn.Name })
			live := c.Killed + c.Timeout + c.Survived + c.NoCoverage
			if live > 0 && c.Killed+c.Timeout == 0 {
				g.Exit = 1
				g.Reasons = append(g.Reasons, fmt.Sprintf("ZERO-COVERAGE: changed function %s in %s has %d mutants and no test kills any of them", fn.Name, f.Path, live))
			}
		}
	}
	return g
}

// Markdown renders the summary from the JSON alone.
func Markdown(r *Report) string {
	var b strings.Builder
	all := Tally(r.Mutants, nil)
	fmt.Fprintf(&b, "### changeling mutation evidence (%s)\n\n", r.DeriveVerdict())
	fmt.Fprintf(&b, "Review evidence, not a score gate. Environment: %s. Mode: %s.", r.Environment, r.Mode)
	if r.Base != "" {
		fmt.Fprintf(&b, " Base: `%s`.", short(r.Base))
	}
	b.WriteString("\n\n")
	if len(r.CouldNotRun) > 0 {
		b.WriteString("**COULD-NOT-RUN**\n")
		for _, c := range r.CouldNotRun {
			b.WriteString("- " + c + "\n")
		}
		b.WriteString("\n")
	}
	for _, n := range r.Notes {
		b.WriteString("- note: " + n + "\n")
	}
	if len(r.Notes) > 0 {
		b.WriteString("\n")
	}
	b.WriteString("| file | function | changed | mutants | killed | timeout | survived | no-coverage | ignored | invalid | not-run | score |\n|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, f := range r.Files {
		for _, fn := range f.Functions {
			c := Tally(r.Mutants, func(m Mutant) bool { return m.File == f.Path && m.Function == fn.Name })
			if c.Total == 0 && !fn.Changed {
				continue
			}
			fmt.Fprintf(&b, "| %s | %s | %v | %d | %d | %d | %d | %d | %d | %d | %d | %s |\n", f.Path, fn.Name, fn.Changed, c.Total, c.Killed, c.Timeout, c.Survived, c.NoCoverage, c.Ignored, c.Invalid, c.NotRun, score(c.Score))
		}
	}
	fmt.Fprintf(&b, "| **all** | | | %d | %d | %d | %d | %d | %d | %d | %d | %s |\n\n", all.Total, all.Killed, all.Timeout, all.Survived, all.NoCoverage, all.Ignored, all.Invalid, all.NotRun, score(all.Score))
	surv := []Mutant{}
	for _, m := range r.Mutants {
		if m.Status == Survived || m.Status == NoCoverage {
			surv = append(surv, m)
		}
	}
	sort.SliceStable(surv, func(i, j int) bool {
		if surv[i].File != surv[j].File {
			return surv[i].File < surv[j].File
		}
		return surv[i].Line < surv[j].Line
	})
	if len(surv) > 0 {
		b.WriteString("#### Survivors\n\n")
		for _, m := range surv {
			fmt.Fprintf(&b, "- `%s:%d` %s in `%s` (%s, id `%s`)\n", m.File, m.Line, m.Operator, m.Function, m.Status, m.ID)
			if m.Diff != "" {
				b.WriteString("  ```diff\n")
				for _, l := range strings.Split(m.Diff, "\n") {
					b.WriteString("  " + l + "\n")
				}
				b.WriteString("  ```\n")
			}
		}
	}
	for _, f := range r.Files {
		for _, o := range f.Oracle {
			if o.Note != "" {
				fmt.Fprintf(&b, "- oracle note `%s` for %s: %s\n", o.Path, f.Path, o.Note)
			}
		}
	}
	return b.String()
}

func score(s *float64) string {
	if s == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.2f%%", *s)
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
