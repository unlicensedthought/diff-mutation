package bashmut

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const subject = `#!/usr/bin/env bash
set -u
gate() {
  local n="$1"
  if [ "$n" -eq 0 ]; then
    return 1
  fi
  [[ -z "$2" ]] && return 2
  (( n > 3 )) || exit 4
  while read -r x; do
    [ "$x" = stop ] && break
    continue
  done
  echo hi 2>&1 >/dev/null
  sleep 5
  cat <<EOF
42
EOF
  return 0
}
top=7
`

func mutantsOf(t *testing.T, src string, sel Selector) (*Script, []Mutant) {
	t.Helper()
	s, err := Parse("s.sh", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return s, s.Mutants(sel, Options{NumericSkipCommands: []string{"sleep"}})
}

func find(ms []Mutant, op, orig, repl string) *Mutant {
	for i := range ms {
		if ms[i].Operator == op && ms[i].Original == orig && ms[i].Replacement == repl {
			return &ms[i]
		}
	}
	return nil
}

// Bugs named: an operator class silently produces no mutants (the run then
// "passes" code it never touched); a mutant that is not valid bash gets run and
// counted as killed; a splice that disturbs bytes outside the site.
func TestOperators(t *testing.T) {
	s, ms := mutantsOf(t, subject, Selector{All: true})
	for _, w := range []struct{ op, orig, repl string }{
		{"cmp-negate", "-eq", "-ne"},
		{"cmp-negate", "-z", "-n"},
		{"cmp-negate", ">", "<="},
		{"cmp-boundary", ">", ">="},
		{"cmp-negate", "=", "!="},
		{"logic-swap", "&&", "||"},
		{"logic-swap", "||", "&&"},
		{"guard-delete", "return 2", ":"},
		{"guard-delete", "exit 4", ":"},
		{"guard-delete", "return 1", ":"},
		{"return-flip", "1", "0"},
		{"return-flip", "0", "1"},
		{"exit-flip", "4", "0"},
		// `&& break`: guard-delete and loopctl-drop are the same byte change;
		// the dedupe keeps one mutant, not two identical runs.
		{"guard-delete", "break", ":"},
		{"loopctl-drop", "continue", ":"},
		{"num-literal", "3", "0"},
		{"num-literal", "7", "0"},
		{"body-empty", "", "{ :; }"},
	} {
		var m *Mutant
		if w.op == "body-empty" {
			for i := range ms {
				if ms[i].Operator == "body-empty" {
					m = &ms[i]
				}
			}
		} else {
			m = find(ms, w.op, w.orig, w.repl)
		}
		if m == nil {
			t.Errorf("no %s mutant %q -> %q", w.op, w.orig, w.repl)
			continue
		}
		out := s.Apply(*m)
		if !Valid("s.sh", out) {
			t.Errorf("%s %q -> %q is not valid bash", w.op, w.orig, w.repl)
		}
		// Bytes outside the site are untouched.
		if !bytes.HasPrefix(out, []byte(subject[:m.start])) || !bytes.HasSuffix(out, []byte(subject[m.end:])) {
			t.Errorf("%s splice disturbed bytes outside [%d,%d)", w.op, m.start, m.end)
		}
	}
	cn := 0
	for _, m := range ms {
		if m.Operator == "cond-negate" {
			cn++
			if !Valid("s.sh", s.Apply(m)) {
				t.Errorf("cond-negate at line %d is invalid", m.Line)
			}
		}
	}
	if cn == 0 {
		t.Error("no cond-negate mutants")
	}
}

// Bugs named: fd numbers in redirects (2>&1) mutated into nonsense; sleep
// arguments mutated (timing, not a decision); heredoc bodies mutated.
func TestNumericExclusions(t *testing.T) {
	_, ms := mutantsOf(t, subject, Selector{All: true})
	for _, m := range ms {
		if m.Operator != "num-literal" {
			continue
		}
		switch m.Original {
		case "2", "1", "5", "42":
			if m.Line >= 14 && m.Line <= 18 {
				t.Errorf("num-literal mutated an excluded literal %q at line %d", m.Original, m.Line)
			}
		}
	}
}

// Bug named: mutant ids keyed on line numbers, so every equivalent-mutant
// entry goes stale the moment a line is inserted above it.
func TestIDsSurviveLineShift(t *testing.T) {
	_, a := mutantsOf(t, subject, Selector{All: true})
	shifted := strings.Replace(subject, "set -u\n", "set -u\n# a new comment\n\n", 1)
	_, b := mutantsOf(t, shifted, Selector{All: true})
	if len(a) != len(b) {
		t.Fatalf("mutant count changed with a comment insert: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			t.Fatalf("id of %s at line %d changed after a line shift: %s vs %s", a[i].Operator, a[i].Line, a[i].ID, b[i].ID)
		}
		if b[i].Line != a[i].Line+2 {
			t.Fatalf("line not shifted: %d vs %d", a[i].Line, b[i].Line)
		}
	}
}

// Bugs named: selecting one function leaks mutants from others; top-level
// code is attributed to a function.
func TestSelection(t *testing.T) {
	_, ms := mutantsOf(t, subject, Selector{Funcs: map[string]bool{TopLevel: true}})
	if len(ms) == 0 {
		t.Fatal("no top-level mutants")
	}
	for _, m := range ms {
		if m.Function != TopLevel {
			t.Fatalf("top-level selection leaked %s from %s", m.Operator, m.Function)
		}
	}
	_, byLine := mutantsOf(t, subject, Selector{Funcs: map[string]bool{}, LineRange: func(l int) bool { return l == 5 }})
	for _, m := range byLine {
		if m.Line != 5 {
			t.Fatalf("line selection leaked line %d", m.Line)
		}
	}
	if len(byLine) == 0 {
		t.Fatal("line selection found nothing on line 5")
	}
}

// Bug named: the <top> probe sat only at line 2, so a test that extracts a
// marked top-level block of a large script and EXECUTES just that block
// recorded no <top> coverage, and every <top> mutant was reported no-coverage
// without ever running.
func TestInstrumentTopLevelFragmentRecordsCoverage(t *testing.T) {
	src := "#!/usr/bin/env bash\nhelper() { echo h; }\nx=1\n# @begin\nif [ \"$x\" = 1 ]; then echo hit; fi\n# @end\n"
	s, err := Parse("frag.sh", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	trace := filepath.Join(dir, "trace")
	out, err := s.Instrument(trace)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(out), "\n")
	frag := strings.Join(lines[4:5], "\n") + "\n" // only the line between the markers
	cmd := exec.Command("bash", "-c", "x=1; "+frag)
	if b, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(b), "hit") {
		t.Fatalf("fragment did not run: %v\n%s\nfragment:\n%s", err, b, frag)
	}
	tr, _ := os.ReadFile(trace)
	if !strings.Contains(string(tr), TopLevel) {
		t.Fatalf("executing a top-level fragment recorded no %s coverage; trace=%q fragment=%q", TopLevel, tr, frag)
	}
	if strings.Contains(string(tr), "helper") {
		t.Fatalf("helper was recorded although the fragment never called it: %q", tr)
	}
}

// Bugs named: instrumentation moves line numbers (every reported line is then
// wrong) or breaks the script.
func TestInstrumentKeepsLines(t *testing.T) {
	s, _ := mutantsOf(t, subject, Selector{All: true})
	out, err := s.Instrument("/tmp/trace")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(out, []byte("\n")) != strings.Count(subject, "\n") {
		t.Fatal("instrumentation changed the line count")
	}
	if !bytes.Contains(out, []byte("'gate' >>'/tmp/trace'")) || !bytes.Contains(out, []byte("'<top>' >>'/tmp/trace'")) {
		t.Fatalf("probes missing:\n%s", out)
	}
	if _, err := s.Instrument("/tmp/it's"); err == nil {
		t.Fatal("a trace path with a quote was accepted")
	}
}
