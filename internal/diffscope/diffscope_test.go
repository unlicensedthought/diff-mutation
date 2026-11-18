package diffscope

import "testing"

// Bugs named: a pure-deletion hunk (+c,0) dropped, so deleting a guard puts
// its function out of scope; a single-line hunk without ",n" parsed as zero
// lines; a deleted file reported as changed.
func TestParse(t *testing.T) {
	diff := `diff --git a/s.sh b/s.sh
--- a/s.sh
+++ b/s.sh
@@ -3,0 +4,2 @@ f() {
+  a
+  b
@@ -10 +12 @@
-x
+y
@@ -20,3 +21,0 @@
-gone1
-gone2
-gone3
diff --git a/old.sh b/old.sh
--- a/old.sh
+++ /dev/null
@@ -1,2 +0,0 @@
-a
-b
`
	ch, err := Parse(diff)
	if err != nil {
		t.Fatal(err)
	}
	got := ch["s.sh"]
	want := []Range{{4, 5}, {12, 12}, {21, 22}}
	if len(got) != len(want) {
		t.Fatalf("ranges %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("range %d = %v, want %v", i, got[i], want[i])
		}
	}
	if ch.Has("old.sh") {
		t.Fatalf("deleted file is in scope: %v", ch)
	}
	if !ch.AnyOverlap("s.sh", 20, 21) || ch.AnyOverlap("s.sh", 6, 11) {
		t.Fatalf("overlap wrong for %v", got)
	}
}

// Found by changeling's own Go engine (Gremlins, 2026-10-29): Contains had no
// test and Overlaps' `<=` boundary survived. Bugs named: a function whose LAST
// line is the only changed line falls out of diff scope (Overlaps a <= End
// written as a < End); a changed line equal to a range's first or last line is
// treated as outside it (Contains off by one), so its mutants are attributed to
// <outside-range>.
func TestRangeBoundaries(t *testing.T) {
	r := Range{Start: 10, End: 12}
	for line, want := range map[int]bool{9: false, 10: true, 11: true, 12: true, 13: false} {
		if r.Contains(line) != want {
			t.Errorf("Contains(%d) = %v, want %v", line, !want, want)
		}
	}
	cases := []struct {
		a, b int
		want bool
	}{
		{1, 9, false},   // ends just before
		{1, 10, true},   // a function whose last line is the range's first
		{12, 20, true},  // a function whose first line is the range's last
		{13, 20, false}, // starts just after
		{11, 11, true},  // inside
		{1, 30, true},   // encloses
	}
	for _, c := range cases {
		if got := r.Overlaps(c.a, c.b); got != c.want {
			t.Errorf("Overlaps(%d,%d) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
	if !(Changes{"f": {r}}).Has("f") || (Changes{}).Has("f") {
		t.Error("Has is wrong")
	}
}
