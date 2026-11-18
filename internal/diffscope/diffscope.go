// Package diffscope computes the new-side line ranges a change touched.
package diffscope

import (
	"bufio"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/unlicensedthought/diff-mutation/internal/gitx"
)

// Range is an inclusive 1-based line range on the new side of a diff.
type Range struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// Contains reports whether line falls inside r.
func (r Range) Contains(line int) bool { return line >= r.Start && line <= r.End }

// Overlaps reports whether [a,b] intersects r.
func (r Range) Overlaps(a, b int) bool { return a <= r.End && b >= r.Start }

// Changes maps a repo-relative path to the ranges changed in it. A path whose
// value is nil was changed by deletion only (lines removed, none added): its
// functions around the deletion point are still in scope, see Parse.
type Changes map[string][]Range

// AnyOverlap reports whether [a,b] intersects any range for path.
func (c Changes) AnyOverlap(path string, a, b int) bool {
	for _, r := range c[path] {
		if r.Overlaps(a, b) {
			return true
		}
	}
	return false
}

// Has reports whether path appears in the change set.
func (c Changes) Has(path string) bool { _, ok := c[path]; return ok }

var hunkRE = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// Compute diffs the merge base of base and HEAD against the WORKING TREE, so
// uncommitted edits in a checkout are in scope exactly as a landing candidate's
// commits are.
func Compute(repo, base string) (Changes, error) {
	mb, err := gitx.Run(repo, "merge-base", base, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("merge-base %s HEAD: %w", base, err)
	}
	out, err := gitx.Run(repo, "diff", "--no-color", "--no-ext-diff", "-U0", "--no-renames", strings.TrimSpace(mb))
	if err != nil {
		return nil, err
	}
	return Parse(out)
}

// Parse reads `git diff -U0` output. A pure-deletion hunk (+c,0) is recorded as
// the single line c..c+1 so the function enclosing the deletion stays in scope:
// deleting a guard is a change to the function that held it.
func Parse(diff string) (Changes, error) {
	ch := Changes{}
	cur := ""
	sc := bufio.NewScanner(strings.NewReader(diff))
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "+++ "):
			p := strings.TrimPrefix(line, "+++ ")
			if p == "/dev/null" {
				cur = ""
				continue
			}
			cur = strings.TrimPrefix(p, "b/")
			if _, ok := ch[cur]; !ok {
				ch[cur] = nil
			}
		case strings.HasPrefix(line, "@@ "):
			if cur == "" {
				continue
			}
			m := hunkRE.FindStringSubmatch(line)
			if m == nil {
				return nil, fmt.Errorf("unparseable hunk header %q", line)
			}
			start, _ := strconv.Atoi(m[1])
			n := 1
			if m[2] != "" {
				n, _ = strconv.Atoi(m[2])
			}
			if n == 0 {
				ch[cur] = append(ch[cur], Range{Start: start, End: start + 1})
				continue
			}
			ch[cur] = append(ch[cur], Range{Start: start, End: start + n - 1})
		}
	}
	return ch, sc.Err()
}
