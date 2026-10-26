package main

import (
	"fmt"
	"os"

	"crease/internal/cli"
)

var version = "0.1.0-dev"

func main() {
	if err := cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, version); err != nil {
		if message := cli.FormatError(err); message != "" {
			fmt.Fprintln(os.Stderr, message)
		}
		os.Exit(1)
	}
}
