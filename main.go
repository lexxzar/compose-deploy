package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/lexxzar/compose-deploy/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		// A failed --wait health gate exits 2 so CI can tell "deployed but
		// unhealthy" apart from "pipeline step failed" (exit 1).
		var waitErr *cmd.WaitError
		if errors.As(err, &waitErr) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
