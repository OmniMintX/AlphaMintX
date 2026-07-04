// Command paperloop replays an agent-plane proposals.jsonl through the Risk
// Gate and paper OMS under the deterministic e2e runspec clock, writing
// byte-deterministic records.jsonl. Exit code 0 only when every runspec
// scenario produced its expected outcome.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/e2e"
)

func main() {
	runspecPath := flag.String("runspec", "e2e/runspec.json", "path to the e2e runspec")
	proposalsPath := flag.String("proposals", "out/proposals.jsonl", "path to the emitted proposal envelopes")
	outPath := flag.String("out", "out/records.jsonl", "path for the records output")
	flag.Parse()

	if err := run(*runspecPath, *proposalsPath, *outPath); err != nil {
		fmt.Fprintf(os.Stderr, "paperloop: %v\n", err)
		os.Exit(1)
	}
}

func run(runspecPath, proposalsPath, outPath string) error {
	spec, err := e2e.LoadRunSpec(runspecPath)
	if err != nil {
		return err
	}
	in, err := os.Open(proposalsPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(out)

	outcomes, err := e2e.Run(spec, in, w)
	if err != nil {
		out.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}

	parts := make([]string, 0, len(outcomes))
	ok := true
	for _, o := range outcomes {
		mark := "ok"
		if !o.OK() {
			mark = fmt.Sprintf("FAIL(want %s)", o.Expected)
			ok = false
		}
		parts = append(parts, fmt.Sprintf("%s=%s %s", o.Scenario, o.Got, mark))
	}
	fmt.Fprintf(os.Stderr, "paperloop: %s\n", strings.Join(parts, " | "))
	if !ok {
		return fmt.Errorf("one or more scenarios missed their expected outcome")
	}
	return nil
}
