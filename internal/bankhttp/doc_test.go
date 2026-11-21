package bankhttp

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// net/http re-sends an idempotent GET by itself when a reused idle connection was closed before any
// response, and WithoutRetries cannot stop it, so a bank may see a call twice. The package
// documentation says so, because a caller counting calls against a limit would otherwise trust
// that one call is one request on the wire.
func TestPackageDocumentationNotesNetHTTPMayResendOnAClosedIdleConnection(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var doc string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), e.Name(), nil, parser.ParseComments|parser.PackageClauseOnly)
		if err != nil {
			t.Fatal(err)
		}
		if f.Doc != nil {
			doc += f.Doc.Text()
		}
	}
	for _, want := range []string{"net/http re-sends an idempotent request", "closed by the peer", "idle connection", "WithoutRetries"} {
		if !strings.Contains(doc, want) {
			t.Errorf("package documentation lacks %q", want)
		}
	}
}
