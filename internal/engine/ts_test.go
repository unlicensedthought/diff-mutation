package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func touch(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Bugs named: a TS file attributed to the wrong package (Stryker then runs the
// wrong test config); a file with no package.json silently given a surface.
func TestSurfaceOf(t *testing.T) {
	repo := t.TempDir()
	touch(t, filepath.Join(repo, "apps", "api", "package.json"))
	touch(t, filepath.Join(repo, "apps", "api", "src", "x.ts"))
	touch(t, filepath.Join(repo, "lonely", "y.ts"))
	if got := surfaceOf(repo, "apps/api/src/x.ts"); got != "apps/api" {
		t.Errorf("surface of apps/api/src/x.ts = %q, want apps/api", got)
	}
	if got := surfaceOf(repo, "lonely/y.ts"); got != "" {
		t.Errorf("surface of a file with no package.json above it = %q, want empty", got)
	}
	touch(t, filepath.Join(repo, "package.json"))
	if got := surfaceOf(repo, "lonely/y.ts"); got != "." {
		t.Errorf("surface with a root package.json = %q, want .", got)
	}
}

// Bug named: a hardcoded package manager that fails on every repo that uses
// another one.
func TestPackageManagerOf(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, "app")
	if got := packageManagerOf(repo, dir); got != "npm" {
		t.Errorf("no lockfile: %q, want npm", got)
	}
	touch(t, filepath.Join(repo, "yarn.lock"))
	if got := packageManagerOf(repo, dir); got != "yarn" {
		t.Errorf("root yarn.lock: %q, want yarn", got)
	}
	touch(t, filepath.Join(dir, "pnpm-lock.yaml"))
	if got := packageManagerOf(repo, dir); got != "pnpm" {
		t.Errorf("surface pnpm-lock.yaml: %q, want pnpm", got)
	}
}
