#!/usr/bin/env bash
# Install, build and test. The last stdout line is a VERIFY verdict.
set -u
cd "$(dirname "$0")/.."

gates=0
fail() {
  echo "VERIFY {\"verdict\":\"fail\",\"failed\":\"$1\"}"
  exit 1
}
step() { # name cmd...
  local name="$1"; shift
  echo "== $name"
  if ! "$@"; then
    fail "$name"
  fi
  gates=$((gates + 1))
}

step install go mod download
step modules-verified go mod verify
step build go build ./...
step vet go vet ./...
step format test -z "$(gofmt -l . | tee /dev/stderr)"
# CHANGELING_LOAD1=0: the tests' own fixtures are tiny; a busy host must not
# make them refuse to start.
step test env CHANGELING_LOAD1=0 go test ./... -count=1
step selftest env CHANGELING_LOAD1=0 go run ./cmd/changeling selftest

echo "VERIFY {\"verdict\":\"pass\",\"gates\":$gates}"
