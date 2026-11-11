// Package telemetrytest is a stand-in OTLP/HTTP collector for tests. It records every request an
// exporter makes and decodes the protobuf bodies into plain values, so a test can ask what a
// process exported without a collector or a vendor decoder.
package telemetrytest

import (
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
)

// Request is one export the sink received.
type Request struct {
	Path   string
	Header http.Header
	Body   []byte
}

// Sink is an OTLP/HTTP collector on a local port.
type Sink struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []Request
	fail map[string]int
}

// FailPath makes the sink answer status to every later request whose path ends in suffix, such
// as "/v1/traces". The request is still recorded.
func (s *Sink) FailPath(suffix string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail == nil {
		s.fail = map[string]int{}
	}
	s.fail[suffix] = status
}

// New starts a plain-HTTP sink that is closed when the test ends. Use it where no credentials
// are configured: the service refuses to send credentials over HTTP.
func New(t testing.TB) *Sink { return start(t, false) }

// NewTLS starts an HTTPS sink. Hand TLSConfig to the code under test, which otherwise would not
// trust the test certificate.
func NewTLS(t testing.TB) *Sink { return start(t, true) }

func start(t testing.TB, secure bool) *Sink {
	t.Helper()
	s := &Sink{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.reqs = append(s.reqs, Request{Path: r.URL.Path, Header: r.Header.Clone(), Body: b})
		status := 0
		for suffix, code := range s.fail {
			if strings.HasSuffix(r.URL.Path, suffix) {
				status = code
			}
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
		if status != 0 {
			w.WriteHeader(status)
		}
	})
	if secure {
		s.srv = httptest.NewTLSServer(h)
	} else {
		s.srv = httptest.NewServer(h)
	}
	t.Cleanup(s.srv.Close)
	return s
}

// URL is the sink's base URL; the exporters add /v1/traces and the like.
func (s *Sink) URL() string { return s.srv.URL }

// TLSConfig trusts an HTTPS sink's certificate.
func (s *Sink) TLSConfig() *tls.Config {
	return s.srv.Client().Transport.(*http.Transport).TLSClientConfig
}

// Requests returns every request received so far.
func (s *Sink) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.reqs...)
}

// Paths returns the distinct request paths received, sorted.
func (s *Sink) Paths() []string {
	seen := map[string]bool{}
	for _, r := range s.Requests() {
		seen[r.Path] = true
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Span is an exported span with its attributes stringified.
type Span struct {
	Name          string
	TraceID       string
	SpanID        string
	ParentSpanID  string
	Attrs         map[string]string
	Resource      map[string]string
	Kind          int32 // 1 internal, 2 server, 3 client, 4 producer, 5 consumer
	StatusCode    int32 // 0 unset, 1 ok, 2 error
	StatusMessage string
	Events        int
	Links         int
	LinkTraceIDs  []string
	// Dropped is the span's dropped-attribute count.
	Dropped uint32
}

// Spans decodes every trace export received.
func (s *Sink) Spans() []Span {
	var out []Span
	for _, r := range s.Requests() {
		if !strings.HasSuffix(r.Path, "/v1/traces") {
			continue
		}
		var req coltrace.ExportTraceServiceRequest
		if err := proto.Unmarshal(r.Body, &req); err != nil {
			continue
		}
		for _, rs := range req.ResourceSpans {
			res := attrs(rs.GetResource().GetAttributes())
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					x := Span{
						Name: sp.Name, TraceID: hex.EncodeToString(sp.TraceId), SpanID: hex.EncodeToString(sp.SpanId),
						ParentSpanID: hex.EncodeToString(sp.ParentSpanId), Attrs: attrs(sp.Attributes), Resource: res,
						StatusCode: int32(sp.GetStatus().GetCode()), StatusMessage: sp.GetStatus().GetMessage(),
						Events: len(sp.Events), Links: len(sp.Links), Dropped: sp.DroppedAttributesCount,
						Kind: int32(sp.Kind),
					}
					for _, l := range sp.Links {
						x.LinkTraceIDs = append(x.LinkTraceIDs, hex.EncodeToString(l.TraceId))
					}
					out = append(out, x)
				}
			}
		}
	}
	return out
}

// LogRecord is an exported log record.
type LogRecord struct {
	Body     string
	TraceID  string
	SpanID   string
	Attrs    map[string]string
	Resource map[string]string
}

// Logs decodes every log export received.
func (s *Sink) Logs() []LogRecord {
	var out []LogRecord
	for _, r := range s.Requests() {
		if !strings.HasSuffix(r.Path, "/v1/logs") {
			continue
		}
		var req collogs.ExportLogsServiceRequest
		if err := proto.Unmarshal(r.Body, &req); err != nil {
			continue
		}
		for _, rl := range req.ResourceLogs {
			res := attrs(rl.GetResource().GetAttributes())
			for _, sl := range rl.ScopeLogs {
				for _, lr := range sl.LogRecords {
					out = append(out, LogRecord{
						Body: value(lr.GetBody()), TraceID: hex.EncodeToString(lr.TraceId), SpanID: hex.EncodeToString(lr.SpanId),
						Attrs: attrs(lr.Attributes), Resource: res,
					})
				}
			}
		}
	}
	return out
}

// Point is one data point of an exported metric. Value holds a number point's value and a
// histogram's count.
type Point struct {
	Attrs      map[string]string
	Value      float64
	Boundaries []float64 // a histogram's explicit bucket bounds
}

// Metric is an exported metric with its points from every export.
type Metric struct {
	Name     string
	Resource map[string]string
	Points   []Point
}

// Metrics decodes every metric export received, merging points by metric name.
func (s *Sink) Metrics() map[string]Metric {
	out := map[string]Metric{}
	for _, r := range s.Requests() {
		if !strings.HasSuffix(r.Path, "/v1/metrics") {
			continue
		}
		var req colmetrics.ExportMetricsServiceRequest
		if err := proto.Unmarshal(r.Body, &req); err != nil {
			continue
		}
		for _, rm := range req.ResourceMetrics {
			res := attrs(rm.GetResource().GetAttributes())
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					x := out[m.Name]
					x.Name, x.Resource = m.Name, res
					if g := m.GetGauge(); g != nil {
						for _, p := range g.DataPoints {
							x.Points = append(x.Points, Point{Attrs: attrs(p.Attributes), Value: number(p.GetAsInt(), p.GetAsDouble())})
						}
					}
					if su := m.GetSum(); su != nil {
						for _, p := range su.DataPoints {
							x.Points = append(x.Points, Point{Attrs: attrs(p.Attributes), Value: number(p.GetAsInt(), p.GetAsDouble())})
						}
					}
					if h := m.GetHistogram(); h != nil {
						for _, p := range h.DataPoints {
							x.Points = append(x.Points, Point{Attrs: attrs(p.Attributes), Value: float64(p.Count), Boundaries: p.ExplicitBounds})
						}
					}
					out[m.Name] = x
				}
			}
		}
	}
	return out
}

func number(i int64, f float64) float64 {
	if i != 0 {
		return float64(i)
	}
	return f
}

func attrs(kvs []*commonpb.KeyValue) map[string]string {
	m := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		m[kv.Key] = value(kv.Value)
	}
	return m
}

func value(v *commonpb.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'g', -1, 64)
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}
