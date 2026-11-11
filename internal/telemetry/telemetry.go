package telemetry

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/rknightion/finstore/internal/config"
	"github.com/rknightion/finstore/internal/semconv"
)

// Role is what kind of process this is.
type Role string

const (
	// RoleServe is the long-running service: HTTP server, scheduler and workers.
	RoleServe Role = "serve"
	// RoleCLI is a one-off command.
	RoleCLI Role = "cli"
	// ServeInstanceID is serve's service.instance.id. One serve runs per deployment environment,
	// so its series keep their labels across restarts. It is a constant in code, not a setting:
	// docker exec inherits the container's environment, and a CLI process must never write
	// serve's series.
	ServeInstanceID = "serve"
)

// Identity says who a process is. Its values win over OTEL_RESOURCE_ATTRIBUTES, which can only
// add attributes.
type Identity struct {
	Role Role
	// Command is the CLI command, such as "account add". It is empty for serve.
	Command string
	// Version is service.version, such as "main-659f3b6".
	Version string
	// InstanceID is service.instance.id: ServeInstanceID for serve, a new UUID per CLI process.
	// Empty means that default.
	InstanceID string
}

// Providers owns the trace, metric and log pipelines. With no OTLP endpoint configured, or with
// the SDK disabled, it is a working no-op that exports nothing.
type Providers struct {
	// Meter creates instruments; pass it to NewMetrics.
	Meter metric.Meter
	// LogHandler forwards records to OTLP logs. It does no filtering: give it
	// to NewLogger as an extra handler so the allow-list applies to it.
	LogHandler slog.Handler
	// MeterProvider is the provider Meter came from.
	MeterProvider metric.MeterProvider
	// TracerProvider makes spans. Finstore code takes its tracers from Tracer, never from the
	// global provider.
	TracerProvider trace.TracerProvider

	shutdown []func(context.Context) error
	flush    []func(context.Context) error
}

// Tracer returns a tracer from the providers' tracer provider.
func (p *Providers) Tracer(name string) trace.Tracer { return p.TracerProvider.Tracer(name) }

// ForceFlush exports whatever each pipeline holds. It is a no-op without an endpoint.
func (p *Providers) ForceFlush(ctx context.Context) error {
	var errs []error
	for _, f := range p.flush {
		errs = append(errs, f(ctx))
	}
	return errors.Join(errs...)
}

// Shutdown flushes and stops the exporters in the order tracer, logger, meter, so that a failure
// the first two cause is counted in the metrics the last one sends. It is a no-op without an
// endpoint. Callers pass a context with their own budget: serve 20 s, a CLI command 5 s.
func (p *Providers) Shutdown(ctx context.Context) error {
	var errs []error
	for _, f := range p.shutdown {
		errs = append(errs, f(ctx))
	}
	return errors.Join(errs...)
}

// errorOut is where the global error handler writes its one line.
var errorOut io.Writer = os.Stderr

// Setup builds the providers from cfg for the process that id describes.
func Setup(ctx context.Context, cfg config.Config, id Identity) (*Providers, error) {
	return setup(ctx, cfg, id, nil)
}

// setup is Setup with a TLS configuration for the exporters, which a test uses to trust its
// collector's certificate.
func setup(ctx context.Context, cfg config.Config, id Identity, tlsCfg *tls.Config) (*Providers, error) {
	if cfg.OTLPEndpoint == "" || cfg.OTelSDKDisabled {
		mp := metricnoop.NewMeterProvider()
		return &Providers{
			Meter:          mp.Meter(semconv.ServiceName),
			MeterProvider:  mp,
			TracerProvider: tracenoop.NewTracerProvider(),
			LogHandler:     slog.DiscardHandler,
		}, nil
	}
	base := strings.TrimRight(cfg.OTLPEndpoint, "/")
	u, err := url.ParseRequestURI(base)
	if err != nil || u.Host == "" {
		return nil, errors.New("OTLP endpoint must be an absolute URL with a host")
	}
	headers := map[string]string{}
	if cfg.OTLPInstanceID != "" && cfg.OTLPToken != "" {
		if u.Scheme != "https" {
			return nil, errors.New("OTLP endpoint must use https when credentials are set")
		}
		auth := base64.StdEncoding.EncodeToString([]byte(cfg.OTLPInstanceID + ":" + cfg.OTLPToken))
		headers["Authorization"] = "Basic " + auth
	}

	res, err := newResource(ctx, cfg, id)
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	self := &selfMetrics{}
	tOpts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(base + "/v1/traces"), otlptracehttp.WithHeaders(headers)}
	mOpts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpointURL(base + "/v1/metrics"), otlpmetrichttp.WithHeaders(headers)}
	lOpts := []otlploghttp.Option{otlploghttp.WithEndpointURL(base + "/v1/logs"), otlploghttp.WithHeaders(headers)}
	if tlsCfg != nil {
		tOpts = append(tOpts, otlptracehttp.WithTLSClientConfig(tlsCfg))
		mOpts = append(mOpts, otlpmetrichttp.WithTLSClientConfig(tlsCfg))
		lOpts = append(lOpts, otlploghttp.WithTLSClientConfig(tlsCfg))
	}
	tx, err := otlptracehttp.New(ctx, tOpts...)
	if err != nil {
		return nil, fmt.Errorf("create trace exporter: %w", err)
	}
	mx, err := otlpmetrichttp.New(ctx, mOpts...)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create metric exporter: %w", err), tx.Shutdown(ctx))
	}
	lx, err := otlploghttp.New(ctx, lOpts...)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create log exporter: %w", err), tx.Shutdown(ctx), mx.Shutdown(ctx))
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricFailureCounter(mx, self))),
		sdkmetric.WithView(histogramViews()...),
	)
	if err := self.bind(mp.Meter(semconv.ServiceName)); err != nil {
		return nil, errors.Join(err, tx.Shutdown(ctx), lx.Shutdown(ctx), mp.Shutdown(ctx))
	}
	// No sampler option: the SDK reads OTEL_TRACES_SAMPLER and otherwise samples everything
	// whose parent was sampled.
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(newFilteringExporter(traceFailureCounter(tx, self), semconv.IsSpanAttribute, self)),
	)
	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logFailureCounter(lx, self))),
	)
	if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
		return nil, errors.Join(fmt.Errorf("start runtime metrics: %w", err),
			tp.Shutdown(ctx), lp.Shutdown(ctx), mp.Shutdown(ctx))
	}

	// Contrib libraries default to the globals.
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	otel.SetErrorHandler(newErrorHandler(errorOut))

	return &Providers{
		Meter:          mp.Meter(semconv.ServiceName),
		MeterProvider:  mp,
		TracerProvider: tp,
		LogHandler:     newOTelHandler(lp.Logger(semconv.ServiceName), cfg.LogLevel),
		shutdown:       []func(context.Context) error{tp.Shutdown, lp.Shutdown, mp.Shutdown},
		flush:          []func(context.Context) error{tp.ForceFlush, lp.ForceFlush, mp.ForceFlush},
	}, nil
}

// newResource builds the resource. The environment's OTEL_RESOURCE_ATTRIBUTES comes first and
// finstore's own attributes last, so they win: an environment inherited by docker exec can add
// attributes but cannot change the service's name, version, instance, environment or role.
func newResource(ctx context.Context, cfg config.Config, id Identity) (*resource.Resource, error) {
	version := id.Version
	if version == "" {
		version = "unknown"
	}
	instance := id.InstanceID
	if instance == "" {
		instance = uuid.NewString()
		if id.Role == RoleServe {
			instance = ServeInstanceID
		}
	}
	ours := []attribute.KeyValue{
		attribute.String(semconv.AttrServiceName, semconv.ServiceName),
		attribute.String(semconv.AttrServiceVersion, version),
		attribute.String(semconv.AttrServiceInstanceID, instance),
		attribute.String(semconv.AttrDeploymentEnvironment, cfg.Environment),
		attribute.String(semconv.AttrProcessRole, string(id.Role)),
	}
	if id.Command != "" {
		ours = append(ours, attribute.String(semconv.AttrCommand, id.Command))
	}
	res, err := resource.New(ctx, resource.WithFromEnv(), resource.WithTelemetrySDK(), resource.WithAttributes(ours...))
	// A malformed OTEL_RESOURCE_ATTRIBUTES pair gives a partial resource: keep it (finstore's own
	// attributes are in it) rather than refuse to start, and never echo the bad value.
	if errors.Is(err, resource.ErrPartialResource) {
		return res, nil
	}
	return res, err
}

// histogramViews sets explicit bucket boundaries on every histogram finstore records or relies on.
// The default boundaries (0 to 10000) suit none of them.
func histogramViews() []sdkmetric.View {
	run := []float64{1, 5, 15, 30, 60, 120, 300, 600, 900, 1800, 2700, 3600}
	bank := []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30} // the request timeout is 30 s
	// Callbacks may take up to 75 s.
	inbound := []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 90}
	view := func(name string, bounds []float64) sdkmetric.View {
		return sdkmetric.NewView(
			sdkmetric.Instrument{Name: name, Kind: sdkmetric.InstrumentKindHistogram},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{Boundaries: bounds}},
		)
	}
	return []sdkmetric.View{
		view(semconv.MetricSyncRunDuration, run),
		view(semconv.MetricSchedulerJobDuration, run),
		view(semconv.MetricBankRequestDuration, bank),
		view("http.server.request.duration", inbound),
	}
}

// signalError tags an export error with the signal whose exporter failed, so the global error
// handler, which sees only an error, can say which.
type signalError struct {
	signal string
	err    error
}

func (e *signalError) Error() string { return e.err.Error() }
func (e *signalError) Unwrap() error { return e.err }

// failed counts and tags the error of an export of signal; a nil error passes through.
func failed(ctx context.Context, s *selfMetrics, signal string, err error) error {
	if err == nil {
		return nil
	}
	s.ExportFailed(ctx, signal)
	return &signalError{signal: signal, err: err}
}

// newErrorHandler returns the global OpenTelemetry error handler. It writes one fixed line and
// never the error, whose text can carry an address.
func newErrorHandler(w io.Writer) otel.ErrorHandler {
	return otel.ErrorHandlerFunc(func(err error) {
		var se *signalError
		if errors.As(err, &se) {
			_, _ = fmt.Fprintf(w, "finstore: telemetry export failed (%s)\n", se.signal)
			return
		}
		_, _ = fmt.Fprintln(w, "finstore: telemetry error")
	})
}

type countingSpans struct {
	sdktrace.SpanExporter
	self *selfMetrics
}

func (c countingSpans) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	return failed(ctx, c.self, semconv.SignalTraces, c.SpanExporter.ExportSpans(ctx, spans))
}

// traceFailureCounter counts and tags the failures of a span exporter.
func traceFailureCounter(inner sdktrace.SpanExporter, s *selfMetrics) sdktrace.SpanExporter {
	return countingSpans{SpanExporter: inner, self: s}
}

type countingMetrics struct {
	sdkmetric.Exporter
	self *selfMetrics
}

func (c countingMetrics) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	return failed(ctx, c.self, semconv.SignalMetrics, c.Exporter.Export(ctx, rm))
}

// metricFailureCounter counts and tags the failures of a metric exporter.
func metricFailureCounter(inner sdkmetric.Exporter, s *selfMetrics) sdkmetric.Exporter {
	return countingMetrics{Exporter: inner, self: s}
}

type countingLogs struct {
	sdklog.Exporter
	self *selfMetrics
}

func (c countingLogs) Export(ctx context.Context, records []sdklog.Record) error {
	return failed(ctx, c.self, semconv.SignalLogs, c.Exporter.Export(ctx, records))
}

// logFailureCounter counts and tags the failures of a log exporter.
func logFailureCounter(inner sdklog.Exporter, s *selfMetrics) sdklog.Exporter {
	return countingLogs{Exporter: inner, self: s}
}
