// Command dev is lap's own inner-loop command: gofmt, go vet and the tests
// of what changed, within 60 seconds. Run it with `mise run dev`.
package main

import (
	"time"

	"github.com/vivster7/lap"
	"github.com/vivster7/lap/adapters"
	"github.com/vivster7/lap/cli"
)

func main() {
	test := adapters.GoTest("-race")
	test.Variants[0].Estimate = 45 * time.Second // the whole suite, with -race

	cli.Main(lap.Config{
		Name:  "dev",
		Tasks: []lap.Task{adapters.Gofmt(), adapters.GoVet(), test},
	})
}
