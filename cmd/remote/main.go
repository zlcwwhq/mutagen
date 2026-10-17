// Command remote runs a command on the opposite endpoint of the Mutagen
// synchronization session whose root contains the current directory. It is
// installed alongside the Mutagen CLI on the daemon's system, while agents act
// as the remote executable on endpoint systems. Commands are routed through the
// daemon.
package main

import (
	"os"

	"github.com/mutagen-io/mutagen/cmd/remote/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
