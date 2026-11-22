package telemetry

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/rknightion/finstore/internal/scheduler"
	"github.com/rknightion/finstore/internal/semconv"
)

// Metrics holds the service's instruments. Other packages record through its
// methods and never see an instrument name.
type Metrics struct {
	meter            metric.Meter
	syncLastSuccess  metric.Int64Gauge
	syncRuns         metric.Int64Counter
	bankRequests     metric.Int64Counter
	consentState     metric.Int64Gauge
	consentExpiry    metric.Int64Gauge
	mismatch         metric.Int64Gauge
	importLast       metric.Int64Gauge
	dumpLastSuccess  metric.Int64Gauge
	up               metric.Int64Gauge
	unresolved       metric.Int64Gauge
	optionalReads    metric.Int64Counter
	cardsUnlisted    metric.Int64Gauge
	processStart     metric.Int64Gauge
	jobRuns          metric.Int64Counter
	jobDuration      metric.Float64Histogram
	jobLastOK        metric.Int64Gauge
	syncRunDuration  metric.Float64Histogram
	lastRunOK        metric.Int64Gauge
	lastRunTime      metric.Int64Gauge
	lastRateLimited  metric.Int64Gauge
	mandatesLastRead metric.Int64Gauge
	unanswered       metric.Int64Gauge
	// history is the source-to-value snapshot the historyOK observable gauge reports at each
	// collection; a source left out of the snapshot has no series.
	history      historySnapshot
	mcpToolCalls metric.Int64Counter
	bank         bankInstruments
}

// NewMetrics creates every instrument on meter.
func NewMetrics(meter metric.Meter) (*Metrics, error) {
	m := Metrics{meter: meter}
	var err error
	gauge := func(dst *metric.Int64Gauge, name, unit, desc string) {
		if err != nil {
			return
		}
		*dst, err = meter.Int64Gauge(name, metric.WithUnit(unit), metric.WithDescription(desc))
		if err != nil {
			err = fmt.Errorf("create %s: %w", name, err)
		}
	}
	counter := func(dst *metric.Int64Counter, name, desc string) {
		if err != nil {
			return
		}
		*dst, err = meter.Int64Counter(name, metric.WithDescription(desc))
		if err != nil {
			err = fmt.Errorf("create %s: %w", name, err)
		}
	}
	gauge(&m.syncLastSuccess, semconv.MetricSyncLastSuccessTime, "s", "Unix time of the last successful scheduled sync")
	counter(&m.syncRuns, semconv.MetricSyncRuns, "Sync runs finished")
	counter(&m.bankRequests, semconv.MetricBankRequests, "Bank requests made")
	gauge(&m.consentState, semconv.MetricConsentState, "1", "1 for the current consent state, 0 for the others")
	gauge(&m.consentExpiry, semconv.MetricConsentExpiryTime, "s", "Unix time the consent is expected to end")
	gauge(&m.mismatch, semconv.MetricReconciliationMismatch, "1", "1 while the last reconciliation failed or the account has a file gap")
	gauge(&m.importLast, semconv.MetricImportLastTime, "s", "Unix time of the newest statement date imported")
	gauge(&m.dumpLastSuccess, semconv.MetricDumpLastSuccessTime, "s", "Unix time of the last successful dump")
	gauge(&m.up, semconv.MetricUp, "1", "1 while the service is running")
	gauge(&m.unresolved, semconv.MetricMandatesUnresolved, "1", "Mandates whose account is not known")
	counter(&m.optionalReads, semconv.MetricTrueLayerOptionalReads, "TrueLayer reads whose answer never reaches the ledger, by endpoint and result")
	gauge(&m.cardsUnlisted, semconv.MetricTrueLayerCardsUnlisted, "1", "Card accounts the newest card list left out")
	gauge(&m.processStart, semconv.MetricProcessStartTime, "s", "Unix time serve started")
	counter(&m.jobRuns, semconv.MetricSchedulerJobRuns, "Scheduler job runs, by job and outcome (ok, error or panic)")
	gauge(&m.jobLastOK, semconv.MetricSchedulerJobLastOK, "1", "1 when the job's last run ended ok, 0 otherwise; held in the serve process, so absent after a restart until the job has run")
	gauge(&m.lastRunOK, semconv.MetricSyncLastRunOK, "1", "1 when the newest finished run of a source and kind ended ok, 0 when it ended partial or failed")
	gauge(&m.lastRunTime, semconv.MetricSyncLastRunTime, "s", "Unix time the newest finished run of a source and kind ended")
	gauge(&m.lastRateLimited, semconv.MetricSyncLastRateLimitedTime, "s", "Unix time the newest rate-limited run of a source ended; absent for a source that never had one")
	gauge(&m.mandatesLastRead, semconv.MetricTrueLayerMandatesLastReadTime, "s", "Unix time a TrueLayer connection last read its standing orders or direct debits successfully; absent for a connection that never has")
	gauge(&m.unanswered, semconv.MetricConnectUnanswered, "1", "Connect attempts of a source that expired unused in the last 24 hours")
	counter(&m.mcpToolCalls, semconv.MetricMCPToolCalls, "MCP tool calls, by tool, outcome (ok, invalid, busy, timeout, refused or error) and client (the grant group of the caller)")
	if err == nil {
		m.syncRunDuration, err = meter.Float64Histogram(semconv.MetricSyncRunDuration,
			metric.WithUnit("s"), metric.WithDescription("Sync run duration, by source, kind and outcome"))
		if err != nil {
			err = fmt.Errorf("create %s: %w", semconv.MetricSyncRunDuration, err)
		}
	}
	if err == nil {
		m.jobDuration, err = meter.Float64Histogram(semconv.MetricSchedulerJobDuration,
			metric.WithUnit("s"), metric.WithDescription("Scheduler job run duration, by job"))
		if err != nil {
			err = fmt.Errorf("create %s: %w", semconv.MetricSchedulerJobDuration, err)
		}
	}
	if err == nil {
		err = m.observeConsentHistory()
	}
	if err == nil {
		m.bank, err = newBankInstruments(meter)
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// SchedulerObserver returns the scheduler's observer. Each job run is a new root span named
// scheduler.job with the job's registered name and outcome, and is counted and timed. Its outcome is
// recorded as the job's last outcome unless it is cancelled (the job returned an error after the
// scheduler was told to stop): a cancelled run is counted and timed, is not a failed span, and
// does not write last_ok, so a deploy cannot trip the Scheduled Job Failed alert. A nil tr makes
// no spans. The job name is a registered name, never data; the outcome is ok, error, panic or
// cancelled, and neither a returned error nor a panic value is recorded.
func (m *Metrics) SchedulerObserver(tr trace.Tracer) scheduler.Observer {
	if tr == nil {
		tr = tracenoop.NewTracerProvider().Tracer(semconv.ServiceName)
	}
	return &schedulerObserver{m: m, tr: tr}
}

type schedulerObserver struct {
	m  *Metrics
	tr trace.Tracer
}

// Start begins a new root span for one run of job. The returned finish records the run's outcome
// on the span and in the metrics, in that order, then ends the span: metrics recorded inside the
// span's context carry it as their exemplar.
func (o *schedulerObserver) Start(ctx context.Context, job string) (context.Context, func(string)) {
	ctx, span := o.tr.Start(ctx, semconv.SpanSchedulerJob,
		trace.WithNewRoot(),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attribute.String(semconv.SpanJob, job)))
	began := time.Now()
	return ctx, func(outcome string) {
		span.SetAttributes(attribute.String(semconv.SpanJobOutcome, outcome))
		// A run cancelled by shutdown is not a failure: no error status and no last_ok write, so a
		// deploy cannot trip the Scheduled Job Failed alert.
		failed := outcome != semconv.JobOK && outcome != semconv.JobCancelled
		if failed {
			Fail(span, "error")
		}
		attr := metric.WithAttributes(attribute.String(semconv.AttrJob, job))
		o.m.jobRuns.Add(ctx, 1, metric.WithAttributes(
			attribute.String(semconv.AttrJob, job), attribute.String(semconv.AttrOutcome, outcome)))
		o.m.jobDuration.Record(ctx, time.Since(began).Seconds(), attr)
		if outcome != semconv.JobCancelled {
			var ok int64
			if outcome == semconv.JobOK {
				ok = 1
			}
			o.m.jobLastOK.Record(ctx, ok, attr)
		}
		span.End()
	}
}

// SyncSucceeded records the time of the last successful scheduled sync.
func (m *Metrics) SyncSucceeded(ctx context.Context, source string, at time.Time) {
	m.syncLastSuccess.Record(ctx, at.Unix(), metric.WithAttributes(attribute.String(semconv.AttrSource, source)))
}

// SyncRun counts a finished run.
func (m *Metrics) SyncRun(ctx context.Context, source, kind, outcome string) {
	m.syncRuns.Add(ctx, 1, metric.WithAttributes(
		attribute.String(semconv.AttrSource, source),
		attribute.String(semconv.AttrKind, kind),
		attribute.String(semconv.AttrOutcome, outcome)))
}

// SyncRunWithCategory counts a finished run as SyncRun does, adding its error category as
// error_category when it has one, so runs that ended for one reason (rate_limited, say) can be
// counted.
func (m *Metrics) SyncRunWithCategory(ctx context.Context, source, kind, outcome, category string) {
	attrs := []attribute.KeyValue{
		attribute.String(semconv.AttrSource, source),
		attribute.String(semconv.AttrKind, kind),
		attribute.String(semconv.AttrOutcome, outcome),
	}
	if category != "" {
		attrs = append(attrs, attribute.String(semconv.AttrErrorCategory, category))
	}
	m.syncRuns.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// LastRun records the newest finished run of a source and kind: whether it ended ok (1) or
// partial or failed (0), and when it finished.
func (m *Metrics) LastRun(ctx context.Context, source, kind string, ok bool, finishedAt time.Time) {
	attrs := metric.WithAttributes(attribute.String(semconv.AttrSource, source), attribute.String(semconv.AttrKind, kind))
	var v int64
	if ok {
		v = 1
	}
	m.lastRunOK.Record(ctx, v, attrs)
	m.lastRunTime.Record(ctx, finishedAt.Unix(), attrs)
}

// LastRateLimited records when a source's newest rate-limited run finished. The caller records it
// only for a source that has had one.
func (m *Metrics) LastRateLimited(ctx context.Context, source string, at time.Time) {
	m.lastRateLimited.Record(ctx, at.Unix(), metric.WithAttributes(attribute.String(semconv.AttrSource, source)))
}

// MandatesLastRead records when a TrueLayer connection last read its mandates successfully. The
// caller records it only for a connection that has.
func (m *Metrics) MandatesLastRead(ctx context.Context, source string, at time.Time) {
	m.mandatesLastRead.Record(ctx, at.Unix(), metric.WithAttributes(attribute.String(semconv.AttrSource, source)))
}

// historySnapshot is what the consent history gauge reports: a value for each source in it.
type historySnapshot struct {
	mu   sync.Mutex
	vals map[string]int64
}

func (h *historySnapshot) set(source string, v int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.vals == nil {
		h.vals = map[string]int64{}
	}
	h.vals[source] = v
}

func (h *historySnapshot) replace(vals map[string]int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.vals = vals
}

func (h *historySnapshot) copy() map[string]int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return maps.Clone(h.vals)
}

// observeConsentHistory registers the consent history gauge as an observable gauge that reports
// the snapshot at each collection. A synchronous gauge keeps every series it has ever recorded for
// as long as the process runs (the SDK's last-value aggregation never forgets one), so a source
// whose consent was lost, expired or replaced would keep reading its last value, and a 0 would
// keep History Missing firing beside Consent Lost. An observable gauge reports only what its
// callback observes, so a source left out of the snapshot has no series, the same way the writer
// lock gauge has none while the lock is free.
func (m *Metrics) observeConsentHistory() error {
	_, err := m.meter.Int64ObservableGauge(semconv.MetricConsentHistoryOK, metric.WithUnit("1"),
		metric.WithDescription("1 when a connect or backfill run, or a capture run that stored at least one response, ended ok since the source's current consent was approved, 0 when none has; absent while the source has no active current consent"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			for source, v := range m.history.copy() {
				o.Observe(v, metric.WithAttributes(attribute.String(semconv.AttrSource, source)))
			}
			return nil
		}))
	if err != nil {
		return fmt.Errorf("create %s: %w", semconv.MetricConsentHistoryOK, err)
	}
	return nil
}

// ConsentHistory sets whether a source's current consent has its history: 1 when a connect or
// backfill run, or a capture run that stored at least one response, ended ok since the approval, 0
// when none has. It leaves every other source as it was; the status job uses ReplaceConsentHistory,
// which also drops the sources that are no longer in the query.
func (m *Metrics) ConsentHistory(_ context.Context, source string, ok bool) {
	var v int64
	if ok {
		v = 1
	}
	m.history.set(source, v)
}

// ReplaceConsentHistory makes sources (source name to whether its current consent has its history,
// see ConsentHistory) the whole set the gauge reports: a source that is not in it, because its
// consent is no longer active or current, has no series from the next collection on.
func (m *Metrics) ReplaceConsentHistory(sources map[string]bool) {
	vals := make(map[string]int64, len(sources))
	for source, ok := range sources {
		var v int64
		if ok {
			v = 1
		}
		vals[source] = v
	}
	m.history.replace(vals)
}

// ConnectsUnanswered records how many of a source's connect attempts expired unused in the last
// 24 hours.
func (m *Metrics) ConnectsUnanswered(ctx context.Context, source string, n int) {
	m.unanswered.Record(ctx, int64(n), metric.WithAttributes(attribute.String(semconv.AttrSource, source)))
}

// MCPToolCall counts one MCP tool call. tool is one of semconv's MCPTool values (a name the caller
// chose that is not one of them is semconv.MCPToolUnknown), outcome one of its MCPOutcome values,
// and client the grant group of the caller. The three are the only attributes: nothing the call
// asked for or read is ever a metric dimension.
func (m *Metrics) MCPToolCall(ctx context.Context, tool, outcome, client string) {
	m.mcpToolCalls.Add(ctx, 1, metric.WithAttributes(
		attribute.String(semconv.AttrTool, tool),
		attribute.String(semconv.AttrOutcome, outcome),
		attribute.String(semconv.AttrClient, client)))
}

// MandatesUnresolved records how many of a source's mandates have no account.
func (m *Metrics) MandatesUnresolved(ctx context.Context, source string, n int) {
	m.unresolved.Record(ctx, int64(n), metric.WithAttributes(attribute.String(semconv.AttrSource, source)))
}

// OptionalRead counts an optional read (one whose answer never reaches the ledger) by its result:
// ok, unsupported, refused, rate_limited or failed.
func (m *Metrics) OptionalRead(ctx context.Context, source, endpoint, result string) {
	m.optionalReads.Add(ctx, 1, metric.WithAttributes(
		attribute.String(semconv.AttrSource, source),
		attribute.String(semconv.AttrEndpoint, endpoint),
		attribute.String(semconv.AttrResult, result)))
}

// CardsUnlisted records how many card accounts of a source its newest card list left out.
func (m *Metrics) CardsUnlisted(ctx context.Context, source string, n int) {
	m.cardsUnlisted.Record(ctx, int64(n), metric.WithAttributes(attribute.String(semconv.AttrSource, source)))
}

// BankRequest counts a request made to a bank.
func (m *Metrics) BankRequest(ctx context.Context, source, endpoint, statusClass string) {
	m.bankRequests.Add(ctx, 1, metric.WithAttributes(
		attribute.String(semconv.AttrSource, source),
		attribute.String(semconv.AttrEndpoint, endpoint),
		attribute.String(semconv.AttrStatusClass, statusClass)))
}

// ConsentState records 1 for current and 0 for every other state in states.
func (m *Metrics) ConsentState(ctx context.Context, source, current string, states []string) {
	for _, s := range states {
		var v int64
		if s == current {
			v = 1
		}
		m.consentState.Record(ctx, v, metric.WithAttributes(
			attribute.String(semconv.AttrSource, source),
			attribute.String(semconv.AttrState, s)))
	}
}

// ConsentExpiry records when the consent is expected to end.
func (m *Metrics) ConsentExpiry(ctx context.Context, source string, at time.Time) {
	m.consentExpiry.Record(ctx, at.Unix(), metric.WithAttributes(attribute.String(semconv.AttrSource, source)))
}

// ReconciliationMismatch records whether the account's last check failed or it
// has a file gap.
func (m *Metrics) ReconciliationMismatch(ctx context.Context, account string, mismatch bool) {
	var v int64
	if mismatch {
		v = 1
	}
	m.mismatch.Record(ctx, v, metric.WithAttributes(attribute.String(semconv.AttrAccount, account)))
}

// ImportLast records the newest statement date imported for an account.
func (m *Metrics) ImportLast(ctx context.Context, account string, at time.Time) {
	m.importLast.Record(ctx, at.Unix(), metric.WithAttributes(attribute.String(semconv.AttrAccount, account)))
}

// DumpSucceeded records the time from the dump status file.
func (m *Metrics) DumpSucceeded(ctx context.Context, at time.Time) {
	m.dumpLastSuccess.Record(ctx, at.Unix())
}

// Up records the liveness gauge as 1. The caller sends it every minute.
func (m *Metrics) Up(ctx context.Context) { m.up.Record(ctx, 1) }

// ProcessStarted records when serve started, as Unix seconds. The gauge keeps its last value
// across collections, so a change in the value is a restart.
func (m *Metrics) ProcessStarted(ctx context.Context, at time.Time) {
	m.processStart.Record(ctx, at.Unix())
}

// ObserveWriterLock registers the writer-lock gauge. At each collection it calls hold, which says
// who holds the lock and since when (store.CurrentHold), and reports the seconds held so far under
// the holder's name. While nobody holds the lock it reports nothing, so a stuck hold is a rising
// series and a free lock is no series at all.
func (m *Metrics) ObserveWriterLock(hold func() (holder string, since time.Time, ok bool)) error {
	g, err := m.meter.Float64ObservableGauge(semconv.MetricWriterLockHeld, metric.WithUnit("s"),
		metric.WithDescription("Seconds the writer lock has been held, by holder; absent while it is free"))
	if err != nil {
		return fmt.Errorf("create %s: %w", semconv.MetricWriterLockHeld, err)
	}
	_, err = m.meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		if holder, since, ok := hold(); ok {
			o.ObserveFloat64(g, time.Since(since).Seconds(),
				metric.WithAttributes(attribute.String(semconv.AttrHolder, holder)))
		}
		return nil
	}, g)
	if err != nil {
		return fmt.Errorf("register %s: %w", semconv.MetricWriterLockHeld, err)
	}
	return nil
}

// selfMetrics counts the telemetry pipeline's own trouble: failed exports and span attributes
// the allow-list removed. Setup builds it before the meter provider exists (the metric exporter
// it wraps is an input to that provider), so it is bound to a meter afterwards; until then it
// counts nothing.
type selfMetrics struct {
	ins atomic.Pointer[selfInstruments]
}

type selfInstruments struct {
	failures metric.Int64Counter
	dropped  metric.Int64Counter
}

// newSelfMetrics creates the instruments on meter.
func newSelfMetrics(meter metric.Meter) (*selfMetrics, error) {
	s := &selfMetrics{}
	return s, s.bind(meter)
}

func (s *selfMetrics) bind(meter metric.Meter) error {
	failures, err := meter.Int64Counter(semconv.MetricTelemetryExportFailures,
		metric.WithDescription("Telemetry exports that failed, by signal"))
	if err != nil {
		return fmt.Errorf("create %s: %w", semconv.MetricTelemetryExportFailures, err)
	}
	dropped, err := meter.Int64Counter(semconv.MetricTelemetrySpanAttributesDropped,
		metric.WithDescription("Span attributes the exporter allow-list removed, by key"))
	if err != nil {
		return fmt.Errorf("create %s: %w", semconv.MetricTelemetrySpanAttributesDropped, err)
	}
	s.ins.Store(&selfInstruments{failures: failures, dropped: dropped})
	return nil
}

// SpanAttributeDropped counts a span attribute key the exporter removed.
func (s *selfMetrics) SpanAttributeDropped(ctx context.Context, key string) {
	if ins := s.ins.Load(); ins != nil {
		ins.dropped.Add(ctx, 1, metric.WithAttributes(attribute.String(semconv.AttrKey, key)))
	}
}

// ExportFailed counts a failed export of signal (traces, metrics or logs).
func (s *selfMetrics) ExportFailed(ctx context.Context, signal string) {
	if ins := s.ins.Load(); ins != nil {
		ins.failures.Add(ctx, 1, metric.WithAttributes(attribute.String(semconv.AttrSignal, signal)))
	}
}
