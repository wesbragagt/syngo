// Command syncthis is the Syncthis file synchronization CLI.
package main

import (
	"os"

	"github.com/wesbragagt/syncthis/internal/cli"
)

func main() {
	os.Exit(cli.Execute(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
