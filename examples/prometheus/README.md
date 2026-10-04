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
plan: 4 to run, 0 deferred, 0 skipped (dev plan for details)
  ▸ gofmt [default · 1 file · 1 cpu]
  ✎ gofmt 0s fixed 1 file(s)
  ▸ go-build [all · 3 cpu]
  ▸ go-test [changed · 3 cpu]
  ▸ golangci-lint [all · 3 cpu]
  ✓ go-test 100ms narrowed: broader variant did not fit
  ✗ golangci-lint 1.8s 1 finding
  ✓ go-build 4.4s

── golangci-lint (findings, exit 1) ──
model/labels/labels_common.go:247:6: func lapDemoHelper is unused (unused)
func lapDemoHelper(a int) int {
     ^

dev in 4.4s: 2 passed, 1 fixed, 1 findings
not verified locally: go-test (narrowed to changed)
run 20261004T061913Z-661fc7 · dev logs <task> · dev why
```

`gofmt` rewrote the function, `golangci-lint` found it unused, the build
passed, and the full test suite was not attempted (its estimate exceeds the
budget): the tests of the changed package ran instead, and the summary says so.

## How it got there

The very first run had a cold Go cache: `go build ./...` used the whole budget
and was killed at the cutoff. lap recorded that as a *lower bound* (57s), so
the next runs deferred `go-build` up front instead of wasting the budget:

```
  – go-build deferred all needs ~57s (timed out after 57.031s (lower bound)), 57s left
```

After three such deferrals lap retried it (caches may have warmed), it
finished in 4.5s, and that completion superseded the old timeout. `dev plan`
now shows what it expects:

```
  run      gofmt                    default              ~0s (p90 of 7 samples (+10%))
  run      go-build                 all                  ~5.6s (max of 2 sample(s) (x1.25); workers relaxed)
  run      golangci-lint            all                  ~39.3s (p90 of 6 samples (+10%); workers relaxed)
  run      go-test                  changed (narrowed)   ~700ms (p90 of 6 samples (+10%); workers relaxed)
```
