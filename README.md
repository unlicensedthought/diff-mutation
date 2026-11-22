# diff-mutation

Mutation testing scoped to the functions a diff touched, with a written reason
required for every mutant you decide to ignore.

The command is called `changeling`. It mutates code, runs your tests against
each mutant, and reports which mutants your tests failed to notice. A surviving
mutant means a bug your tests would let through.

Write-up: [Mutation testing scoped to a diff, and why survivors need a written reason](https://unlicensedthought.com/writing/mutation-testing-scoped-to-a-diff/)

## The problem

A green test suite says the tests pass. It does not say they would fail if the
code were wrong. Mutation testing checks that: change the code in a small way
(flip `-lt` to `-ge`, swap `&&` for `||`, delete a guard) and see whether any
test goes red. If none does, the tests do not cover that behaviour.

Running that over a whole repository is slow. `changeling` runs it only on
the functions your diff changed.

## What it does

| engine | how | scope config |
|---|---|---|
| bash | built in. Parses the script with `mvdan.cc/sh/v3`, makes one byte-splice per mutant, re-parses it before running | `scopes.bash` |
| TypeScript | shells out to the repo's own Stryker, resolved by path (never `npx`) | `scopes.ts` (empty by default) |
| Go | shells out to Gremlins, pinned to one version | `scopes.go` (empty by default) |

Exit codes, never conflated:

- **0** clean: every mutant in scope was evaluated and none survived.
- **1** survivors: the run finished and at least one mutant survived.
- **2** could not run: no oracle, a red oracle, a missing tool, a cut budget,
  a busy host, a damaged sandbox. This is not a verdict about the code.

## Quick start

Needs Go 1.25+ and git. The bash engine needs bash.

```
go build -o changeling ./cmd/changeling
./changeling selftest          # two-way self-check on an embedded fixture
```

Mutate named functions of a script (explicit mode):

```
./changeling run --target scripts/deploy.sh:check_args,parse_flags
./changeling run --target scripts/deploy.sh:'*'      # every function
```

Mutate only what a branch changed (diff mode):

```
./changeling run --base origin/main
./changeling select --base origin/main               # 0 = something in scope, 1 = nothing
./changeling score .changeling/changeling-report.json
./changeling gate  .changeling/changeling-report.json --not-before 2026-01-01T00:00:00Z
```

Copy `changeling.json` to your repo and edit `scopes` and `oracle.testGlobs`.

## How it works

**Scope.** In diff mode `git diff -U0` against the merge base gives the changed
line ranges. A function is in scope when a changed range overlaps its span. A
pure deletion counts as a change to the function that held the deleted lines.

**Operators (bash).** `cmp-negate`, `cmp-boundary`, `cond-negate`, `logic-swap`,
`return-flip`, `exit-flip`, `loopctl-drop`, `body-empty`, `num-literal`,
`guard-delete`. Each mutant is a byte splice of the original, so a kill is
caused by the mutation and not by reformatted text.

**Oracle.** Tests matching `oracle.testGlobs` that name the file. A coverage
pass runs each test once against a copy with a probe at the head of every
function. A mutant then runs only the tests that executed its function. A test
that fails on the untouched file is excluded and named. No oracle, or no green
oracle, is exit 2.

**Sandbox.** Every mutant runs in a copy of the tracked tree, committed as a
throwaway git repo under the work directory. The original file is never opened
for writing, so a `kill -9` leaves a stale copy at worst; the next run reaps it.
Each test runs in its own process group with a timeout. Files a test writes are
undone, and the target is re-hashed before the next mutant. A copy that cannot
be restored voids the run. The original is re-hashed at the end too.

**Contention.** `run` refuses to start when the 1-minute load average exceeds
the core count, and a run where more than 10% of executed mutants timed out is
exit 2, because those timeouts measured the host.

**Equivalent mutants.** Some mutants change nothing a test could observe. List
them in `equivalents.json` with a reason. An entry without a reason makes the
file fail to load. Mutant ids hash (file, function, operator, original,
replacement, ordinal), not line numbers, so an entry survives edits above it.

```json
{
  "equivalents": [
    {
      "id": "0123456789ab",
      "file": "scripts/deploy.sh",
      "function": "check_args",
      "operator": "num-literal",
      "original": "1",
      "replacement": "0",
      "reason": "the value is only used as a loop start and the loop body is idempotent"
    }
  ]
}
```

## Limits

- The bash oracle is plain test scripts that exit non-zero on failure. Bats and
  other frameworks are not wired in unless you point `oracle.runner` at them.
- The TypeScript and Go engines delegate. They are only as good as Stryker and
  Gremlins, and the TypeScript path has no test that runs Stryker.
- Gremlins does not name the test that killed a mutant, so Go kills are
  recorded against the package suite.
- Equivalent-mutant judgement is manual. The tool makes you write the reason;
  it cannot tell if the reason is true.
- The gate fails only on could-not-run or on a changed function where no test
  kills any mutant. Survivors alone do not fail it. They are evidence for review.
- Linux and macOS only (process groups, `/proc/loadavg` or `sysctl`).

## Gremlins pin

`changeling.json` pins `github.com/go-gremlins/gremlins` at `v0.6.0`. The engine
reads the module path and version from the binary's embedded build info and
refuses any other:

```
GOBIN=$HOME/.cache/changeling/bin go install github.com/go-gremlins/gremlins/cmd/gremlins@v0.6.0
# or point CHANGELING_GREMLINS at the binary
```

## Tests

```
scripts/verify.sh        # download, verify modules, build, vet, gofmt, test, selftest
go test ./...            # just the tests
```

The last line of `scripts/verify.sh` is `VERIFY {"verdict":"pass","gates":N}` or
`VERIFY {"verdict":"fail","failed":"<step>"}`.

## License

Apache-2.0. See `LICENSE`.
