package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/testpg"
)

// openReader opens a reader as finstore_mcp on a fresh cluster.
func openReader(t *testing.T, opts ...store.OpenOption) (*store.Reader, cluster) {
	t.Helper()
	c := newCluster(t)
	return openReaderAt(t, c, c.mcpURL(t), opts...), c
}

func openReaderAt(t *testing.T, c cluster, url string, opts ...store.OpenOption) *store.Reader {
	t.Helper()
	r, err := store.OpenReader(context.Background(), url, opts...)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	t.Cleanup(r.Close)
	return r
}

var smallLimits = store.Limits{MaxRows: 100, MaxBytes: 1 << 20}

func TestReaderRefusesASecondStatement(t *testing.T) {
	r, _ := openReader(t)
	ctx := context.Background()
	start := time.Now()
	// The second statement is refused before anything runs: the sleep never starts.
	_, err := r.Query(ctx, "SELECT pg_sleep(5); SELECT 1", smallLimits)
	if err == nil || store.Outcome(err) != store.OutcomeRefused || pgCode(err) != "42601" {
		t.Fatalf("two statements: outcome %q, error %v (%s), want refused (42601)", store.Outcome(err), err, pgCode(err))
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("the refusal took %v: the first statement ran", d)
	}
	// One statement with a trailing semicolon is one statement.
	res, err := r.Query(ctx, "SELECT 1;", smallLimits)
	if err != nil || len(res.Rows) != 1 {
		t.Fatalf("one statement with a semicolon: %+v %v", res, err)
	}
	// The reader still works after a refusal.
	if _, err := r.Query(ctx, "SELECT 2", smallLimits); err != nil {
		t.Fatalf("after a refusal: %v", err)
	}
}

func TestReaderOutcomes(t *testing.T) {
	r, _ := openReader(t)
	ctx := context.Background()
	for _, tt := range []struct{ name, sql, want string }{
		{"ok", "SELECT 1", store.OutcomeOK},
		{"no rows", "SELECT 1 WHERE false", store.OutcomeOK},
		{"syntax", "SELEC 1", store.OutcomeRefused},
		{"secret", "SELECT * FROM secret.oauth_token", store.OutcomeRefused},
		{"write in the read-only transaction", "INSERT INTO raw.fetch (source, kind, run_id, started_at) VALUES ('x', 'y', 1, now())", store.OutcomeRefused},
		{"create", "CREATE TEMP TABLE leftover (x int)", store.OutcomeRefused},
		{"runtime error", "SELECT 1 / 0", store.OutcomeError},
		{"missing relation", "SELECT * FROM core.nonexistent", store.OutcomeError},
		{"empty", "  ", store.OutcomeInvalid},
	} {
		_, err := r.Query(ctx, tt.sql, smallLimits)
		if got := store.Outcome(err); got != tt.want {
			t.Errorf("%s: outcome %q (%v), want %q", tt.name, got, err, tt.want)
		}
	}
	for _, l := range []store.Limits{{}, {MaxRows: 1}, {MaxBytes: 1}, {MaxRows: -1, MaxBytes: 10}} {
		if _, err := r.Query(ctx, "SELECT 1", l); store.Outcome(err) != store.OutcomeInvalid {
			t.Errorf("limits %+v: outcome %q (%v), want invalid", l, store.Outcome(err), err)
		}
	}
	if store.Outcome(nil) != store.OutcomeOK || store.Outcome(errors.New("x")) != store.OutcomeError ||
		store.Outcome(store.ErrBusy) != store.OutcomeBusy || store.Outcome(context.DeadlineExceeded) != store.OutcomeTimeout {
		t.Error("Outcome misclassifies a plain error")
	}
}

// tamper does to the role what a login as it can do for itself: remove its limits, role-wide and
// for the database, and turn read-only off.
func tamper(t *testing.T, c cluster) {
	t.Helper()
	conn := connect(t, c.super)
	inDB := "ALTER ROLE finstore_mcp IN DATABASE " + testpg.Database + " SET "
	for _, stmt := range []string{
		inDB + "statement_timeout = 0", inDB + "lock_timeout = 0", inDB + "idle_in_transaction_session_timeout = 0",
		inDB + "default_transaction_read_only = off",
		"ALTER ROLE finstore_mcp SET statement_timeout = 0", "ALTER ROLE finstore_mcp SET default_transaction_read_only = off",
	} {
		if _, err := conn.Exec(context.Background(), stmt); err != nil {
			t.Fatal(err)
		}
	}
}

// The reader's own 15 s statement_timeout ends a statement that runs too long, even for a role whose
// settings were rewritten (a login as the role can ALTER ROLE itself, per database too).
func TestReaderTimesOutAtFifteenSeconds(t *testing.T) {
	r, c := openReader(t)
	tamper(t, c)
	start := time.Now()
	_, err := r.Query(context.Background(), "SELECT pg_sleep(30)", smallLimits)
	d := time.Since(start)
	if err == nil || store.Outcome(err) != store.OutcomeTimeout || pgCode(err) != "57014" {
		t.Fatalf("pg_sleep(30): outcome %q, error %v (%s), want timeout (57014)", store.Outcome(err), err, pgCode(err))
	}
	if d < 14*time.Second || d > 20*time.Second {
		t.Errorf("the statement ran %v, want about 15 s", d)
	}
	// The reader works afterwards.
	if _, err := r.Query(context.Background(), "SELECT 1", smallLimits); err != nil {
		t.Fatalf("after a timeout: %v", err)
	}
}

// The Go deadline of a whole call ends one the database does not, and is a timeout too.
func TestReaderGoDeadlineIsATimeout(t *testing.T) {
	r, _ := openReader(t)
	r.SetTimingsForTest(time.Second, 700*time.Millisecond)
	start := time.Now()
	_, err := r.Query(context.Background(), "SELECT pg_sleep(10)", smallLimits)
	if store.Outcome(err) != store.OutcomeTimeout {
		t.Fatalf("outcome %q (%v), want timeout", store.Outcome(err), err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the call ran %v past a 0.7 s deadline", d)
	}
}

// activeStatements counts finstore_mcp backends that are running a statement.
func activeStatements(t *testing.T, c cluster) int {
	t.Helper()
	var n int
	if err := connect(t, c.super).QueryRow(context.Background(),
		`SELECT count(*) FROM pg_stat_activity WHERE usename = 'finstore_mcp' AND state = 'active'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestReaderTruncatesAtTheRowCapAndStopsTheQuery(t *testing.T) {
	r, c := openReader(t)
	ctx := context.Background()
	start := time.Now()
	// Far more rows than could be read in the test's lifetime: reading them all is not an option.
	res, err := r.Query(ctx, "SELECT generate_series(1, 1000000000) AS g", store.Limits{MaxRows: 5, MaxBytes: 1 << 20})
	if err != nil || store.Outcome(err) != store.OutcomeOK {
		t.Fatalf("truncated query: outcome %q, %v", store.Outcome(err), err)
	}
	if len(res.Rows) != 5 || !res.Truncated || len(res.Columns) != 1 || res.Columns[0] != "g" {
		t.Fatalf("truncated query: %d rows, truncated %v, columns %v", len(res.Rows), res.Truncated, res.Columns)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("the capped query took %v: it was not stopped", d)
	}
	// The statement does not keep running on the server.
	deadline := time.Now().Add(10 * time.Second)
	for activeStatements(t, c) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the cancelled statement is still active after 10 s")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Exactly the cap is not a truncation.
	res, err = r.Query(ctx, "SELECT g FROM generate_series(1, 5) g", store.Limits{MaxRows: 5, MaxBytes: 1 << 20})
	if err != nil || len(res.Rows) != 5 || res.Truncated {
		t.Fatalf("exactly MaxRows rows: %d rows, truncated %v, %v", len(res.Rows), res.Truncated, err)
	}
}

func TestReaderTruncatesAtTheByteCap(t *testing.T) {
	r, _ := openReader(t)
	ctx := context.Background()
	// Each row encodes as ["xxxx...", n]: about 1 KB.
	const sql = "SELECT repeat('x', 1000) AS pad, g FROM generate_series(1, 100) g"
	res, err := r.Query(ctx, sql, store.Limits{MaxRows: 100, MaxBytes: 3500})
	if err != nil || store.Outcome(err) != store.OutcomeOK {
		t.Fatalf("byte-capped query: outcome %q, %v", store.Outcome(err), err)
	}
	if len(res.Rows) != 3 || !res.Truncated {
		t.Fatalf("byte-capped query: %d rows, truncated %v, want 3 rows and truncated", len(res.Rows), res.Truncated)
	}
	enc, _ := json.Marshal(res.Rows)
	if len(enc) > 3500 {
		t.Errorf("the returned rows encode to %d bytes, over the 3500 cap", len(enc))
	}
	// A cap that holds every row is not a truncation.
	res, err = r.Query(ctx, "SELECT repeat('x', 1000), g FROM generate_series(1, 3) g", store.Limits{MaxRows: 100, MaxBytes: 3500})
	if err != nil || len(res.Rows) != 3 || res.Truncated {
		t.Fatalf("rows that fit: %d rows, truncated %v, %v", len(res.Rows), res.Truncated, err)
	}
	// A single row over the cap is not returned; the result says so by truncating.
	res, err = r.Query(ctx, "SELECT repeat('x', 5000)", store.Limits{MaxRows: 100, MaxBytes: 1000})
	if err != nil || len(res.Rows) != 0 || !res.Truncated || res.Rows == nil {
		t.Fatalf("one oversize row: %d rows (nil %v), truncated %v, %v", len(res.Rows), res.Rows == nil, res.Truncated, err)
	}
}

func TestReaderEncodesEveryType(t *testing.T) {
	// pgx decodes an instant into the process's zone; the encoding must not show it. This holds in any
	// zone (run the test with TZ=Asia/Kolkata to see it fail if the conversion is removed).
	r, _ := openReader(t)
	cells := []struct{ sql, want string }{
		{"1::int2", `1`},
		{"-2::int4", `-2`},
		{"9007199254740992::int8", `9007199254740992`},
		{"-9007199254740992::int8", `-9007199254740992`},
		{"9007199254740993::int8", `"9007199254740993"`},
		{"-9007199254740993::int8", `"-9007199254740993"`},
		{"9223372036854775807::int8", `"9223372036854775807"`},
		{"1.50::numeric", `"1.50"`},
		{"'NaN'::numeric", `"NaN"`},
		{"1.5::float8", `1.5`},
		{"0.1::float4", `0.1`},
		{"'NaN'::float8", `"NaN"`},
		{"'-Infinity'::float8", `"-Infinity"`},
		{"true", `true`},
		{"false", `false`},
		{"'hello'::text", `"hello"`},
		{"'quote\"d'::varchar", `"quote\"d"`},
		{"'a_name'::name", `"a_name"`},
		{"'6f1c1f5e-2f43-4a39-9a3e-3b3e1d1b0c55'::uuid", `"6f1c1f5e-2f43-4a39-9a3e-3b3e1d1b0c55"`},
		{"'2026-03-01'::date", `"2026-03-01"`},
		{"'infinity'::date", `"infinity"`},
		{"'2026-03-01 09:00:00+01'::timestamptz", `"2026-03-01T08:00:00Z"`},
		{"'2026-03-01 09:00:00.25+00'::timestamptz", `"2026-03-01T09:00:00.25Z"`},
		{"'2026-03-01 09:00:00'::timestamp", `"2026-03-01T09:00:00Z"`},
		{"'-infinity'::timestamptz", `"-infinity"`},
		{`'{"a": [1, 2.50, null], "b": "x"}'::jsonb`, `{"a":[1,2.50,null],"b":"x"}`},
		{`'{"a": 1}'::json`, `{"a":1}`},
		{`'"text"'::jsonb`, `"text"`},
		{`'\x010203'::bytea`, `{"bytea_length":3}`},
		{`''::bytea`, `{"bytea_length":0}`},
		{"int4range(1, 5)", `"[1,5)"`},
		{"ARRAY[1, 2, 3]", `"{1,2,3}"`},
		{"ARRAY['a', 'b c']", `"{a,\"b c\"}"`},
		{"'1 day 02:00:00'::interval", `"1 day 02:00:00"`},
		{"'10.0.0.1'::inet", `"10.0.0.1"`},
		{"'a'::char(3)", `"a  "`},
		{"NULL::text", `null`},
		{"NULL::int8", `null`},
		{"NULL::jsonb", `null`},
		{"NULL::bytea", `null`},
		{"NULL::timestamptz", `null`},
	}
	var sql strings.Builder
	sql.WriteString("SELECT ")
	for i, c := range cells {
		if i > 0 {
			sql.WriteString(", ")
		}
		sql.WriteString(c.sql + " AS c" + strconv.Itoa(i))
	}
	res, err := r.Query(context.Background(), sql.String(), smallLimits)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Rows) != 1 || len(res.Rows[0]) != len(cells) || len(res.Columns) != len(cells) || res.Columns[3] != "c3" {
		t.Fatalf("shape: %d rows, columns %v", len(res.Rows), res.Columns)
	}
	for i, c := range cells {
		got, err := json.Marshal(res.Rows[0][i])
		if err != nil {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("%s encodes as %s, want %s", c.sql, got, c.want)
		}
	}
	// A zero-row result has its columns and an empty, non-nil row list (JSON [] and not null).
	res, err = r.Query(context.Background(), "SELECT 1 AS one, 'x'::text AS two WHERE false", smallLimits)
	if err != nil || len(res.Rows) != 0 || res.Rows == nil || strings.Join(res.Columns, ",") != "one,two" || res.Truncated {
		t.Fatalf("no rows: %+v %v", res, err)
	}
	if res.Elapsed <= 0 {
		t.Error("Elapsed is not set")
	}
}

// The transaction is read only because the reader says so (an explicit READ ONLY transaction, on a
// session whose startup parameters also say so), not because the role's default does: a role whose
// default were turned off still cannot write, not even to a temporary table (which its grants
// would allow in a read-write transaction).
func TestReaderTransactionIsReadOnlyWhateverTheRoleDefaultSays(t *testing.T) {
	r, c := openReader(t)
	ctx := context.Background()
	tamper(t, c)
	for _, sql := range []string{
		"CREATE TEMP TABLE scratch (x int)",
		"CREATE TEMP TABLE scratch AS SELECT 1 AS x",
		"SELECT * INTO TEMP scratch FROM generate_series(1, 3)",
	} {
		_, err := r.Query(ctx, sql, smallLimits)
		if store.Outcome(err) != store.OutcomeRefused || pgCode(err) != "25006" {
			t.Errorf("%s: outcome %q, error %v (%s), want refused (25006)", sql, store.Outcome(err), err, pgCode(err))
		}
	}
}

// Text and date types come out the same whatever the session's DateStyle says; the reader does not
// depend on it.
func TestReaderEncodingIgnoresSessionDateStyle(t *testing.T) {
	r, _ := openReader(t)
	res, err := r.Query(context.Background(),
		"SELECT set_config('DateStyle', 'SQL, DMY', false) AS s, '2026-03-04'::date AS d, '2026-03-04 05:06:07+00'::timestamptz AS t",
		smallLimits)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows[0][1] != "2026-03-04" || res.Rows[0][2] != "2026-03-04T05:06:07Z" {
		t.Errorf("under another DateStyle: %v", res.Rows[0])
	}
}

// waitFor retries cond for up to five seconds; a backend takes a moment to end after its socket closes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within 5 s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestReaderLeavesNoAdvisoryLockBehind(t *testing.T) {
	r, c := openReader(t)
	ctx := context.Background()
	key := strconv.FormatInt(store.WriterLockKey, 10)
	if _, err := r.Query(ctx, "SELECT pg_advisory_lock("+key+")", smallLimits); err != nil {
		t.Fatalf("take the lock through the reader: %v", err)
	}
	// An app connection can take the writer lock: the reader's session, and its lock, ended with the call.
	app := connect(t, c.app)
	waitFor(t, "the writer lock being free", func() bool {
		var got bool
		if err := app.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", store.WriterLockKey).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	})
}

func TestReaderLeavesNoSessionStateBehind(t *testing.T) {
	r, _ := openReader(t)
	ctx := context.Background()
	if res, err := r.Query(ctx, "SELECT set_config('finstore.probe', 'x', false)", smallLimits); err != nil || res.Rows[0][0] != "x" {
		t.Fatalf("set_config: %+v %v", res, err)
	}
	// current_setting is null for a custom setting the session never saw, and 'x' for one it did.
	res, err := r.Query(ctx, "SELECT current_setting('finstore.probe', true)", smallLimits)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows[0][0] != nil {
		t.Fatalf("the next call sees finstore.probe = %v: the session was reused", res.Rows[0][0])
	}
	// The same goes for a setting that changes how the next statement behaves.
	if _, err := r.Query(ctx, "SELECT set_config('search_path', 'pg_catalog', false)", smallLimits); err != nil {
		t.Fatal(err)
	}
	res, err = r.Query(ctx, "SHOW search_path", smallLimits)
	if err != nil || res.Rows[0][0] == "pg_catalog" {
		t.Fatalf("search_path leaked into the next call: %v %v", res.Rows, err)
	}
}

func TestReaderSerialisesCallsAndReturnsBusyPastTheWait(t *testing.T) {
	r, _ := openReader(t)
	ctx := context.Background()

	// Two calls of one second each, started together, run one after the other.
	var wg sync.WaitGroup
	start := time.Now()
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Query(ctx, "SELECT pg_sleep(1)", smallLimits); err != nil {
				t.Errorf("concurrent call: %v", err)
			}
		}()
	}
	wg.Wait()
	if d := time.Since(start); d < 1900*time.Millisecond {
		t.Errorf("two 1 s calls took %v: they ran at once", d)
	}

	// With a short wait, a call that cannot get the connection in time is busy; the one holding it
	// is not disturbed.
	r.SetTimingsForTest(300*time.Millisecond, 50*time.Second)
	holder := make(chan error, 1)
	go func() {
		_, err := r.Query(ctx, "SELECT pg_sleep(2)", smallLimits)
		holder <- err
	}()
	time.Sleep(500 * time.Millisecond)
	waited := time.Now()
	_, err := r.Query(ctx, "SELECT 1", smallLimits)
	if !errors.Is(err, store.ErrBusy) || store.Outcome(err) != store.OutcomeBusy {
		t.Fatalf("a call behind a long one: outcome %q, %v, want busy", store.Outcome(err), err)
	}
	if d := time.Since(waited); d < 250*time.Millisecond || d > 1500*time.Millisecond {
		t.Errorf("the busy answer came after %v, want about the 300 ms wait", d)
	}
	if err := <-holder; err != nil {
		t.Errorf("the call holding the connection: %v", err)
	}
	// And once it is free the reader answers again.
	if _, err := r.Query(ctx, "SELECT 1", smallLimits); err != nil {
		t.Fatalf("after the busy period: %v", err)
	}
}

// The reader's limits are its own: startup parameters outrank what the role says, so a role whose
// settings were all rewritten still gets 15 s, 2 s, 30 s and read only, and cannot write or create.
func TestReaderLimitsHoldWhateverTheRoleSettingsSay(t *testing.T) {
	r, c := openReader(t)
	ctx := context.Background()
	tamper(t, c)
	// A direct login sees the tampered values: the premise of the test.
	var direct string
	if err := connect(t, c.mcpURL(t)).QueryRow(ctx, "SHOW statement_timeout").Scan(&direct); err != nil || direct != "0" {
		t.Fatalf("the role's statement_timeout after the tamper: %q %v, want 0", direct, err)
	}
	for setting, want := range map[string]string{
		"statement_timeout": "15s", "lock_timeout": "2s", "idle_in_transaction_session_timeout": "30s",
		"default_transaction_read_only": "on", "transaction_read_only": "on", "temp_file_limit": "1GB",
	} {
		res, err := r.Query(ctx, "SELECT current_setting('"+setting+"')", smallLimits)
		if err != nil || res.Rows[0][0] != want {
			t.Errorf("%s = %v (%v), want %q", setting, res.Rows, err, want)
		}
	}
	for _, sql := range []string{"CREATE TEMP TABLE scratch (x int)", "INSERT INTO raw.fetch DEFAULT VALUES", "DELETE FROM core.account"} {
		if _, err := r.Query(ctx, sql, smallLimits); store.Outcome(err) != store.OutcomeRefused {
			t.Errorf("%s: outcome %q (%v), want refused", sql, store.Outcome(err), err)
		}
	}
}

// Cancelling a call, by its deadline, its caller or a truncated result, stops the statement on the
// server: a CPU-bound one that never writes to the socket does not keep running into the next call.
func TestReaderCancelStopsACPUBoundStatement(t *testing.T) {
	r, c := openReader(t)
	const cpuBound = "SELECT count(*) FROM (SELECT generate_series(1, 100000000000)) s"
	waitIdle := func(what string) {
		t.Helper()
		waitFor(t, "no active finstore_mcp statement after "+what, func() bool { return activeStatements(t, c) == 0 })
	}

	// The caller's deadline.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := r.Query(ctx, cpuBound, smallLimits)
	// 57014 is the server's own answer to a cancel request: the statement was cancelled there, not
	// abandoned by closing the socket (which gives a network timeout and no SQLSTATE).
	if err == nil || store.Outcome(err) != store.OutcomeTimeout || pgCode(err) != "57014" {
		t.Fatalf("cancelled CPU-bound call: outcome %q, error %v (%s), want timeout answered by the server (57014)",
			store.Outcome(err), err, pgCode(err))
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the cancelled call returned after %v", d)
	}
	waitIdle("a deadline")

	// A statement that waits and never writes to the socket, which closing the socket does not
	// reach: it would run to the 15 s statement_timeout.
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if _, err := r.Query(ctx2, "SELECT pg_sleep(100)", smallLimits); pgCode(err) != "57014" {
		t.Fatalf("a cancelled pg_sleep call: %v (%s), want the server's cancel answer (57014)", err, pgCode(err))
	}
	waitIdle("a deadline on pg_sleep")

	// The reader's own Go deadline.
	r.SetTimingsForTest(time.Second, time.Second)
	if _, err := r.Query(context.Background(), cpuBound, smallLimits); store.Outcome(err) != store.OutcomeTimeout || pgCode(err) != "57014" {
		t.Fatalf("CPU-bound call past the Go deadline: outcome %q, %v (%s)", store.Outcome(err), err, pgCode(err))
	}
	waitIdle("the Go deadline")

	// The caller giving up.
	r.SetTimingsForTest(store.ReaderWait, store.ReaderDeadline)
	ctx, cancel = context.WithCancel(context.Background())
	go func() { time.Sleep(500 * time.Millisecond); cancel() }()
	if _, err := r.Query(ctx, cpuBound, smallLimits); err == nil {
		t.Fatal("a cancelled call succeeded")
	}
	waitIdle("a cancel")

	// And the reader is usable after all three.
	if res, err := r.Query(context.Background(), "SELECT 1", smallLimits); err != nil || len(res.Rows) != 1 {
		t.Fatalf("after the cancelled calls: %v", err)
	}
}

// A superuser connection that starts with role=finstore_mcp has current_user = finstore_mcp and can
// reset it (set_config('role', 'none', false)) to read secret: CheckRole refuses it by session_user.
func TestReaderCheckRoleRefusesASuperuserActingAsTheRole(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	u, err := url.Parse(c.super)
	if err != nil {
		t.Fatal(err)
	}
	u.RawQuery += "&options=" + strings.ReplaceAll(url.QueryEscape("-c role="+store.RoleMCP), "+", "%20")
	r := openReaderAt(t, c, u.String())
	// The premise: current_user is the right one, and the session can leave it.
	res, err := r.Query(ctx, "SELECT current_user::text, session_user::text", smallLimits)
	if err != nil || res.Rows[0][0] != store.RoleMCP || res.Rows[0][1] != testpg.Superuser {
		t.Fatalf("the URL does not give role=finstore_mcp for a superuser session: %v %v", res.Rows, err)
	}
	err = r.CheckRole(ctx)
	if err == nil || !strings.Contains(err.Error(), testpg.Superuser) {
		t.Fatalf("CheckRole on a superuser session acting as finstore_mcp: %v", err)
	}
}

func TestReaderCheckRole(t *testing.T) {
	r, c := openReader(t)
	ctx := context.Background()
	if err := r.CheckRole(ctx); err != nil {
		t.Fatalf("CheckRole as finstore_mcp: %v", err)
	}
	// The app and the owner can log in with the test cluster's trust authentication: a reader opened
	// as either is refused.
	for _, url := range []string{c.app, c.owner} {
		bad := openReaderAt(t, c, url)
		err := bad.CheckRole(ctx)
		if err == nil || !strings.Contains(err.Error(), store.RoleMCP) {
			t.Errorf("CheckRole on a reader that is not finstore_mcp: %v", err)
		}
	}
}

func TestOpenReaderFailsWhenTheDatabaseIsUnreachable(t *testing.T) {
	c := newCluster(t)
	url := c.mcpURL(t)
	url = strings.Replace(url, "/finstore?", "/no_such_database?", 1)
	r, err := store.OpenReader(context.Background(), url)
	if err == nil {
		r.Close()
		t.Fatal("OpenReader accepted a database that does not exist")
	}
}
