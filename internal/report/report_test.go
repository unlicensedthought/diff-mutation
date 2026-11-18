package report

import (
	"testing"
	"time"
)

func rep(ms ...Mutant) *Report {
	return &Report{
		Schema: Schema, StartedAt: time.Now().UTC().Format(time.RFC3339),
		Files:   []File{{Path: "a.sh", Functions: []Function{{Name: "f", Changed: true}, {Name: "g", Changed: false}}}},
		Mutants: ms,
	}
}

func m(fn, st string) Mutant { return Mutant{File: "a.sh", Function: fn, Status: st} }

// Bug named: a score taken from a stored number instead of the mutant list.
func TestTallyRecomputes(t *testing.T) {
	c := Tally([]Mutant{m("f", Killed), m("f", Timeout), m("f", Survived), m("f", NoCoverage), m("f", Ignored), m("f", Invalid)}, nil)
	if c.Total != 6 || c.Killed != 1 || c.Timeout != 1 || c.Survived != 1 || c.NoCoverage != 1 || c.Ignored != 1 || c.Invalid != 1 {
		t.Fatalf("counts %+v", c)
	}
	if c.Score == nil || *c.Score != 50 {
		t.Fatalf("score %v, want 50 (ignored/invalid excluded from the denominator)", c.Score)
	}
	if Tally(nil, nil).Score != nil {
		t.Fatal("empty tally must have a null score, not 0 or 100")
	}
}

// Bugs named: the gate failing on survivors (a score gate in disguise); the
// gate passing a changed function nothing kills; could-not-run read as pass;
// a stale report accepted; a contended run accepted.
func TestGate(t *testing.T) {
	cases := []struct {
		name string
		r    *Report
		want int
	}{
		{"survivors alone pass", rep(m("f", Killed), m("f", Survived), m("g", Survived)), 0},
		{"changed function with zero kills fails", rep(m("f", Survived), m("f", NoCoverage), m("g", Killed)), 1},
		{"unchanged function with zero kills passes", rep(m("f", Killed), m("g", Survived)), 0},
		{"changed function with only ignored mutants passes", rep(m("f", Ignored), m("g", Killed)), 0},
		{"timeout counts as a kill", rep(m("f", Timeout), m("f", Killed), m("f", Killed), m("f", Killed), m("f", Killed), m("f", Killed), m("f", Killed), m("f", Killed), m("f", Killed), m("f", Killed), m("f", Killed)), 0},
		{"not-run is could-not-run", rep(m("f", Killed), m("f", NotRun)), 2},
	}
	for _, c := range cases {
		if g := Gate(c.r, time.Time{}); g.Exit != c.want {
			t.Errorf("%s: gate %d, want %d (%v)", c.name, g.Exit, c.want, g.Reasons)
		}
	}
	cnr := rep(m("f", Killed))
	cnr.CouldNotRun = []string{"NO ORACLE"}
	if g := Gate(cnr, time.Time{}); g.Exit != 2 {
		t.Errorf("could-not-run gate %d", g.Exit)
	}
	stale := rep(m("f", Killed))
	stale.StartedAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if g := Gate(stale, time.Now()); g.Exit != 2 {
		t.Errorf("stale report gate %d, want 2", g.Exit)
	}
	// 2 timeouts of 10 executed = 20% > 10%.
	cont := rep(m("f", Timeout), m("f", Timeout), m("f", Killed), m("f", Killed), m("f", Killed), m("f", Killed), m("f", Killed), m("f", Killed), m("f", Killed), m("f", Killed))
	if g := Gate(cont, time.Time{}); g.Exit != 2 {
		t.Errorf("contended run gate %d, want 2", g.Exit)
	}
	// Exactly 10% is not contended: 1 of 10.
	edge := rep(m("f", Timeout), m("f", Killed), m("f", Killed), m("f", Killed), m("f", Killed), m("f", Killed), m("f", Killed), m("f", Killed), m("f", Killed), m("f", Killed))
	if g := Gate(edge, time.Time{}); g.Exit != 0 {
		t.Errorf("10%% timeouts gate %d, want 0", g.Exit)
	}
}

// Bug named: the exit code drifting from the verdict.
func TestExitCodes(t *testing.T) {
	for v, want := range map[string]int{"clean": 0, "survivors": 1, "could-not-run": 2, "": 2, "garbage": 2} {
		if got := ExitCode(v); got != want {
			t.Errorf("ExitCode(%q) = %d, want %d", v, got, want)
		}
	}
}
