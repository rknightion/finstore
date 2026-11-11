package telemetry

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/rknightion/finstore/internal/bankhttp"
	"github.com/rknightion/finstore/internal/semconv"
)

// bankInstruments are the bank call instruments the bankhttp client's observer records: the
// duration of every attempt and the count of retries. finstore.bank.requests, the count of
// requests, is recorded by the workers and is unchanged.
type bankInstruments struct {
	duration metric.Float64Histogram
	retries  metric.Int64Counter
}

func newBankInstruments(meter metric.Meter) (bankInstruments, error) {
	var b bankInstruments
	var err error
	b.duration, err = meter.Float64Histogram(semconv.MetricBankRequestDuration, metric.WithUnit("s"),
		metric.WithDescription("Duration of one bank attempt, by source, endpoint and status class"))
	if err != nil {
		return b, fmt.Errorf("create %s: %w", semconv.MetricBankRequestDuration, err)
	}
	b.retries, err = meter.Int64Counter(semconv.MetricBankRetries,
		metric.WithDescription("Bank calls repeated, by source, endpoint and reason"))
	if err != nil {
		return b, fmt.Errorf("create %s: %w", semconv.MetricBankRetries, err)
	}
	return b, nil
}

// BankObserver returns the observer a bankhttp client for source reports each attempt and retry
// to. An attempt with no HTTP response arrives with the status class "no_response".
func (m *Metrics) BankObserver(source string) bankhttp.Observer {
	return bankObserver{m: m, source: source}
}

type bankObserver struct {
	m      *Metrics
	source string
}

func (o bankObserver) Attempt(ctx context.Context, endpoint, statusClass string, d time.Duration) {
	o.m.bank.duration.Record(ctx, d.Seconds(), metric.WithAttributes(
		attribute.String(semconv.AttrSource, o.source),
		attribute.String(semconv.AttrEndpoint, endpoint),
		attribute.String(semconv.AttrStatusClass, statusClass)))
}

func (o bankObserver) Retry(ctx context.Context, endpoint, reason string) {
	o.m.bank.retries.Add(ctx, 1, metric.WithAttributes(
		attribute.String(semconv.AttrSource, o.source),
		attribute.String(semconv.AttrEndpoint, endpoint),
		attribute.String(semconv.AttrReason, reason)))
}
