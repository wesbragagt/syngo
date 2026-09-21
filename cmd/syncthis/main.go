// Command syncthis is the Syncthis file synchronization CLI.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "syncthis:", err)
		os.Exit(1)
	}
}

func run(_ []string) error {
	return nil
}
