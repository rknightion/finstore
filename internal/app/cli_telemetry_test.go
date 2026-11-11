package app_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/app"
	"github.com/rknightion/finstore/internal/config"
	"github.com/rknightion/finstore/internal/monzotest"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/telemetry/telemetrytest"
	"github.com/rknightion/finstore/internal/testpg"
)

const testVersion = "main-test123"

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// newTelemetryWorld bootstraps and migrates a fresh database with telemetry off, then returns a
// configuration with OTLP pointed at sink, so only the commands a test runs reach the sink.
func newTelemetryWorld(t *testing.T, sink *telemetrytest.Sink, extra map[string]string) (*world, config.Config) {
	t.Helper()
	super := testpg.Start(t)
	env := map[string]string{
		config.EnvSuperuserDatabaseURL: super,
		config.EnvOwnerDatabaseURL:     testpg.UserURL(t, super, store.RoleOwner),
		config.EnvDatabaseURL:          testpg.UserURL(t, super, store.RoleApp),
		config.EnvOwnerPassword:        "owner-password",
		config.EnvAppPassword:          "app-password",
		config.EnvListenAddr:           "127.0.0.1:0",
		config.EnvPublicURL:            "https://finstore.example.test",
		config.EnvAllowedEmails:        ownerEmail,
		config.EnvMonzoClientID:        monzotest.DefaultClientID,
		config.EnvMonzoClientSecret:    monzotest.DefaultClientSecret,
		config.EnvMonzoAPIURL:          "https://api.example.test",
		config.EnvMonzoAuthURL:         "https://auth.example.test/",
		config.EnvEnvironment:          "test",
	}
	for k, v := range extra {
		env[k] = v
	}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, super: super, cfg: cfg, fake: monzotest.New(monzotest.WithClock(time.Now)), stderr: &syncBuffer{}}
	if code, _ := w.command(nil, app.Bootstrap); code != app.ExitOK {
		t.Fatalf("bootstrap: %s", w.stderr.String())
	}
	if code, _ := w.command(nil, app.Migrate); code != app.ExitOK {
		t.Fatalf("migrate: %s", w.stderr.String())
	}
	cfg.OTLPEndpoint = sink.URL()
	return w, cfg
}

// runCLI runs one command function the way the binary does, with the build version set, and
// returns its exit code and what it wrote to standard error.
func runCLI(cfg config.Config, stdin io.Reader, fn func(ctx context.Context, e app.Env) int) (int, string) {
	var errBuf syncBuffer
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	code := fn(context.Background(), app.Env{Cfg: cfg, Stdin: stdin, Stdout: io.Discard, Stderr: &errBuf, Version: testVersion})
	return code, errBuf.String()
}

func spansNamed(spans []telemetrytest.Span, name string) []telemetrytest.Span {
	var out []telemetrytest.Span
	for _, s := range spans {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

// Every CLI command but bootstrap, healthcheck, version and help exports a root span, its logs
// and its metrics under its own identity: role cli, its command, a UUID of its own, and the build
// version. The environment a docker exec inherits cannot make it write serve's series.
func TestCLICommandsExportSpansAndLogsUnderTheirOwnIdentity(t *testing.T) {
	sink := telemetrytest.New(t)
	_, cfg := newTelemetryWorld(t, sink, nil)
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.instance.id=serve,service.version=env-version,team=ops")

	if code, errOut := runCLI(cfg, nil, func(ctx context.Context, e app.Env) int {
		return app.AccountAdd(ctx, e, app.AccountSpec{Slug: "bank-joint", Type: "joint", Currency: "GBP", Name: "made-up joint"})
	}); code != app.ExitOK {
		t.Fatalf("account add exit %d: %s", code, errOut)
	}
	broken := "date,type,amount,balance_after,description\n2026-10-01,PAY,10.00,10.00,made-up\n2026-10-02,PAY,10.00,25.00,made-up\n"
	importCode, importErr := runCLI(cfg, strings.NewReader(broken), func(ctx context.Context, e app.Env) int {
		return app.Import(ctx, e, "bank-joint", app.FormatManualCSV, "broken.csv")
	})
	if importCode != app.ExitRejected {
		t.Fatalf("import exit %d, want %d: %s", importCode, app.ExitRejected, importErr)
	}
	if code, errOut := runCLI(cfg, nil, app.AccountList); code != app.ExitOK {
		t.Fatalf("account list exit %d: %s", code, errOut)
	}

	spans := sink.Spans()
	ids := map[string]bool{}
	for name, exit := range map[string]int{"account add": 0, "import": app.ExitRejected, "account list": 0} {
		got := spansNamed(spans, semconv.SpanCommandPrefix+name)
		if len(got) != 1 {
			t.Fatalf("%d spans named %q (all: %d), want 1", len(got), semconv.SpanCommandPrefix+name, len(spans))
		}
		s := got[0]
		res := s.Resource
		if res[semconv.AttrProcessRole] != "cli" || res[semconv.AttrCommand] != name ||
			res[semconv.AttrServiceVersion] != testVersion || res[semconv.AttrServiceName] != "finstore" ||
			res[semconv.AttrDeploymentEnvironment] != "test" || res["team"] != "ops" {
			t.Errorf("%s: resource = %v", name, res)
		}
		id := res[semconv.AttrServiceInstanceID]
		if !uuidRE.MatchString(id) || id == semconv.ServiceName || id == "serve" || ids[id] {
			t.Errorf("%s: service.instance.id = %q, want a UUID of its own", name, id)
		}
		ids[id] = true
		if s.Attrs[semconv.SpanCommand] != name || s.Attrs[semconv.SpanProcessExitCode] != strconv.Itoa(exit) {
			t.Errorf("%s: attributes = %v, want the command and exit code %d", name, s.Attrs, exit)
		}
		if s.ParentSpanID != "" {
			t.Errorf("%s: the command span is not a root", name)
		}
		failed := s.StatusCode == 2
		if failed != (exit != 0) || (failed && s.StatusMessage != "error") {
			t.Errorf("%s: status %d %q for exit %d", name, s.StatusCode, s.StatusMessage, exit)
		}
	}

	// The import's warning reached OTLP with the command's trace id, and stderr carries it too.
	importSpan := spansNamed(spans, "finstore import")[0]
	var found bool
	for _, l := range sink.Logs() {
		if l.Body != "raw row skipped by the projection" {
			continue
		}
		found = true
		if l.TraceID != importSpan.TraceID || l.SpanID != importSpan.SpanID {
			t.Errorf("log trace %s span %s, want the import's %s %s", l.TraceID, l.SpanID, importSpan.TraceID, importSpan.SpanID)
		}
		if l.Resource[semconv.AttrServiceInstanceID] != importSpan.Resource[semconv.AttrServiceInstanceID] {
			t.Errorf("the log record has another process's instance id: %v", l.Resource)
		}
	}
	if !found {
		t.Fatalf("the import's warning never reached OTLP logs (%d records)", len(sink.Logs()))
	}
	if !strings.Contains(importErr, `"trace_id":"`+importSpan.TraceID+`"`) || !strings.Contains(importErr, "raw row skipped") {
		t.Errorf("stderr lacks the log line with its trace id: %s", importErr)
	}
}

// A command's telemetry never decides its outcome: settings that cannot export leave the command
// to run, with one line on stderr that does not repeat the setting.
func TestCLICommandRunsWhenTheOTLPSettingsAreBad(t *testing.T) {
	sink := telemetrytest.New(t)
	_, cfg := newTelemetryWorld(t, sink, nil)
	cfg.OTLPEndpoint = "collector.example.test"
	var out bytes.Buffer
	var errBuf syncBuffer
	code := app.AccountList(context.Background(), app.Env{Cfg: cfg, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errBuf, Version: testVersion})
	if code != app.ExitOK || !strings.Contains(out.String(), "SLUG") {
		t.Fatalf("account list exit %d, output %q, stderr %q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "finstore: telemetry disabled") || strings.Contains(errBuf.String(), "collector.example.test") {
		t.Errorf("stderr = %q, want the telemetry warning without the setting", errBuf.String())
	}
	if n := len(sink.Requests()); n != 0 {
		t.Errorf("the sink got %d requests", n)
	}
}

// Leak test 6: bootstrap executes CREATE ROLE with a password literal, so it never sets telemetry
// up, and neither does the container health check. Nothing reaches the collector.
func TestBootstrapAndHealthcheckExportNothing(t *testing.T) {
	sink := telemetrytest.New(t)
	super := testpg.Start(t)
	cfg, err := config.Load(func(k string) string {
		return map[string]string{
			config.EnvSuperuserDatabaseURL: super,
			config.EnvOwnerPassword:        "CANARY-OWNER-PASSWORD",
			config.EnvAppPassword:          "CANARY-APP-PASSWORD",
			config.EnvOTLPEndpoint:         sink.URL(),
			config.EnvListenAddr:           "127.0.0.1:1",
			config.EnvLogLevel:             "debug",
		}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // bootstrap is safe to run again
		if code, errOut := runCLI(cfg, nil, app.Bootstrap); code != app.ExitOK {
			t.Fatalf("bootstrap exit %d: %s", code, errOut)
		}
	}
	if code, _ := runCLI(cfg, nil, app.Healthcheck); code != app.ExitError {
		t.Fatalf("healthcheck against nothing exit %d, want %d", code, app.ExitError)
	}
	if reqs := sink.Requests(); len(reqs) != 0 {
		t.Fatalf("bootstrap and healthcheck sent %d requests to the collector: %v", len(reqs), sink.Paths())
	}
}

// serve exports under its fixed identity, whatever the environment says, and records when it
// started. /healthz makes no span; the status page does, named for its route.
func TestServeExportsItsFixedIdentityAndStartTime(t *testing.T) {
	sink := telemetrytest.New(t)
	_, cfg := newTelemetryWorld(t, sink, nil)
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.instance.id=other-instance,service.version=env-version")

	started := time.Now()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	listening := make(chan net.Addr, 1)
	done := make(chan int, 1)
	var stderr syncBuffer
	go func() {
		done <- app.Serve(ctx, app.Env{Cfg: cfg, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: &stderr, Version: testVersion},
			&app.Hooks{NoSchedule: true, Listening: func(a net.Addr) { listening <- a }})
	}()
	var addr string
	select {
	case a := <-listening:
		addr = a.String()
	case code := <-done:
		t.Fatalf("serve exited %d before listening: %s", code, stderr.String())
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not start listening")
	}
	get := func(path string, signedIn bool) int {
		req, err := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if signedIn {
			req.Header.Set("X-Auth-Request-Email", ownerEmail)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := get("/healthz", false); code != http.StatusOK {
		t.Fatalf("healthz %d", code)
	}
	if code := get("/", true); code != http.StatusOK {
		t.Fatalf("status page %d", code)
	}
	stop()
	select {
	case code := <-done:
		if code != app.ExitOK {
			t.Fatalf("serve exit %d: %s", code, stderr.String())
		}
	case <-time.After(60 * time.Second):
		t.Fatal("serve did not stop")
	}

	spans := sink.Spans()
	if len(spans) != 1 || spans[0].Name != "GET /{$}" || spans[0].Attrs[semconv.SpanHTTPRoute] != "/{$}" {
		t.Fatalf("spans = %+v, want only the status page's server span", spans)
	}
	res := spans[0].Resource
	if res[semconv.AttrServiceInstanceID] != "serve" || res[semconv.AttrServiceVersion] != testVersion ||
		res[semconv.AttrProcessRole] != "serve" || res[semconv.AttrServiceName] != "finstore" {
		t.Errorf("resource = %v", res)
	}
	if v, ok := res[semconv.AttrCommand]; ok {
		t.Errorf("serve has finstore.command = %q", v)
	}
	metrics := sink.Metrics()
	pts := metrics[semconv.MetricProcessStartTime].Points
	if len(pts) == 0 {
		t.Fatalf("no %s metric; have %d metrics", semconv.MetricProcessStartTime, len(metrics))
	}
	if at := time.Unix(int64(pts[len(pts)-1].Value), 0); at.Before(started.Add(-2*time.Second)) || at.After(time.Now().Add(time.Second)) {
		t.Errorf("start time %s, serve started about %s", at, started)
	}
	if metrics[semconv.MetricProcessStartTime].Resource[semconv.AttrServiceInstanceID] != "serve" {
		t.Errorf("start time metric has another instance id: %v", metrics[semconv.MetricProcessStartTime].Resource)
	}
	if len(metrics["http.server.request.duration"].Points) == 0 {
		t.Error("no http.server.request.duration points")
	}
	for _, l := range sink.Logs() {
		if l.Resource[semconv.AttrServiceInstanceID] != "serve" {
			t.Errorf("a log record carries instance id %q", l.Resource[semconv.AttrServiceInstanceID])
		}
	}
}
