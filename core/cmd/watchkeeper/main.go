// Command watchkeeper is the Watchkeeper core entrypoint (placeholder).
//
// Real wiring lands as milestones M1–M10 ship per docs/ROADMAP-phase1.md.
package main

import (
	"io"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout))
}

func run(_ []string, out io.Writer) int {
	if _, err := io.WriteString(out, "watchkeeper-core: placeholder — see docs/ROADMAP-phase1.md\n"); err != nil {
		return 1
	}
	return 0
}
