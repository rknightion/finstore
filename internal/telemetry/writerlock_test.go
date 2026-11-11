package telemetry

import (
	"context"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rknightion/finstore/internal/semconv"
)

// heldPoints collects the writer-lock gauge's data points from reader.
func heldPoints(t *testing.T, reader *sdkmetric.ManualReader) []metricdata.DataPoint[float64] {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != semconv.MetricWriterLockHeld {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[float64])
			if !ok {
				t.Fatalf("%s is a %T, want a float gauge", m.Name, m.Data)
			}
			if m.Unit != "s" {
				t.Errorf("unit %q, want s", m.Unit)
			}
			return g.DataPoints
		}
	}
	return nil
}

// The held gauge names the holder and the seconds held so far while a hold exists, and has no
// point at all otherwise.
func TestObserveWriterLockReportsTheHolderOnlyWhileHeld(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := NewMetrics(mp.Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	var (
		holder = "truelayer.sync"
		since  = time.Now().Add(-90 * time.Second)
		held   = true
	)
	if err := m.ObserveWriterLock(func() (string, time.Time, bool) { return holder, since, held }); err != nil {
		t.Fatal(err)
	}

	pts := heldPoints(t, reader)
	if len(pts) != 1 {
		t.Fatalf("%d points while held, want 1", len(pts))
	}
	if got, _ := pts[0].Attributes.Value(semconv.AttrHolder); got.AsString() != holder {
		t.Errorf("holder attribute %q, want %q", got.AsString(), holder)
	}
	if v := pts[0].Value; v < 90 || v > 120 {
		t.Errorf("held for %v s, want about 90", v)
	}

	held = false
	if pts := heldPoints(t, reader); len(pts) != 0 {
		t.Errorf("%d points after release, want none", len(pts))
	}
}
