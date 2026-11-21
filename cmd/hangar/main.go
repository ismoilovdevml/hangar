// Command hangar is the Hangar package registry server.
//
//	hangar serve   [--config hangar.yaml] [--migrate]
//	hangar migrate [--config hangar.yaml] up|down|status|version
//	hangar admin   bootstrap | reset-password | token create ...
//	hangar apply   -f FILE|DIR --server URL [--prune --apply-set NAME] [--dry-run]
//	hangar diff    -f FILE|DIR --server URL
//	hangar export  --server URL
//	hangar version [--json]
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ismoilovdevml/hangar/internal/version"
)

const usage = `Hangar — self-hosted package registry

Usage:
  hangar serve   [--config FILE] [--migrate]       run the server
  hangar migrate [--config FILE] up|down|status|version
                                                    manage the database schema
  hangar admin   bootstrap --username NAME --password-stdin
                                                    create the first administrator
  hangar admin   reset-password --user NAME        print a one-time password for a local user
  hangar admin   token create --user NAME --name LABEL --scopes LIST [--expires 90d]
                                                    create a personal access token
  hangar admin   rotate-secrets [--dry-run] [--downgrade]
                                                    re-encrypt stored secrets with security.secret_key
                                                    (--downgrade: hgs1 format, before going back to v0.1.x)
  hangar apply   -f FILE|DIR [--dry-run] [--prune --apply-set NAME] --server URL
                                                    make a server match resource documents
  hangar diff    -f FILE|DIR --server URL           print what apply would change
                                                    (exit 0 no changes, 2 changes, 1 error)
  hangar export  [--kind LIST] --server URL        write the server's configuration as documents
  hangar version [--json]                           print build information

Configuration is read from --config (or $HANGAR_CONFIG) and HANGAR_* environment
variables; see hangar.example.yaml. apply, diff and export talk to a running
server over the REST API and read the token from $HANGAR_TOKEN or --token-file;
see docs/guides/config-as-code.md.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "serve":
		err = serveCmd(args[1:], stderr)
	case "admin":
		err = adminCmd(args[1:], stdin, stdout, stderr)
	case "migrate":
		err = migrateCmd(args[1:], stdout, stderr)
	case "apply", "diff", "export":
		err = cacCommand(context.Background(), args[0], args[1:], stdin, stdout, stderr)
	case "version", "--version", "-v":
		err = versionCmd(args[1:], stdout)
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if code, ok := errors.AsType[exitCode](err); ok {
		return int(code)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hangar %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

func versionCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	info := version.Get()
	if *asJSON {
		return json.NewEncoder(stdout).Encode(info)
	}
	_, err := fmt.Fprintf(stdout, "hangar %s (commit %s, built %s, %s)\n", info.Version, orUnknown(info.Commit), orUnknown(info.Date), info.GoVersion)
	return err
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
