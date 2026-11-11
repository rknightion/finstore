package telemetry

import (
	"context"
	"fmt"
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
	syncLastSuccess metric.Int64Gauge
	syncRuns        metric.Int64Counter
	bankRequests    metric.Int64Counter
	consentState    metric.Int64Gauge
	consentExpiry   metric.Int64Gauge
	mismatch        metric.Int64Gauge
	importLast      metric.Int64Gauge
	dumpLastSuccess metric.Int64Gauge
	up              metric.Int64Gauge
	unresolved      metric.Int64Gauge
	optionalReads   metric.Int64Counter
	cardsUnlisted   metric.Int64Gauge
	processStart    metric.Int64Gauge
	jobRuns         metric.Int64Counter
	jobDuration     metric.Float64Histogram
	jobLastOK       metric.Int64Gauge
	bank            bankInstruments
}

// NewMetrics creates every instrument on meter.
func NewMetrics(meter metric.Meter) (*Metrics, error) {
	var m Metrics
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
	if err == nil {
		m.jobDuration, err = meter.Float64Histogram(semconv.MetricSchedulerJobDuration,
			metric.WithUnit("s"), metric.WithDescription("Scheduler job run duration, by job"))
		if err != nil {
			err = fmt.Errorf("create %s: %w", semconv.MetricSchedulerJobDuration, err)
		}
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
// scheduler.job with the job's registered name and outcome, and is counted, timed, and recorded
// as the job's last outcome. A nil tr makes no spans. The job name is a registered name, never
// data; the outcome is ok, error or panic, and neither a returned error nor a panic value is
// recorded.
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
