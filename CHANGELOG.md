# Changelog

## 0.1.0 - 2026-11-18

- `changeling` mutates only the functions a diff touched and reports which mutants the tests failed to notice.
- Engines: bash (built in, using `mvdan.cc/sh/v3`), TypeScript (the repo's own Stryker) and Go (Gremlins, pinned to one version).
- A written reason is required for every mutant you decide to ignore.
- Exit codes: 0 clean, 1 survivors, 2 could not run.
- `changeling selftest` two-way self-check on an embedded fixture.
