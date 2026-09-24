// Command syngo is the Syngo file synchronization CLI.
package main

import (
	"os"

	"github.com/wesbragagt/syngo/internal/cli"
)

func main() {
	os.Exit(cli.Execute(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
