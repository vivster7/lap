// Command dev is an example inner-loop command for the Prometheus repository
// (github.com/prometheus/prometheus), built with lap.
//
// Prometheus is a large Go codebase whose full `go test ./...` takes minutes:
// far more than a 60s local budget. dev formats changed files, lints the
// packages you touched, builds everything, and runs the tests of changed
// packages when the full suite does not fit, saying so in the report.
//
// Usage, from a Prometheus checkout with the tools from mise.toml installed:
//
//	go run github.com/vivster7/lap/examples/prometheus@latest [group|task ...]
package main

import (
	"time"

	"github.com/vivster7/lap"
	"github.com/vivster7/lap/adapters"
	"github.com/vivster7/lap/cli"
)

func main() {
	gofmt := adapters.Gofmt()

	// No plain `go vet`: Prometheus runs govet through golangci-lint with
	// its own analyzer settings, and plain vet reports findings on main.

	lint := adapters.GolangciLint()
	lint.Priority = -1 // useful but slow; schedule after build and vet

	build := adapters.GoBuild()
	build.Priority = 1 // compile errors are the most valuable early signal

	test := adapters.GoTest("-short")
	// The whole suite takes minutes even with -short (TSDB, PromQL). Saying
	// so up front lets the first run pick the "changed" variant instead of
	// learning it from a timeout; history refines it afterwards.
	test.Variants[0].Estimate = 5 * time.Minute

	cli.Main(lap.Config{
		Name:   "dev",
		Budget: 60 * time.Second,
		Tasks:  []lap.Task{gofmt, build, lint, test},
	})
}
