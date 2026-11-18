// Package bashmut is the bash mutation engine: it parses a script with a real
// bash AST (mvdan.cc/sh/v3), enumerates mutation sites, and produces each
// mutant as a BYTE SPLICE of the original source.
//
// Why splice and not reprint: printing the AST back reformats the whole file,
// so every mutant would also differ from the original in whitespace, quoting
// and comment layout. A test that (wrongly) asserts over source text would then
// "kill" every mutant for a reason that has nothing to do with the mutation.
// A splice changes only the bytes of the site; everything else is identical,
// so a kill is attributable to the mutation alone. Every mutant is re-parsed
// and an unparseable one is recorded as invalid, never run.
package bashmut

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// TopLevel names the pseudo-function holding code outside every function.
const TopLevel = "<top>"

// Func is one function definition.
type Func struct {
	Name      string `json:"name"`
	StartLine int    `json:"startLine"`
	EndLine   int    `json:"endLine"`
	start     uint
	end       uint
	lbrace    uint // offset just after the body's opening brace/paren; 0 when not instrumentable
}

// Mutant is one single-site change.
type Mutant struct {
	ID          string `json:"id"`
	File        string `json:"file"`
	Function    string `json:"function"`
	Line        int    `json:"line"`
	Col         int    `json:"col"`
	Operator    string `json:"operator"`
	Original    string `json:"original"`
	Replacement string `json:"replacement"`
	start, end  uint
}

// Options tunes site enumeration.
type Options struct {
	// NumericSkipCommands: numeric arguments of these commands are not mutated
	// (sleep 5 -> sleep 0 changes timing, not a decision).
	NumericSkipCommands []string
}

// Script is a parsed file.
type Script struct {
	Path  string
	Src   []byte
	File  *syntax.File
	Funcs []Func

	lineStarts []uint
}

func parser() *syntax.Parser {
	return syntax.NewParser(syntax.KeepComments(true), syntax.Variant(syntax.LangBash))
}

// Parse parses src as bash.
func Parse(path string, src []byte) (*Script, error) {
	f, err := parser().Parse(bytes.NewReader(src), path)
	if err != nil {
		return nil, err
	}
	s := &Script{Path: path, Src: src, File: f}
	seen := map[string]int{}
	syntax.Walk(f, func(n syntax.Node) bool {
		fd, ok := n.(*syntax.FuncDecl)
		if !ok || fd.Name == nil {
			return true
		}
		name := fd.Name.Value
		seen[name]++
		if seen[name] > 1 {
			name = fmt.Sprintf("%s@%d", name, fd.Pos().Line())
		}
		fn := Func{Name: name, StartLine: int(fd.Pos().Line()), EndLine: int(fd.End().Line()), start: fd.Pos().Offset(), end: fd.End().Offset()}
		if fd.Body != nil {
			switch b := fd.Body.Cmd.(type) {
			case *syntax.Block:
				fn.lbrace = b.Lbrace.Offset() + 1
			case *syntax.Subshell:
				fn.lbrace = b.Lparen.Offset() + 1
			}
		}
		s.Funcs = append(s.Funcs, fn)
		return true
	})
	return s, nil
}

// FuncAt returns the innermost function containing offset, or TopLevel.
func (s *Script) FuncAt(off uint) string {
	best := TopLevel
	var bestLen uint = ^uint(0)
	for _, f := range s.Funcs {
		if off >= f.start && off < f.end && f.end-f.start < bestLen {
			best, bestLen = f.Name, f.end-f.start
		}
	}
	return best
}

// Selector decides which sites are in scope.
type Selector struct {
	All       bool
	Funcs     map[string]bool
	LineRange func(line int) bool // nil = none
}

// InScope reports whether a site at line inside function fn is selected.
func (sel Selector) InScope(fn string, line int) bool {
	if sel.All || sel.Funcs[fn] {
		return true
	}
	return sel.LineRange != nil && sel.LineRange(line)
}

// ChangedFuncs returns the functions whose span overlaps a changed range.
func (s *Script) ChangedFuncs(overlaps func(a, b int) bool) []string {
	var out []string
	for _, f := range s.Funcs {
		if overlaps(f.StartLine, f.EndLine) {
			out = append(out, f.Name)
		}
	}
	return out
}

var digitsRE = regexp.MustCompile(`^[0-9]+$`)

// cmpNegate maps a comparison operator to its negation; cmpBoundary to the
// off-by-one neighbour.
var cmpNegate = map[string]string{
	"-eq": "-ne", "-ne": "-eq", "-lt": "-ge", "-ge": "-lt", "-gt": "-le", "-le": "-gt",
	"=": "!=", "==": "!=", "!=": "==", "-z": "-n", "-n": "-z",
}
var cmpBoundary = map[string]string{"-lt": "-le", "-le": "-lt", "-gt": "-ge", "-ge": "-gt"}
var arithNegate = map[string]string{"<": ">=", ">=": "<", ">": "<=", "<=": ">", "==": "!=", "!=": "==", "&&": "||", "||": "&&"}
var arithBoundary = map[string]string{"<": "<=", "<=": "<", ">": ">=", ">=": ">"}

type rawSite struct {
	op, repl   string
	start, end uint
}

func litWord(w *syntax.Word) *syntax.Lit {
	if w == nil || len(w.Parts) != 1 {
		return nil
	}
	l, _ := w.Parts[0].(*syntax.Lit)
	return l
}

func callName(c *syntax.CallExpr) string {
	if c == nil || len(c.Args) == 0 {
		return ""
	}
	if l := litWord(c.Args[0]); l != nil {
		return l.Value
	}
	return ""
}

// Mutants enumerates every in-scope mutant of the script.
func (s *Script) Mutants(sel Selector, opt Options) []Mutant {
	skipNum := map[string]bool{}
	for _, c := range opt.NumericSkipCommands {
		skipNum[c] = true
	}
	var sites []rawSite
	add := func(op, repl string, start, end uint) {
		sites = append(sites, rawSite{op: op, repl: repl, start: start, end: end})
	}
	// Offsets of literal words that must not get a num-literal mutant: command
	// names, return/exit codes (their own operator), skipped commands' args.
	noNum := map[uint]bool{}
	var noNumRanges [][2]uint // redirects and heredocs
	syntax.Walk(s.File, func(n syntax.Node) bool {
		switch x := n.(type) {
		case *syntax.Redirect:
			noNumRanges = append(noNumRanges, [2]uint{x.Pos().Offset(), x.End().Offset()})
			if x.Hdoc != nil {
				noNumRanges = append(noNumRanges, [2]uint{x.Hdoc.Pos().Offset(), x.Hdoc.End().Offset()})
			}
		case *syntax.CallExpr:
			name := callName(x)
			if len(x.Args) > 0 {
				noNum[x.Args[0].Pos().Offset()] = true
			}
			if skipNum[name] || name == "return" || name == "exit" {
				for _, a := range x.Args {
					noNum[a.Pos().Offset()] = true
				}
			}
		}
		return true
	})
	inNoNum := func(off uint) bool {
		for _, r := range noNumRanges {
			if off >= r[0] && off < r[1] {
				return true
			}
		}
		return false
	}

	syntax.Walk(s.File, func(n syntax.Node) bool {
		switch x := n.(type) {
		case *syntax.FuncDecl:
			if x.Body != nil {
				switch b := x.Body.Cmd.(type) {
				case *syntax.Block:
					add("body-empty", "{ :; }", b.Lbrace.Offset(), b.Rbrace.Offset()+1)
				case *syntax.Subshell:
					add("body-empty", "( :; )", b.Lparen.Offset(), b.Rparen.Offset()+1)
				}
			}
		case *syntax.Stmt:
			if x.Negated {
				// `! cmd` -> `  cmd`: same width, negation removed.
				add("cond-negate", " ", x.Position.Offset(), x.Position.Offset()+1)
			}
		case *syntax.IfClause:
			s.condSites(x.Cond, add)
			s.guardSites(x.Then, add)
		case *syntax.WhileClause:
			s.condSites(x.Cond, add)
		case *syntax.BinaryCmd:
			switch x.Op {
			case syntax.AndStmt:
				add("logic-swap", "||", x.OpPos.Offset(), x.OpPos.Offset()+2)
				add("guard-delete", ":", x.Y.Pos().Offset(), x.Y.End().Offset())
			case syntax.OrStmt:
				add("logic-swap", "&&", x.OpPos.Offset(), x.OpPos.Offset()+2)
				add("guard-delete", ":", x.Y.Pos().Offset(), x.Y.End().Offset())
			}
		case *syntax.BinaryTest:
			op := x.Op.String()
			if r, ok := cmpNegate[op]; ok {
				add("cmp-negate", r, x.OpPos.Offset(), x.OpPos.Offset()+uint(len(op)))
			}
			if r, ok := cmpBoundary[op]; ok {
				add("cmp-boundary", r, x.OpPos.Offset(), x.OpPos.Offset()+uint(len(op)))
			}
			if op == "&&" || op == "||" {
				r := map[string]string{"&&": "||", "||": "&&"}[op]
				add("logic-swap", r, x.OpPos.Offset(), x.OpPos.Offset()+2)
			}
		case *syntax.UnaryTest:
			op := x.Op.String()
			if r, ok := cmpNegate[op]; ok {
				add("cmp-negate", r, x.OpPos.Offset(), x.OpPos.Offset()+uint(len(op)))
			}
			if op == "!" {
				add("cond-negate", " ", x.OpPos.Offset(), x.OpPos.Offset()+1)
			}
		case *syntax.BinaryArithm:
			op := x.Op.String()
			if r, ok := arithNegate[op]; ok {
				add("cmp-negate", r, x.OpPos.Offset(), x.OpPos.Offset()+uint(len(op)))
			}
			if r, ok := arithBoundary[op]; ok {
				add("cmp-boundary", r, x.OpPos.Offset(), x.OpPos.Offset()+uint(len(op)))
			}
		case *syntax.CallExpr:
			s.callSites(x, add)
		case *syntax.Word:
			l := litWord(x)
			if l == nil || !digitsRE.MatchString(l.Value) {
				return true
			}
			off := x.Pos().Offset()
			if noNum[off] || inNoNum(off) {
				return true
			}
			// The word must be exactly the literal in the source (not a fragment
			// of a larger token the parser split).
			if string(s.Src[off:x.End().Offset()]) != l.Value {
				return true
			}
			repl := "0"
			if l.Value == "0" {
				repl = "1"
			}
			add("num-literal", repl, off, x.End().Offset())
		}
		return true
	})

	sort.SliceStable(sites, func(i, j int) bool {
		if sites[i].start != sites[j].start {
			return sites[i].start < sites[j].start
		}
		return sites[i].op < sites[j].op
	})
	var out []Mutant
	ordinal := map[string]int{}
	dedupe := map[string]bool{}
	for _, st := range sites {
		dk := fmt.Sprintf("%d:%d:%s", st.start, st.end, st.repl)
		if dedupe[dk] {
			continue
		}
		dedupe[dk] = true
		fn := s.FuncAt(st.start)
		pos := s.position(st.start)
		if !sel.InScope(fn, pos.line) {
			continue
		}
		orig := string(s.Src[st.start:st.end])
		key := strings.Join([]string{s.Path, fn, st.op, orig, st.repl}, "\x00")
		ordinal[key]++
		h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", key, ordinal[key])))
		out = append(out, Mutant{
			ID: hex.EncodeToString(h[:6]), File: s.Path, Function: fn, Line: pos.line, Col: pos.col,
			Operator: st.op, Original: orig, Replacement: st.repl, start: st.start, end: st.end,
		})
	}
	return out
}

func (s *Script) condSites(cond []*syntax.Stmt, add func(op, repl string, start, end uint)) {
	if len(cond) == 0 {
		return
	}
	first, last := cond[0], cond[len(cond)-1]
	start, end := first.Pos().Offset(), last.End().Offset()
	// Wrap the whole condition: `! { <cond>\n}\n`. The trailing newline keeps
	// `then`/`do` a reserved word whatever separator the condition ended with.
	add("cond-negate", "! { "+string(s.Src[start:end])+"\n}\n", start, end)
}

func (s *Script) guardSites(then []*syntax.Stmt, add func(op, repl string, start, end uint)) {
	for _, st := range then {
		add("guard-delete", ":", st.Pos().Offset(), stmtEnd(st))
	}
}

// stmtEnd is the end of a statement without its trailing `;`/`&`.
func stmtEnd(st *syntax.Stmt) uint {
	if st.Cmd != nil {
		e := st.Cmd.End().Offset()
		for _, r := range st.Redirs {
			if r.End().Offset() > e {
				e = r.End().Offset()
			}
		}
		return e
	}
	return st.End().Offset()
}

func (s *Script) callSites(c *syntax.CallExpr, add func(op, repl string, start, end uint)) {
	name := callName(c)
	switch name {
	case "return", "exit":
		op := name + "-flip"
		if len(c.Args) == 1 {
			e := c.Args[0].End().Offset()
			add(op, name+" 1", c.Args[0].Pos().Offset(), e)
			return
		}
		a := c.Args[1]
		if l := litWord(a); l != nil && digitsRE.MatchString(l.Value) {
			r := "0"
			if l.Value == "0" {
				r = "1"
			}
			add(op, r, a.Pos().Offset(), a.End().Offset())
			return
		}
		add(op, "0", a.Pos().Offset(), a.End().Offset())
	case "continue", "break":
		add("loopctl-drop", ":", c.Pos().Offset(), c.End().Offset())
	case "[", "test":
		for _, a := range c.Args[1:] {
			l := litWord(a)
			if l == nil {
				continue
			}
			if r, ok := cmpNegate[l.Value]; ok {
				add("cmp-negate", r, a.Pos().Offset(), a.End().Offset())
			}
			if r, ok := cmpBoundary[l.Value]; ok {
				add("cmp-boundary", r, a.Pos().Offset(), a.End().Offset())
			}
		}
	}
}

type lc struct{ line, col int }

func (s *Script) position(off uint) lc {
	if s.lineStarts == nil {
		s.lineStarts = []uint{0}
		for i, b := range s.Src {
			if b == '\n' {
				s.lineStarts = append(s.lineStarts, uint(i+1))
			}
		}
	}
	i := sort.Search(len(s.lineStarts), func(i int) bool { return s.lineStarts[i] > off }) - 1
	return lc{i + 1, int(off-s.lineStarts[i]) + 1}
}

// Apply returns the source with m spliced in.
func (s *Script) Apply(m Mutant) []byte {
	out := make([]byte, 0, len(s.Src)+len(m.Replacement))
	out = append(out, s.Src[:m.start]...)
	out = append(out, m.Replacement...)
	return append(out, s.Src[m.end:]...)
}

// Valid reports whether src still parses as bash.
func Valid(path string, src []byte) bool {
	_, err := parser().Parse(bytes.NewReader(src), path)
	return err == nil
}

// Diff renders the mutated line(s) as a two-line -/+ diff.
func (s *Script) Diff(m Mutant) string {
	ls := lineStart(s.Src, m.start)
	le := lineEnd(s.Src, m.end)
	before := string(s.Src[ls:le])
	after := string(s.Src[ls:m.start]) + m.Replacement + string(s.Src[m.end:le])
	var b strings.Builder
	for _, l := range strings.Split(before, "\n") {
		b.WriteString("- " + l + "\n")
	}
	for _, l := range strings.Split(after, "\n") {
		b.WriteString("+ " + l + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func lineStart(src []byte, off uint) uint {
	for off > 0 && src[off-1] != '\n' {
		off--
	}
	return off
}

func lineEnd(src []byte, off uint) uint {
	for off < uint(len(src)) && src[off] != '\n' {
		off++
	}
	return off
}

// Instrument returns the source with a coverage probe at the head of every
// function body and of the top level. Each probe appends the function's name
// to trace. No newline is inserted, so every line number is unchanged.
func (s *Script) Instrument(trace string) ([]byte, error) {
	if strings.ContainsAny(trace, "'\n") {
		return nil, fmt.Errorf("trace path %q cannot be single-quoted", trace)
	}
	probe := func(name string) string {
		return fmt.Sprintf(" { printf '%%s\\n' '%s' >>'%s'; } 2>/dev/null || :; ", strings.ReplaceAll(name, "'", ""), trace)
	}
	type ins struct {
		at   uint
		text string
	}
	var all []ins
	for _, f := range s.Funcs {
		if f.lbrace > 0 {
			all = append(all, ins{f.lbrace, probe(f.Name)})
		}
	}
	// Top level: before EVERY top-level statement, not once at line 2. Tests
	// that extract a marked block of a large script and execute only that block
	// never run line 2; with a single probe there, every <top> mutant would read
	// as no-coverage and never be executed.
	for _, st := range s.File.Stmts {
		all = append(all, ins{st.Pos().Offset(), strings.TrimLeft(probe(TopLevel), " ")})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at < all[j].at })
	var out []byte
	prev := uint(0)
	for _, x := range all {
		out = append(out, s.Src[prev:x.at]...)
		out = append(out, x.text...)
		prev = x.at
	}
	out = append(out, s.Src[prev:]...)
	if !Valid(s.Path, out) {
		return nil, fmt.Errorf("instrumented %s does not parse", s.Path)
	}
	return out, nil
}
