package main

import (
	"os"

	"github.com/bionicman/savetoa/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
