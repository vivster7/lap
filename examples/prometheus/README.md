# dev for Prometheus

An inner-loop command for [prometheus/prometheus](https://github.com/prometheus/prometheus)
(Go, ~1.7k files). Its full `go test ./...` takes minutes even with `-short`,
so within 60s `dev` runs:

| task | phase | variants (broadest first) |
|---|---|---|
| `gofmt` | prepare (writer) | changed `.go` files |
| `go-build` | check | `./...` |
| `golangci-lint` | check | `./...`, then changed packages |
| `go-test -short` | check | `./...` (estimate: 5m), then changed packages |

Plain `go vet` is deliberately absent: Prometheus runs govet through
golangci-lint with its own analyzer settings, and plain vet reports findings
on `main`.

## Setup

```sh
cp mise.toml /path/to/prometheus/   # or merge it
cd /path/to/prometheus
mise install && mise run setup      # warms the Go build/test cache
mise run dev
```

Warming the cache matters: on a cold cache the first `go build ./...` alone
took the whole 60s budget and was killed at the cutoff (recorded as a lower
bound; lap retries such variants after a few deferrals).

## A real run

A change adding a badly formatted, unused function to
`model/labels/labels_common.go`, warm caches, 12-core laptop:

```
dev · budget 1m0s · fix mode · 9 cpu · 1 changed files vs origin/main (merge-base 961c9ba409)
plan: 3 to run, 1 deferred, 0 skipped (dev plan for details)
  ▸ gofmt [default · 1 file · 1 cpu]
  ✎ gofmt 0s fixed 1 file(s)
  – go-build deferred all needs ~57s (timed out after 57.031s (lower bound)), 57s left
  ▸ go-test [changed · 3 cpu]
  ▸ golangci-lint [all · 3 cpu]
  ✓ go-test 600ms narrowed: broader variant did not fit
  ✗ golangci-lint 35.7s 1 finding

── golangci-lint (findings, exit 1) ──
model/labels/labels_common.go:247:6: func lapDemoHelper is unused (unused)
func lapDemoHelper(a int) int {
     ^

dev in 35.8s: 1 passed, 1 fixed, 1 findings, 1 deferred
not verified locally: go-build, go-test (narrowed to changed)
```

`gofmt` rewrote the function, `golangci-lint` found it unused, the full test
suite was not attempted (its estimate exceeds the budget) and the tests of the
changed package ran instead — and the summary says both of those things.
