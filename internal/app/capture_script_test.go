package app

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The Aviva capture script runs inside the operator's signed-in browser, so the only thing keeping
// it read-only is what it is written to do. These tests fail if its text refers to any network,
// storage, cookie, navigation, click, submit or page-writing API. Nothing is stripped before
// matching, so a banned word in a comment or a string fails too: the script has no use for one.
// Matching is by whole identifier, so a key such as allocation_percent, which contains the letters
// of "location", is not a reference to it.

// captureBannedWords are identifiers the script must not mention.
var captureBannedWords = map[string]bool{
	"fetch":              true,
	"XMLHttpRequest":     true,
	"WebSocket":          true,
	"EventSource":        true,
	"sendBeacon":         true,
	"postMessage":        true,
	"Worker":             true,
	"SharedWorker":       true,
	"localStorage":       true,
	"sessionStorage":     true,
	"indexedDB":          true,
	"caches":             true,
	"cookie":             true,
	"cookies":            true,
	"location":           true,
	"navigate":           true,
	"navigation":         true,
	"click":              true,
	"submit":             true,
	"requestSubmit":      true,
	"dispatchEvent":      true,
	"eval":               true,
	"constructor":        true,
	"innerHTML":          true,
	"outerHTML":          true,
	"insertAdjacentHTML": true,
	"write":              true,
	"writeln":            true,
}

// capturePattern is a banned reference that spans more than one token.
type capturePattern struct {
	name string
	re   *regexp.Regexp
}

// captureAssigned matches a property being assigned to, including compound assignment, and not a
// comparison: `.value = x` and `.value += x` match, `.value == x`, `.value <= x` and reads do not.
func captureAssigned(property string) *regexp.Regexp {
	return regexp.MustCompile(`\.\s*` + property + `\s*[-+*/%&|^?]*=(?:[^=]|$)`)
}

var capturePatterns = []capturePattern{
	{"window.open", regexp.MustCompile(`\bwindow\s*\.\s*open\b`)},
	{".open(", regexp.MustCompile(`\.\s*open\s*\(`)},
	{".click", regexp.MustCompile(`\.\s*click\b`)},
	{"import(", regexp.MustCompile(`\bimport\s*\(`)},
	{"Function(", regexp.MustCompile(`\bFunction\s*\(`)},
	{".value =", captureAssigned("value")},
	{".checked =", captureAssigned("checked")},
	{".selected =", captureAssigned("selected")},
	{".disabled =", captureAssigned("disabled")},
	{".textContent =", captureAssigned("textContent")},
	{".innerText =", captureAssigned("innerText")},
}

var (
	captureIdent        = regexp.MustCompile(`[A-Za-z0-9_$]+`)
	captureLocalHistory = regexp.MustCompile(`\b(?:const|let|var)\s+history\b`)
)

// bannedCaptureReferences returns the sorted names of every banned reference in src.
func bannedCaptureReferences(src string) []string {
	found := map[string]bool{}
	localHistory := captureLocalHistory.MatchString(src)
	for _, loc := range captureIdent.FindAllStringIndex(src, -1) {
		token := src[loc[0]:loc[1]]
		if token == "history" {
			if captureHistoryReference(src, loc[0], loc[1], localHistory) {
				found["history"] = true
			}
			continue
		}
		if captureBannedWords[token] {
			found[token] = true
		}
	}
	for _, p := range capturePatterns {
		if p.re.MatchString(src) {
			found[p.name] = true
		}
	}
	return slices.Sorted(maps.Keys(found))
}

// captureHistoryReference decides whether the identifier `history` at src[start:end] can be the
// browser's history object. The script has to say "Contribution history" and "Full history", the
// page's own labels, and keeps a local variable of that name, so the bare word is allowed in
// those two places only. A member access on it, or a use of it when no local of that name is
// declared, is a reference to the browser object.
func captureHistoryReference(src string, start, end int, localHistory bool) bool {
	before := src[:start]
	for _, label := range []string{"Contribution ", "Full ", "full-"} {
		if strings.HasSuffix(before, label) {
			return false
		}
	}
	if strings.HasSuffix(strings.TrimRight(before, " \t\r\n"), ".") {
		return true
	}
	after := strings.TrimLeft(src[end:], " \t\r\n")
	for _, member := range []string{".", "[", "(", "?."} {
		if strings.HasPrefix(after, member) {
			return true
		}
	}
	return !localHistory
}

// The matcher is proved here on small strings, so a pass on the real script means something: each
// banned reference must be flagged, and the names the script legitimately uses must not be.
func TestCaptureScriptMatcherFlagsBannedReferencesOnly(t *testing.T) {
	flagged := []struct{ name, src string }{
		{"fetch", `fetch("/x")`},
		{"XMLHttpRequest", `const x = new XMLHttpRequest();`},
		{"WebSocket", `new WebSocket("wss://example.invalid")`},
		{"EventSource", `new EventSource("/x")`},
		{"sendBeacon", `navigator.sendBeacon("/x", "y")`},
		{"localStorage", `localStorage.getItem("k")`},
		{"sessionStorage", `sessionStorage.setItem("k", "v")`},
		{"indexedDB", `indexedDB.open("db")`},
		{"caches", `caches.keys()`},
		{"cookie", `document.cookie`},
		{"location", `window.location = "/x"`},
		{"location", `location.href`},
		{"history", `window.history.back()`},
		{"history", `history.pushState(null, "", "/x")`},
		{"history", `self.history.length`},
		{"history", `const h = history;`},
		{"history", `const history = 1; history.back();`},
		{"window.open", `window.open("/x")`},
		{"navigate", `navigation.navigate("/x")`},
		{".click", `button.click()`},
		{"click", `const c = button["click"];`},
		{"submit", `form.submit()`},
		{"dispatchEvent", `node.dispatchEvent(new Event("x"))`},
		{"import(", `import("./x.js")`},
		{"eval", `eval("1")`},
		{"Function(", `new Function("return 1")()`},
		{"innerHTML", `node.innerHTML = "x"`},
		{".value =", `input.value = "1"`},
		{".value =", `input.value+= "1"`},
		{".checked =", `box.checked = true`},
		// A banned word in a comment or a string fails, since nothing is stripped first.
		{"fetch", `// never call fetch here`},
		{"cookie", `const label = "cookie";`},
	}
	for _, c := range flagged {
		t.Run("flags "+c.src, func(t *testing.T) {
			if got := bannedCaptureReferences(c.src); !slices.Contains(got, c.name) {
				t.Fatalf("%q was not flagged as %q (flagged: %v)", c.src, c.name, got)
			}
		})
	}

	allowed := []string{
		// The script's required key contains the letters of "location" and must not trip it.
		`allocation_percent: decimal(field("Pension allocated %")),`,
		`value_history,`,
		`const relocation = 1; const fetcher = 2; const evaluate = 3; const submitted = 4;`,
		`const clicks = 1; const importer = 2; const cookiejar = 3;`,
		// The page's own labels, and a local variable that is not the browser object.
		`// Contribution history with Full history selected`,
		`document.getElementById("full-history")`,
		`lines.findIndex((l) => l.startsWith("Contribution history ("));`,
		`const history = 1; if (total > history) { return history + 1; }`,
		// Reads and comparisons are not writes.
		`if (!box.checked) fail("x")`,
		`a.value == b`,
		`a.value === b`,
		`a.value <= 1`,
		`a.checked >= 1`,
		`a.checked != 1`,
		`p.textContent.trim() === "Units held"`,
		`// for finstore import --format aviva-capture-json`,
	}
	for _, src := range allowed {
		t.Run("allows "+src, func(t *testing.T) {
			if got := bannedCaptureReferences(src); len(got) != 0 {
				t.Fatalf("%q was flagged: %v", src, got)
			}
		})
	}
}

// The script reads the page and returns text. It has no reference to any network, storage, cookie,
// navigation, click, submit or page-writing API, in code, comment or string.
func TestCaptureScriptHasNoNetworkStorageNavigationClickSubmitOrWriteReference(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "capture", "aviva-capture.js")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the capture script: %v", err)
	}
	// An empty or wrong file would pass the scan, so check it is the script.
	if !strings.Contains(string(src), "function avivaCapture(") {
		t.Fatalf("%s does not define avivaCapture", path)
	}
	if got := bannedCaptureReferences(string(src)); len(got) != 0 {
		t.Fatalf("the capture script refers to banned APIs: %v", got)
	}
}
