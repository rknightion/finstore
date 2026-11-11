package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/rknightion/finstore/internal/telemetry/telemetrytest"
)

// withVersion sets the build version for one test.
func withVersion(t *testing.T, v string) {
	t.Helper()
	old := version
	version = v
	t.Cleanup(func() { version = old })
}

// The binary passes its build version to telemetry. A command that fails for want of its
// database settings still exports its root span, so it is the cheapest command to observe.
func TestTheBuildVersionReachesTelemetry(t *testing.T) {
	withVersion(t, "main-1234567")
	sink := telemetrytest.New(t)
	code, _, errOut := runWith(t, map[string]string{"FINSTORE_OTLP_ENDPOINT": sink.URL(), "FINSTORE_ENVIRONMENT": "test"}, "coverage")
	if code != 1 || !strings.Contains(errOut, "FINSTORE_DATABASE_URL") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	spans := sink.Spans()
	if len(spans) != 1 || spans[0].Name != "finstore coverage" {
		t.Fatalf("spans = %+v, want the coverage command's root span", spans)
	}
	if got := spans[0].Resource["service.version"]; got != "main-1234567" {
		t.Errorf("service.version = %q, want the build version", got)
	}
}

// Leak test 6 at the binary: bootstrap, the health check, version and help set no telemetry up,
// so nothing reaches the collector even with every setting present.
func TestBootstrapHealthcheckVersionAndHelpExportNothing(t *testing.T) {
	sink := telemetrytest.New(t)
	env := map[string]string{
		"FINSTORE_OTLP_ENDPOINT":          sink.URL(),
		"FINSTORE_LISTEN_ADDR":            "127.0.0.1:1",
		"FINSTORE_LOG_LEVEL":              "debug",
		"FINSTORE_SUPERUSER_DATABASE_URL": "postgres://superuser:CANARY-PASSWORD@127.0.0.1:1/finstore",
		"FINSTORE_APP_PASSWORD":           "CANARY-APP",
		"FINSTORE_OWNER_PASSWORD":         "CANARY-OWNER",
	}
	for _, args := range [][]string{{"bootstrap"}, {"healthcheck"}, {"version"}, {"help"}, {"--help"}, {"-h"}} {
		runWith(t, env, args...)
	}
	if reqs := sink.Requests(); len(reqs) != 0 {
		t.Fatalf("%d requests reached the collector: %v", len(reqs), sink.Paths())
	}
}

func runWith(t *testing.T, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(context.Background(), args, strings.NewReader(""), &out, &errOut, func(k string) string { return env[k] })
	return code, out.String(), errOut.String()
}

func TestVersionPrintsBuildFacts(t *testing.T) {
	code, out, _ := runWith(t, nil, "version")
	if code != 0 || !strings.HasPrefix(out, "finstore ") {
		t.Fatalf("code %d, output %q", code, out)
	}
}

func TestUnknownCommandAndNoCommandPrintUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"nope"}} {
		code, _, errOut := runWith(t, nil, args...)
		if code != exitUsage || !strings.Contains(errOut, "usage:") {
			t.Errorf("%v: code %d, stderr %q", args, code, errOut)
		}
	}
}

// A command run without its settings must name the missing variable and never start work.
func TestCommandsNameTheMissingVariable(t *testing.T) {
	cases := map[string][]string{
		"FINSTORE_SUPERUSER_DATABASE_URL": {"bootstrap"},
		"FINSTORE_OWNER_DATABASE_URL":     {"migrate"},
		"FINSTORE_DATABASE_URL":           {"coverage"},
	}
	for variable, args := range cases {
		code, _, errOut := runWith(t, nil, args...)
		if code != 1 || !strings.Contains(errOut, variable) {
			t.Errorf("%v: code %d, stderr %q, want 1 naming %s", args, code, errOut, variable)
		}
	}
}

func TestAStrayArgumentIsRefused(t *testing.T) {
	code, _, errOut := runWith(t, nil, "migrate", "extra")
	if code != exitUsage || !strings.Contains(errOut, "unexpected argument") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}

func TestAccountEndNeedsASlugAndExactlyOneOfLastDateAndClear(t *testing.T) {
	for _, args := range [][]string{
		{"account", "end", "--last-date", "2024-08-21"},
		{"account", "end", "--slug", "bank-joint"},
		{"account", "end", "--slug", "bank-joint", "--last-date", "2024-08-21", "--clear"},
		{"account", "end", "--slug", "bank-joint", "--last-date", "21/08/2024"},
	} {
		code, _, errOut := runWith(t, nil, args...)
		if code != exitUsage || !strings.Contains(errOut, "finstore account end") {
			t.Errorf("%v: code %d, stderr %q", args, code, errOut)
		}
	}
	// A well-formed command goes on to need the database.
	code, _, errOut := runWith(t, nil, "account", "end", "--slug", "bank-joint", "--clear")
	if code != 1 || !strings.Contains(errOut, "FINSTORE_DATABASE_URL") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}

func TestImportNeedsAccountAndFormat(t *testing.T) {
	code, _, errOut := runWith(t, nil, "import", "--account", "a")
	if code != exitUsage || !strings.Contains(errOut, "--format") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}

// A setting that does not parse stops every command, with the variable's name and not its value.
func TestABadSettingStopsTheCommandWithoutEchoingIt(t *testing.T) {
	code, _, errOut := runWith(t, map[string]string{"FINSTORE_MONZO_WINDOW": "VALUE-MARKER"}, "coverage")
	if code != 1 || !strings.Contains(errOut, "FINSTORE_MONZO_WINDOW") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	if strings.Contains(errOut, "VALUE-MARKER") {
		t.Fatalf("stderr echoes the value: %q", errOut)
	}
}
