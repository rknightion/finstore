package app_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/app"
	"github.com/rknightion/finstore/internal/monzo"
	"github.com/rknightion/finstore/internal/monzotest"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/telemetry/telemetrytest"
)

// serve's wiring of the connect and callback spans and the scheduler's job telemetry, through the
// real exporters into a stand-in collector: a connect post, a callback with a canary code and
// state that the service refuses, and the real callback with a canary code, whose state is the
// service's own. No code or state, canary or real, may appear anywhere an exporter sent.
func TestServeExportsConnectCallbackAndSchedulerTelemetry(t *testing.T) {
	const canaryCode, canaryState = "CANARYCODE-5d1f", "CANARYSTATE-0b9c"
	sink := telemetrytest.New(t)
	w, cfg := newTelemetryWorld(t, sink, nil)
	srv := w.fake.Start()
	t.Cleanup(srv.Close)
	cfg.MonzoAPIURL = srv.URL
	// An open account, so the history fetch has a range to fetch.
	w.fake.AddAccount(monzotest.Account{ID: "acc_test_1", Type: "uk_retail", Created: time.Now().AddDate(0, -3, 0)})
	client := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	// A connect attempt past its time: the status job marks it expired when it runs at start.
	w.queryInt(`INSERT INTO sync.connect_attempt (source, state_sha256, started_at, expires_at)
		VALUES ('monzo', '\x00', now() - interval '2 hours', now() - interval '1 hour') RETURNING id`)

	var approve sync.Once
	var worker *monzo.Worker
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	listening := make(chan net.Addr, 1)
	done := make(chan int, 1)
	var stderr syncBuffer
	go func() {
		done <- app.Serve(ctx, app.Env{Cfg: cfg, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: &stderr, Version: testVersion, Now: w.fake.Now},
			&app.Hooks{
				Listening: func(a net.Addr) { listening <- a },
				Worker:    func(mw *monzo.Worker) { worker = mw },
				MonzoOptions: func(o *monzo.Options) {
					o.Sleep = func(ctx context.Context, _ time.Duration) error {
						approve.Do(w.fake.Approve)
						return ctx.Err()
					}
				},
			})
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
	do := func(method, path string, header map[string]string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, "http://"+addr+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range header {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := do(http.MethodPost, "/connect/monzo", map[string]string{"X-Auth-Request-Email": ownerEmail, "Sec-Fetch-Site": "same-origin"})
	bank, err := url.Parse(continueLink(t, resp))
	if err != nil {
		t.Fatal(err)
	}
	realState := bank.Query().Get("state")
	if realState == "" {
		t.Fatal("the connect page carries no state")
	}
	if resp := do(http.MethodGet, "/callback/monzo?code="+canaryCode+"&state="+canaryState, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("callback with a forged state: %d, want 400", resp.StatusCode)
	}
	w.fake.RegisterCode(canaryCode)
	if resp := do(http.MethodGet, "/callback/monzo?code="+canaryCode+"&state="+url.QueryEscape(realState), nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("callback: %d, want 303: %s", resp.StatusCode, stderr.String())
	}
	worker.Wait()

	// The status job runs at start and marks the attempt past its time as expired; seeing that
	// means the run has begun, and serve's shutdown waits for a running job before the final flush.
	deadline := time.Now().Add(20 * time.Second)
	for w.queryInt(`SELECT count(*) FROM sync.connect_attempt WHERE outcome = 'expired'`) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("the status job did not run within 20 s: %s", stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
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
	byName := map[string][]telemetrytest.Span{}
	for _, s := range spans {
		// The database driver's own CLIENT spans (its "connect" among them) are not finstore's.
		if s.Attrs[semconv.SpanDBSystemName] != "" {
			continue
		}
		byName[s.Name] = append(byName[s.Name], s)
	}
	connects := byName[semconv.SpanConnect]
	if len(connects) != 1 || connects[0].Attrs[semconv.SpanConnectOutcome] != "started" || connects[0].Attrs[semconv.SpanSource] != "monzo" {
		t.Fatalf("connect spans = %+v", connects)
	}
	callbacks := byName[semconv.SpanCallback]
	if len(callbacks) != 2 {
		t.Fatalf("%d callback spans, want 2: %+v", len(callbacks), callbacks)
	}
	outcomes := map[string]bool{}
	for _, c := range callbacks {
		outcomes[c.Attrs[semconv.SpanConnectOutcome]] = true
		if c.Attrs[semconv.SpanSource] != "monzo" || c.ParentSpanID == "" {
			t.Errorf("callback span = %+v, want source monzo under a server span", c)
		}
	}
	if !outcomes["refused"] || !outcomes["exchanged"] {
		t.Errorf("callback outcomes = %v, want refused and exchanged", outcomes)
	}

	// The history fetch the exchanged callback started is its own trace, linked to the callback,
	// and each range it fetched is a root linked to that run.
	var exchanged telemetrytest.Span
	for _, c := range callbacks {
		if c.Attrs[semconv.SpanConnectOutcome] == "exchanged" {
			exchanged = c
		}
	}
	var backfill *telemetrytest.Span
	for i, s := range byName[semconv.SpanSyncRun] {
		if s.Attrs[semconv.SpanRunKind] == "backfill" {
			backfill = &byName[semconv.SpanSyncRun][i]
		}
	}
	if backfill == nil {
		t.Fatalf("no backfill sync.run span in %+v", byName[semconv.SpanSyncRun])
	}
	if backfill.ParentSpanID != "" || backfill.TraceID == exchanged.TraceID ||
		len(backfill.LinkTraceIDs) != 1 || backfill.LinkTraceIDs[0] != exchanged.TraceID {
		t.Errorf("backfill sync.run = %+v, want a root in its own trace linked to the callback trace %s", *backfill, exchanged.TraceID)
	}
	if backfill.Attrs[semconv.SpanSource] != "monzo" || backfill.Attrs[semconv.SpanRunID] == "" || backfill.Attrs[semconv.SpanRunOutcome] == "" {
		t.Errorf("backfill sync.run attributes = %v", backfill.Attrs)
	}
	ranges := byName[semconv.SpanMonzoFetchRange]
	if len(ranges) == 0 {
		t.Error("the backfill exported no monzo.fetch_range span")
	}
	for _, r := range ranges {
		if r.ParentSpanID != "" || len(r.LinkTraceIDs) != 1 || r.LinkTraceIDs[0] != backfill.TraceID {
			t.Errorf("monzo.fetch_range = %+v, want a root linked to the backfill run's trace", r)
		}
	}

	jobs := byName[semconv.SpanSchedulerJob]
	var status *telemetrytest.Span
	for i := range jobs {
		if jobs[i].Attrs[semconv.SpanJob] == "status" {
			status = &jobs[i]
		}
		if jobs[i].ParentSpanID != "" {
			t.Errorf("scheduler.job span has a parent: %+v", jobs[i])
		}
	}
	if status == nil || status.Attrs[semconv.SpanJobOutcome] != "ok" {
		t.Fatalf("no ok status job span in %+v", jobs)
	}
	metrics := sink.Metrics()
	var runs float64
	for _, p := range metrics[semconv.MetricSchedulerJobRuns].Points {
		if p.Attrs[semconv.AttrJob] == "status" && p.Attrs[semconv.AttrOutcome] == "ok" && p.Value > runs {
			runs = p.Value
		}
	}
	if runs < 1 {
		t.Errorf("%s{job=status,outcome=ok} = %v, want at least 1; points %+v", semconv.MetricSchedulerJobRuns, runs, metrics[semconv.MetricSchedulerJobRuns].Points)
	}
	if len(metrics[semconv.MetricSchedulerJobDuration].Points) == 0 {
		t.Errorf("no %s points", semconv.MetricSchedulerJobDuration)
	}
	// Other jobs (the hourly sync, after its jitter) may add points of their own: read the status
	// job's, the last one it exported.
	var statusLastOK []float64
	for _, p := range metrics[semconv.MetricSchedulerJobLastOK].Points {
		if p.Attrs[semconv.AttrJob] == "status" {
			statusLastOK = append(statusLastOK, p.Value)
		}
	}
	if len(statusLastOK) == 0 || statusLastOK[len(statusLastOK)-1] != 1 {
		t.Errorf("%s{job=status} = %v, want 1; all points %+v", semconv.MetricSchedulerJobLastOK, statusLastOK,
			metrics[semconv.MetricSchedulerJobLastOK].Points)
	}

	// The expired attempt the status job retired is one unanswered connect for monzo, and the
	// run state it publishes carries the same source label the alerts select on.
	unanswered := false
	for _, p := range metrics[semconv.MetricConnectUnanswered].Points {
		if p.Attrs[semconv.AttrSource] == "monzo" && p.Value == 1 {
			unanswered = true
		}
	}
	if !unanswered {
		t.Errorf("%s{source=monzo} = 1 not exported; points %+v", semconv.MetricConnectUnanswered, metrics[semconv.MetricConnectUnanswered].Points)
	}

	// Nothing a callback carried left the process: not in the decoded signals, and not in a raw
	// request body either.
	// The connect post carried the signed-in address in the identity header: that is leak test 2.
	secrets := []string{canaryCode, canaryState, realState, "CANARY", ownerEmail}
	decoded := fmt.Sprintf("%+v\n%+v\n%+v", spans, sink.Logs(), metrics)
	for _, secret := range secrets {
		if strings.Contains(decoded, secret) {
			t.Errorf("%q reached an exported span, log or metric", secret)
		}
		for _, r := range sink.Requests() {
			if bytes.Contains(r.Body, []byte(secret)) {
				t.Errorf("%q is in a raw export to %s", secret, r.Path)
			}
		}
	}
	if len(sink.Logs()) == 0 {
		t.Error("no log records exported: the leak check proved nothing about logs")
	}
}
