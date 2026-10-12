// Command cpd is the Comfy Portal cloud server: it runs on a rented GPU,
// installs the launch's models and extensions, keeps ComfyUI alive, and
// reports on all of it over an HTTP API.
//
//	cpd serve            run the supervisor (default)
//	cpd status [--json]  print the running supervisor's state
//	cpd report           print the launch's per-phase timings and download rates
//	cpd logs <stream>    print a log tail
//	cpd health           exit 0 if the supervisor answers (for HEALTHCHECK)
//	cpd version
package main

import (
	"fmt"
	"os"
)

// Set at build time: -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "status":
		err = status(args)
	case "report":
		err = report()
	case "logs":
		err = logsCmd(args)
	case "health":
		err = health()
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cpd:", err)
		os.Exit(1)
	}
}

const usage = `usage: cpd [serve | status [--json] | report | logs <stream> [-n N] | health | version]
`
