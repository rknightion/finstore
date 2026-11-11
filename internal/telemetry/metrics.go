package telemetry

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

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
	if err != nil {
		return nil, err
	}
	return &m, nil
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
