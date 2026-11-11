package telemetry

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"runtime/debug"
	"strings"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/rknightion/finstore/internal/config"
	"github.com/rknightion/finstore/internal/semconv"
)

// Providers owns the metric and log pipelines. With no OTLP endpoint
// configured it is a working no-op that exports nothing.
type Providers struct {
	// Meter creates instruments; pass it to NewMetrics.
	Meter metric.Meter
	// LogHandler forwards records to OTLP logs. It does no filtering: give it
	// to NewLogger as an extra handler so the allow-list applies to it.
	LogHandler slog.Handler

	shutdown []func(context.Context) error
}

// Setup builds the providers from cfg.
func Setup(ctx context.Context, cfg config.Config) (*Providers, error) {
	if cfg.OTLPEndpoint == "" {
		return &Providers{
			Meter:      metricnoop.NewMeterProvider().Meter(semconv.ServiceName),
			LogHandler: slog.DiscardHandler,
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

	res, err := resource.New(ctx, resource.WithAttributes(
		attribute.String(semconv.AttrServiceName, semconv.ServiceName),
		attribute.String(semconv.AttrServiceVersion, version()),
		attribute.String(semconv.AttrServiceInstanceID, uuid.NewString()),
		attribute.String(semconv.AttrDeploymentEnvironment, cfg.Environment),
	))
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}
	mx, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(base+"/v1/metrics"), otlpmetrichttp.WithHeaders(headers))
	if err != nil {
		return nil, fmt.Errorf("create metric exporter: %w", err)
	}
	lx, err := otlploghttp.New(ctx, otlploghttp.WithEndpointURL(base+"/v1/logs"), otlploghttp.WithHeaders(headers))
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create log exporter: %w", err), mx.Shutdown(ctx))
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(mx)),
	)
	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(lx)),
	)
	return &Providers{
		Meter:      mp.Meter(semconv.ServiceName),
		LogHandler: newOTelHandler(lp.Logger(semconv.ServiceName), cfg.LogLevel),
		shutdown:   []func(context.Context) error{mp.Shutdown, lp.Shutdown},
	}, nil
}

// Shutdown flushes and stops the exporters. It is a no-op without an endpoint.
func (p *Providers) Shutdown(ctx context.Context) error {
	var errs []error
	for _, f := range p.shutdown {
		errs = append(errs, f(ctx))
	}
	return errors.Join(errs...)
}

func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "unknown"
}
