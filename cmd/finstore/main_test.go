package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

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
