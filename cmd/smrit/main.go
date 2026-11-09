// Command smrit is the Smrit command line.
package main

import (
	"os"

	"github.com/smrit-ai/cli/internal/cli"
)

// commands is the table later tasks add to.
var commands = cli.Commands(cli.DefaultServices())

func main() {
	os.Exit(cli.Main(commands, os.Args[1:]))
}
