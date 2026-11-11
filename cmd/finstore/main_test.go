package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunVersionPrintsBuildFacts(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"version"}, &out, &errOut); code != 0 {
		t.Fatalf("exit code %d, want 0", code)
	}
	if !strings.HasPrefix(out.String(), "finstore ") {
		t.Fatalf("unexpected output %q", out.String())
	}
}

func TestRunUnknownCommandExitsTwo(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"nope"}, &out, &errOut); code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "usage:") {
		t.Fatalf("no usage on stderr: %q", errOut.String())
	}
}
