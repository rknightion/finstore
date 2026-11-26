// Command finstore is a self-hosted datastore for one person's bank data.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/rknightion/finstore/internal/app"
	"github.com/rknightion/finstore/internal/config"
	"github.com/rknightion/finstore/internal/ledger"
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
  account accept-gap          record a gap between two statements as accepted
  account end                 end an account at a date, or clear its end
  account scope               set an account's scope, or clear it
  payee list|show             list payees, or show one payee with its keys, rules and series
  payee merge|separate|rename|class|category|recurring|clear
                              record an operator rule about a payee or a recurring series
  category name|clear         name a category, or clear its name
  txn class|clear             set the flow class of one transaction, or clear it
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
	case "payee":
		return runPayee(ctx, env, rest, flags, parse)
	case "category":
		return runCategory(ctx, env, rest, flags, parse)
	case "txn":
		return runTxn(ctx, env, rest, flags, parse)
	case "import":
		fs := flags(command)
		account := fs.String("account", "", "slug of the account the statement belongs to, or of the platform for an investment file")
		format := fs.String("format", "", "the file's format, such as "+app.FormatHalifaxPDF+" or "+app.FormatManualCSV)
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
		_, _ = fmt.Fprintln(env.Stderr, "finstore account: expected add, list, accept-difference, accept-gap, end or scope")
		return exitUsage
	}
	switch args[0] {
	case "add":
		fs := flags("account add")
		var spec app.AccountSpec
		fs.StringVar(&spec.Slug, "slug", "", "short name used in commands and views, such as bank-joint")
		fs.StringVar(&spec.Type, "type", "", "current, joint, savings, mortgage or platform")
		fs.StringVar(&spec.Provider, "provider", "", "for --type platform only: fidelity or aviva")
		fs.StringVar(&spec.Currency, "currency", "GBP", "three-letter currency code")
		fs.StringVar(&spec.Name, "name", "", "display name")
		fs.BoolVar(&spec.ExpectsImports, "expects-imports", false, "report an import as due when the newest statement is over 35 days old (70 days for an investment account's newest valuation)")
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
	case "accept-gap":
		fs := flags("account accept-gap")
		slug := fs.String("slug", "", "slug of the account")
		from := fs.String("from", "", "the last date of the statement before the gap, YYYY-MM-DD, as coverage shows it")
		to := fs.String("to", "", "the first date of the statement after the gap, YYYY-MM-DD, as coverage shows it")
		reason := fs.String("reason", "", "why the gap is accepted")
		if !parse(fs, args[1:]) {
			return exitUsage
		}
		if *slug == "" || *from == "" || *to == "" {
			_, _ = fmt.Fprintln(env.Stderr, "finstore account accept-gap: --slug, --from and --to are required")
			return exitUsage
		}
		after, err1 := time.Parse(time.DateOnly, *from)
		before, err2 := time.Parse(time.DateOnly, *to)
		if err1 != nil || err2 != nil {
			_, _ = fmt.Fprintln(env.Stderr, "finstore account accept-gap: --from and --to must be YYYY-MM-DD")
			return exitUsage
		}
		return app.AcceptGap(ctx, env, *slug, after, before, *reason)
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
	case "scope":
		fs := flags("account scope")
		slug := fs.String("slug", "", "slug of the account")
		scope := fs.String("scope", "", "personal, joint or business")
		clear := fs.Bool("clear", false, "remove the account's scope setting instead")
		if !parse(fs, args[1:]) {
			return exitUsage
		}
		if *slug == "" || (*scope == "") == !*clear {
			_, _ = fmt.Fprintln(env.Stderr, "finstore account scope: --slug and exactly one of --scope and --clear are required")
			return exitUsage
		}
		if *clear {
			return app.AccountScope(ctx, env, *slug, nil)
		}
		return app.AccountScope(ctx, env, *slug, scope)
	default:
		_, _ = fmt.Fprintf(env.Stderr, "finstore account: unknown subcommand %q\n", args[0])
		return exitUsage
	}
}

// requireFlags reports whether every named string flag has a value; it prints what is missing
// otherwise.
func requireFlags(env app.Env, command string, required map[string]string) bool {
	var missing []string
	for name, value := range required {
		if value == "" {
			missing = append(missing, "--"+name)
		}
	}
	if len(missing) == 0 {
		return true
	}
	slices.Sort(missing)
	_, _ = fmt.Fprintf(env.Stderr, "finstore %s: %s required\n", command, strings.Join(missing, ", "))
	return false
}

func runPayee(ctx context.Context, env app.Env, args []string,
	flags func(string) *flag.FlagSet, parse func(*flag.FlagSet, []string) bool,
) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(env.Stderr, "finstore payee: expected list, show, merge, separate, rename, class, category, recurring or clear")
		return exitUsage
	}
	command := "payee " + args[0]
	fs := flags(command)
	key := fs.String("key", "", "the payee key, such as mg:<group id> or n:<name>")
	switch args[0] {
	case "list":
		query := fs.String("query", "", "only payees whose name contains this text (case-insensitive)")
		limit := fs.Int("limit", app.DefaultPayeeListLimit, "the most payees to print")
		if !parse(fs, args[1:]) {
			return exitUsage
		}
		return app.PayeeList(ctx, env, *query, *limit)
	case "show":
		if !parse(fs, args[1:]) || !requireFlags(env, command, map[string]string{"key": *key}) {
			return exitUsage
		}
		return app.PayeeShow(ctx, env, *key)
	case "merge":
		into := fs.String("into", "", "the payee key to merge into")
		if !parse(fs, args[1:]) || !requireFlags(env, command, map[string]string{"key": *key, "into": *into}) {
			return exitUsage
		}
		return app.PayeeMerge(ctx, env, *key, *into)
	case "separate":
		if !parse(fs, args[1:]) || !requireFlags(env, command, map[string]string{"key": *key}) {
			return exitUsage
		}
		return app.PayeeSeparate(ctx, env, *key)
	case "rename":
		name := fs.String("name", "", "the display name")
		if !parse(fs, args[1:]) || !requireFlags(env, command, map[string]string{"key": *key, "name": *name}) {
			return exitUsage
		}
		return app.PayeeRename(ctx, env, *key, *name)
	case "class":
		class := fs.String("class", "", "the flow class: "+strings.Join(ledger.FlowClasses, ", "))
		employer := fs.Bool("employer", false, "the payee is an employer (only with --class income)")
		if !parse(fs, args[1:]) || !requireFlags(env, command, map[string]string{"key": *key, "class": *class}) {
			return exitUsage
		}
		return app.PayeeClass(ctx, env, *key, *class, *employer)
	case "category":
		category := fs.String("category", "", "the category")
		if !parse(fs, args[1:]) || !requireFlags(env, command, map[string]string{"key": *key, "category": *category}) {
			return exitUsage
		}
		return app.PayeeCategory(ctx, env, *key, *category)
	case "recurring":
		series := fs.String("series", "", "the series key, such as sub:<payee key>")
		decision := fs.String("decision", "", "confirm, ignore or not-recurring")
		if !parse(fs, args[1:]) || !requireFlags(env, command, map[string]string{"series": *series, "decision": *decision}) {
			return exitUsage
		}
		return app.PayeeRecurring(ctx, env, *series, *decision)
	case "clear":
		series := fs.String("series", "", "the series key whose decision to remove")
		what := fs.String("what", "", "what to remove from --key: merge, separate, name, class or category")
		if !parse(fs, args[1:]) {
			return exitUsage
		}
		if (*series == "") == (*key == "") || (*series != "" && *what != "") || (*key != "" && *what == "") {
			_, _ = fmt.Fprintln(env.Stderr, "finstore payee clear: either --key with --what, or --series alone, is required")
			return exitUsage
		}
		return app.PayeeClear(ctx, env, *key, *what, *series)
	default:
		_, _ = fmt.Fprintf(env.Stderr, "finstore payee: unknown subcommand %q\n", args[0])
		return exitUsage
	}
}

func runTxn(ctx context.Context, env app.Env, args []string,
	flags func(string) *flag.FlagSet, parse func(*flag.FlagSet, []string) bool,
) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(env.Stderr, "finstore txn: expected class or clear")
		return exitUsage
	}
	command := "txn " + args[0]
	fs := flags(command)
	id := fs.String("id", "", "the transaction id, as agent.transactions shows it")
	switch args[0] {
	case "class":
		class := fs.String("class", "", "the flow class: "+strings.Join(ledger.FlowClasses, ", "))
		employer := fs.Bool("employer", false, "the transaction is pay from an employer (only with --class income)")
		if !parse(fs, args[1:]) || !requireFlags(env, command, map[string]string{"id": *id, "class": *class}) {
			return exitUsage
		}
		return app.TxnClass(ctx, env, *id, *class, *employer)
	case "clear":
		if !parse(fs, args[1:]) || !requireFlags(env, command, map[string]string{"id": *id}) {
			return exitUsage
		}
		return app.TxnClear(ctx, env, *id)
	default:
		_, _ = fmt.Fprintf(env.Stderr, "finstore txn: unknown subcommand %q\n", args[0])
		return exitUsage
	}
}

func runCategory(ctx context.Context, env app.Env, args []string,
	flags func(string) *flag.FlagSet, parse func(*flag.FlagSet, []string) bool,
) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(env.Stderr, "finstore category: expected name or clear")
		return exitUsage
	}
	command := "category " + args[0]
	fs := flags(command)
	category := fs.String("category", "", "the category, as agent.transactions shows it")
	switch args[0] {
	case "name":
		name := fs.String("name", "", "the display name")
		if !parse(fs, args[1:]) || !requireFlags(env, command, map[string]string{"category": *category, "name": *name}) {
			return exitUsage
		}
		return app.CategoryName(ctx, env, *category, *name)
	case "clear":
		if !parse(fs, args[1:]) || !requireFlags(env, command, map[string]string{"category": *category}) {
			return exitUsage
		}
		return app.CategoryClear(ctx, env, *category)
	default:
		_, _ = fmt.Fprintf(env.Stderr, "finstore category: unknown subcommand %q\n", args[0])
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
