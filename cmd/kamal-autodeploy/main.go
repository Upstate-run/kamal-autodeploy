// Command kamal-autodeploy generates and runs merge-to-main deploys for Kamal projects.
// It is an independent open-source project, not affiliated with 37signals or Kamal.
package main

import (
	"os"

	"github.com/Upstate-run/kamal-autodeploy/internal/cli"
)

func main() { os.Exit(cli.Main(os.Args[1:])) }
