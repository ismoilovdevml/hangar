package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ismoilovdevml/hangar/internal/cac"
	"github.com/ismoilovdevml/hangar/internal/version"
)

// exitCode makes run exit with code without printing anything more: diff
// mode answers 2 when there are changes (like terraform plan
// -detailed-exitcode).
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// defaultRequestTimeout bounds each attempt of each API request; --timeout
// overrides it.
const defaultRequestTimeout = 30 * time.Second

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// remoteFlags are the flags of every command that talks to a server.
type remoteFlags struct {
	server    string
	tokenFile string
	timeout   time.Duration
}

func (r *remoteFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&r.server, "server", os.Getenv("HANGAR_URL"), "Hangar server URL (default $HANGAR_URL)")
	fs.StringVar(&r.tokenFile, "token-file", "", "file holding the API token (default: $HANGAR_TOKEN)")
	fs.DurationVar(&r.timeout, "timeout", defaultRequestTimeout, "time limit of each API request")
}

// client builds the API client. The token comes from --token-file or
// HANGAR_TOKEN and is never printed.
func (r *remoteFlags) client(stderr io.Writer) (*cac.Client, error) {
	if r.server == "" {
		return nil, errors.New("--server (or HANGAR_URL) is required")
	}
	token := os.Getenv("HANGAR_TOKEN")
	if r.tokenFile != "" {
		b, err := os.ReadFile(r.tokenFile) // #nosec G304 -- the operator names the token file
		if err != nil {
			return nil, fmt.Errorf("--token-file: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	c, err := cac.NewClient(cac.ClientOptions{Server: r.server, Token: token, Timeout: r.timeout,
		UserAgent: "hangar-cli/" + version.Get().Version})
	if err != nil {
		return nil, err
	}
	if u, _ := url.Parse(r.server); u != nil && u.Scheme == "http" && !loopback(u.Hostname()) {
		_, _ = fmt.Fprintf(stderr, "warning: %s is plain HTTP; the token travels unencrypted\n", u.Host)
	}
	return c, nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// planFlags are the flags apply and diff share.
type planFlags struct {
	remoteFlags
	files         stringList
	recursive     bool
	applySet      string
	applySetSet   bool
	prune         bool
	takeOwnership bool
	updateSecrets bool
	verbose       bool
}

func (p *planFlags) register(fs *flag.FlagSet) {
	p.remoteFlags.register(fs)
	fs.Var(&p.files, "f", "file or directory of resource documents, or - for stdin (repeatable)")
	fs.BoolVar(&p.recursive, "R", false, "also read the subdirectories of directories given with -f")
	fs.StringVar(&p.applySet, "apply-set", "default", "name of the configuration that owns the resources; --prune requires it")
	fs.BoolVar(&p.prune, "prune", false, "delete resources of the apply set that the files no longer declare")
	fs.BoolVar(&p.takeOwnership, "take-ownership", false, "adopt resources that another apply set owns")
	fs.BoolVar(&p.updateSecrets, "update-secrets", false, "send declared secrets again (the server never returns them, so changed values are not noticed otherwise)")
	fs.BoolVar(&p.verbose, "v", false, "also list unchanged resources")
}

func (p *planFlags) parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q (use -f for files)", fs.Args())
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "apply-set" {
			p.applySetSet = true
		}
	})
	if len(p.files) == 0 {
		return errors.New("-f is required")
	}
	if p.prune && !p.applySetSet {
		return errors.New("--prune needs an explicit --apply-set NAME, so that it only deletes what this configuration manages")
	}
	return nil
}

// makePlan loads the files and compares them with the server.
func (p *planFlags) makePlan(ctx context.Context, stdin io.Reader, stderr io.Writer) (*cac.Plan, *cac.Client, error) {
	docs, err := cac.Load(p.files, p.recursive, stdin, cac.Resolver{})
	if err != nil {
		return nil, nil, err
	}
	if len(docs) == 0 {
		return nil, nil, fmt.Errorf("no resources in %s", strings.Join(p.files, ", "))
	}
	c, err := p.client(stderr)
	if err != nil {
		return nil, nil, err
	}
	plan, err := cac.MakePlan(ctx, c, c.Server(), docs, cac.Options{ApplySet: p.applySet, Prune: p.prune,
		TakeOwnership: p.takeOwnership, UpdateSecrets: p.updateSecrets})
	if err != nil {
		return nil, nil, err
	}
	return plan, c, nil
}

const applyUsage = `usage:
  hangar apply -f FILE|DIR|- [-f ...] [-R] --server URL [--token-file FILE]
               [--apply-set NAME] [--prune] [--dry-run] [--take-ownership]
               [--update-secrets] [-v] [--timeout 30s]
        make the server match the resource documents: create what is
        missing, update what differs; with --prune also delete what the
        apply set owns and the files no longer declare. --dry-run prints
        the plan only and exits like hangar diff.
  hangar diff  -f FILE|DIR|- [...]   same flags; print the plan, change nothing
        exit status: 0 no changes, 2 changes, 1 error
  hangar export --server URL [--kind LIST] [--apply-set NAME] [-o FILE]
        write the server's configuration as resource documents; secrets
        become valueFrom references
The token is read from $HANGAR_TOKEN or --token-file. See
docs/guides/config-as-code.md.
`

func cacCommand(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch name {
	case "apply":
		return applyCmd(ctx, args, stdin, stdout, stderr)
	case "diff":
		return diffCmd(ctx, args, stdin, stdout, stderr)
	default:
		return exportCmd(ctx, args, stdout, stderr)
	}
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprint(stderr, applyUsage, "\nflags:\n")
		fs.PrintDefaults()
	}
	return fs
}

func applyCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("apply", stderr)
	var p planFlags
	p.register(fs)
	dryRun := fs.Bool("dry-run", false, "print the plan, change nothing (exit status as hangar diff)")
	if err := p.parse(fs, args); err != nil {
		return err
	}
	plan, c, err := p.makePlan(ctx, stdin, stderr)
	if err != nil {
		return err
	}
	plan.Render(stdout, p.verbose)
	if *dryRun {
		if plan.HasChanges() {
			return exitCode(2)
		}
		return nil
	}
	if !plan.HasChanges() {
		return nil
	}
	_, _ = fmt.Fprintln(stdout)
	return cac.Apply(ctx, c, plan, stdout)
}

func diffCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("diff", stderr)
	var p planFlags
	p.register(fs)
	if err := p.parse(fs, args); err != nil {
		return err
	}
	plan, _, err := p.makePlan(ctx, stdin, stderr)
	if err != nil {
		return err
	}
	plan.Render(stdout, p.verbose)
	if plan.HasChanges() {
		return exitCode(2)
	}
	return nil
}

func exportCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("export", stderr)
	var r remoteFlags
	r.register(fs)
	kinds := fs.String("kind", "", "comma-separated kinds to export (default all): "+strings.Join(cac.KindNames(), ", "))
	applySet := fs.String("apply-set", "", "only export resources this apply set owns")
	out := fs.String("o", "-", "output file, - for standard output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	c, err := r.client(stderr)
	if err != nil {
		return err
	}
	var o cac.ExportOptions
	for k := range strings.SplitSeq(*kinds, ",") {
		if k = strings.TrimSpace(k); k != "" {
			o.Kinds = append(o.Kinds, k)
		}
	}
	o.ApplySet = *applySet
	// A file is written only after the export succeeded, so a failure
	// never leaves a truncated file behind.
	var b strings.Builder
	w := stdout
	if *out != "-" {
		w = &b
	}
	notes, err := cac.Export(ctx, c, o, w)
	for _, n := range notes {
		_, _ = fmt.Fprintf(stderr, "note: %s\n", n)
	}
	if err != nil || *out == "-" {
		return err
	}
	return os.WriteFile(*out, []byte(b.String()), 0o600)
}
