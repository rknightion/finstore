// Command finstore is a self-hosted datastore for one person's bank data.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rknightion/finstore/internal/app"
	"github.com/rknightion/finstore/internal/config"
)

// Set at build time with -ldflags.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv)
	stop()
	os.Exit(code)
}

const usageText = `usage: finstore <command> [flags]

commands:
  serve                       run the HTTP server, the scheduler and the Monzo worker
  bootstrap                   create the database roles and schemas (superuser)
  migrate                     apply migrations and grants (owner)
  renormalise [--verify]      rebuild the normalised record from the stored source data (owner)
  account add                 create an account fed by statement files
  account list                list accounts
  account accept-difference   record a known balance difference as accepted
  account end                 end an account at a date, or clear its end
  import                      import one statement from standard input
  load-capture                load API responses captured before the service existed
  coverage [--json]           print what is held for each account, and the gaps
  healthcheck                 exit 0 when the local server is healthy
  version                     print the version

Settings come from FINSTORE_ environment variables; see docs/operations.md.
`

// exitUsage is returned for a command line that cannot be understood.
const exitUsage = 64

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usageText)
		return exitUsage
	}
	command, rest := args[0], args[1:]
	switch command {
	case "version":
		_, _ = fmt.Fprintf(stdout, "finstore %s (commit %s, built %s)\n", version, commit, buildDate)
		return app.ExitOK
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, usageText)
		return app.ExitOK
	}

	cfg, err := config.Load(getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "finstore:", err)
		return app.ExitError
	}
	env := app.Env{Cfg: cfg, Stdin: stdin, Stdout: stdout, Stderr: stderr, Version: version}

	flags := func(name string) *flag.FlagSet {
		fs := flag.NewFlagSet("finstore "+name, flag.ContinueOnError)
		fs.SetOutput(stderr)
		return fs
	}
	// parse reports whether the flags parsed and no stray argument was left.
	parse := func(fs *flag.FlagSet, args []string) bool {
		if err := fs.Parse(args); err != nil {
			return false
		}
		if fs.NArg() > 0 {
			_, _ = fmt.Fprintf(stderr, "%s: unexpected argument %q\n", fs.Name(), fs.Arg(0))
			return false
		}
		return true
	}

	switch command {
	case "serve":
		if !parse(flags(command), rest) {
			return exitUsage
		}
		return app.Serve(ctx, env, nil)
	case "bootstrap":
		if !parse(flags(command), rest) {
			return exitUsage
		}
		return app.Bootstrap(ctx, env)
	case "migrate":
		if !parse(flags(command), rest) {
			return exitUsage
		}
		return app.Migrate(ctx, env)
	case "renormalise":
		fs := flags(command)
		verify := fs.Bool("verify", false, "rebuild a copy, compare it with the live record, and change nothing")
		if !parse(fs, rest) {
			return exitUsage
		}
		return app.Renormalise(ctx, env, *verify)
	case "account":
		return runAccount(ctx, env, rest, flags, parse)
	case "import":
		fs := flags(command)
		account := fs.String("account", "", "slug of the account the statement belongs to")
		format := fs.String("format", "", app.FormatHalifaxPDF+" or "+app.FormatManualCSV)
		name := fs.String("name", "", "the file's name, kept for reference")
		if !parse(fs, rest) {
			return exitUsage
		}
		if *account == "" || *format == "" {
			_, _ = fmt.Fprintln(stderr, "finstore import: --account and --format are required")
			return exitUsage
		}
		return app.Import(ctx, env, *account, *format, *name)
	case "load-capture":
		fs := flags(command)
		var dirs stringList
		fs.Var(&dirs, "dir", "a capture directory; repeat the flag to load several together")
		if !parse(fs, rest) {
			return exitUsage
		}
		return app.LoadCapture(ctx, env, dirs)
	case "coverage":
		fs := flags(command)
		asJSON := fs.Bool("json", false, "print JSON")
		if !parse(fs, rest) {
			return exitUsage
		}
		return app.Coverage(ctx, env, *asJSON)
	case "healthcheck":
		if !parse(flags(command), rest) {
			return exitUsage
		}
		return app.Healthcheck(ctx, env)
	default:
		_, _ = fmt.Fprintf(stderr, "finstore: unknown command %q\n\n%s", command, usageText)
		return exitUsage
	}
}

func runAccount(ctx context.Context, env app.Env, args []string,
	flags func(string) *flag.FlagSet, parse func(*flag.FlagSet, []string) bool,
) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(env.Stderr, "finstore account: expected add, list, accept-difference or end")
		return exitUsage
	}
	switch args[0] {
	case "add":
		fs := flags("account add")
		var spec app.AccountSpec
		fs.StringVar(&spec.Slug, "slug", "", "short name used in commands and views, such as bank-joint")
		fs.StringVar(&spec.Type, "type", "", "current, joint, savings or mortgage")
		fs.StringVar(&spec.Currency, "currency", "GBP", "three-letter currency code")
		fs.StringVar(&spec.Name, "name", "", "display name")
		fs.BoolVar(&spec.ExpectsImports, "expects-imports", false, "report an import as due when the newest statement is over 35 days old")
		if !parse(fs, args[1:]) {
			return exitUsage
		}
		return app.AccountAdd(ctx, env, spec)
	case "list":
		if !parse(flags("account list"), args[1:]) {
			return exitUsage
		}
		return app.AccountList(ctx, env)
	case "accept-difference":
		fs := flags("account accept-difference")
		slug := fs.String("slug", "", "slug of the account")
		reason := fs.String("reason", "", "why the difference is accepted")
		if !parse(fs, args[1:]) {
			return exitUsage
		}
		if *slug == "" {
			_, _ = fmt.Fprintln(env.Stderr, "finstore account accept-difference: --slug is required")
			return exitUsage
		}
		return app.AcceptDifference(ctx, env, *slug, *reason)
	case "end":
		fs := flags("account end")
		slug := fs.String("slug", "", "slug of the account")
		lastDate := fs.String("last-date", "", "the last day the account has transactions on, YYYY-MM-DD")
		clear := fs.Bool("clear", false, "remove the account's end instead")
		if !parse(fs, args[1:]) {
			return exitUsage
		}
		if *slug == "" || (*lastDate == "") == !*clear {
			_, _ = fmt.Fprintln(env.Stderr, "finstore account end: --slug and exactly one of --last-date and --clear are required")
			return exitUsage
		}
		if *clear {
			return app.AccountEnd(ctx, env, *slug, nil)
		}
		d, err := time.Parse(time.DateOnly, *lastDate)
		if err != nil {
			_, _ = fmt.Fprintln(env.Stderr, "finstore account end: --last-date must be YYYY-MM-DD")
			return exitUsage
		}
		return app.AccountEnd(ctx, env, *slug, &d)
	default:
		_, _ = fmt.Fprintf(env.Stderr, "finstore account: unknown subcommand %q\n", args[0])
		return exitUsage
	}
}

// stringList collects a repeated string flag.
type stringList []string

func (l *stringList) String() string { return fmt.Sprint([]string(*l)) }

func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}
