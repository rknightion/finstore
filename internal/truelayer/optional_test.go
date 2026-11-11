package truelayer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/consent"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/truelayertest"
)

// connectedNextDay connects Lloyds on one day, which reads /info once, and moves to the next, so the
// next scheduled run is the first of its day.
func (h *harness) connectedNextDay() *Worker {
	h.t.Helper()
	h.lloydsFixture()
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	h.fake.Advance(24 * time.Hour)
	return w
}

// requireOKRunWithActiveConsent fails unless the newest run is an ok sync and the consent is still
// active: an optional read never changes either.
func requireOKRunWithActiveConsent(t *testing.T, h *harness) {
	t.Helper()
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindSync, outcomeOK, "")
	if c := h.consent(Source(ConnLloyds)); c.State != store.ConsentActive || c.LastSyncOKAt == nil || !c.LastSyncOKAt.Equal(h.fake.Now()) {
		t.Fatalf("consent %s, last ok %v; want active and synced now (%v)", c.State, c.LastSyncOKAt, h.fake.Now())
	}
}

// An endpoint the bank says it does not support (501, or a bank code for an unsupported feature) is
// logged at info level and counted unsupported; the run stays ok and nothing ends.
func TestInfoUnsupportedLeavesTheRunOK(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		code   string
	}{
		"501":                    {http.StatusNotImplemented, ""},
		"endpoint_not_supported": {http.StatusBadRequest, "endpoint_not_supported"},
		"not_supported":          {http.StatusBadRequest, "not_supported"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			w := h.connectedNextDay()
			h.fake.FailWith(ConnLloyds, pathInfo, c.status, c.code, 1)
			before := len(h.fake.Requests())
			if err := w.RunScheduled(context.Background()); err != nil {
				t.Fatalf("RunScheduled: %v", err)
			}
			requireOKRunWithActiveConsent(t, h)
			if n := h.optional(pathInfo, "unsupported"); n != 1 {
				t.Errorf("optional_reads{info, unsupported} = %d, want 1", n)
			}
			if n := h.optional(pathInfo, "ok"); n != 1 {
				t.Errorf("optional_reads{info, ok} = %d, want only the connect's 1", n)
			}
			reqs := h.requests(before)
			if last := reqs[len(reqs)-1]; last.Path != pathInfo {
				t.Errorf("last request %s, want /info", last.Path)
			}
			if got := h.logs.messages(slog.LevelInfo); !slices.Contains(got, "truelayer optional read unsupported") {
				t.Errorf("info log lines %v lack the unsupported line", got)
			}
			if got := h.logs.attrs("truelayer optional read unsupported"); len(got) != 1 || !strings.Contains(got[0], "endpoint="+pathInfo) {
				t.Errorf("unsupported line fields %v, want the endpoint", got)
			}
			for _, w := range h.logs.messages(slog.LevelWarn) {
				if strings.Contains(w, "optional read") {
					t.Errorf("an unsupported answer logged a warning: %s", w)
				}
			}
		})
	}
}

// A refused data call on an optional read (401, a plain 403 or access_denied) never ends the
// consent: the run stays ok, a warning is logged and the read is counted refused.
func TestInfoRefusedLeavesTheConsentActiveAndTheRunOK(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		code   string
	}{
		"401":           {http.StatusUnauthorized, ""},
		"403":           {http.StatusForbidden, ""},
		"access_denied": {http.StatusForbidden, "access_denied"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			w := h.connectedNextDay()
			h.fake.FailWith(ConnLloyds, pathInfo, c.status, c.code, 1)
			before := len(h.fake.Requests())
			if err := w.RunScheduled(context.Background()); err != nil {
				t.Fatalf("RunScheduled: %v", err)
			}
			requireOKRunWithActiveConsent(t, h)
			if n := h.optional(pathInfo, "refused"); n != 1 {
				t.Errorf("optional_reads{info, refused} = %d, want 1", n)
			}
			if got := h.logs.messages(slog.LevelWarn); !slices.Contains(got, "truelayer optional read refused") {
				t.Errorf("warnings %v lack the refused line", got)
			}
			if h.syncRuns(outcomePartial, "") != 0 || h.syncRuns(outcomeFailed, "") != 0 {
				t.Error("an optional refusal marked a run partial or failed")
			}
			reqs := h.requests(before)
			if last := reqs[len(reqs)-1]; last.Path != pathInfo {
				t.Errorf("last request %s, want the refused /info", last.Path)
			}
		})
	}
}

// A 429 on an optional read is counted rate_limited and leaves the run ok.
func TestInfoRateLimitedLeavesTheRunOK(t *testing.T) {
	h := newHarness(t)
	w := h.connectedNextDay()
	h.fake.Fail429(ConnLloyds, pathInfo, 1)
	if err := w.RunScheduled(context.Background()); err != nil {
		t.Fatalf("RunScheduled: %v", err)
	}
	requireOKRunWithActiveConsent(t, h)
	if n := h.optional(pathInfo, "rate_limited"); n != 1 {
		t.Errorf("optional_reads{info, rate_limited} = %d, want 1", n)
	}
	if got := h.logs.messages(slog.LevelWarn); !slices.Contains(got, "truelayer optional read rate limited") {
		t.Errorf("warnings %v lack the rate limited line", got)
	}
}

// Any other bank failure on an optional read is counted failed and leaves the run ok.
func TestInfoOtherFailureLeavesTheRunOK(t *testing.T) {
	h := newHarness(t)
	w := h.connectedNextDay()
	h.fake.Fail(ConnLloyds, pathInfo, http.StatusInternalServerError, 1)
	if err := w.RunScheduled(context.Background()); err != nil {
		t.Fatalf("RunScheduled: %v", err)
	}
	requireOKRunWithActiveConsent(t, h)
	if n := h.optional(pathInfo, "failed"); n != 1 {
		t.Errorf("optional_reads{info, failed} = %d, want 1", n)
	}
	if got := h.logs.messages(slog.LevelWarn); !slices.Contains(got, "truelayer optional read failed") {
		t.Errorf("warnings %v lack the failed line", got)
	}
}

// stepOptional: finstore's own daily limit counts rate_limited and makes the rest of the run's
// optional reads not happen; a failure that is not a bank answer to the data call is returned.
func TestStepOptionalRules(t *testing.T) {
	h := newHarness(t)
	w := h.worker(ConnLloyds, nil)
	ctx := context.Background()

	run := &optionalRun{}
	if err := w.stepOptional(ctx, run, pathInfo, func(context.Context) error { return errDailyLimit }); err != nil {
		t.Fatalf("the daily limit stopped the run: %v", err)
	}
	if n := h.optional(pathInfo, "rate_limited"); n != 1 {
		t.Errorf("optional_reads{info, rate_limited} = %d, want 1", n)
	}
	called := false
	if err := w.stepOptional(ctx, run, pathInfo, func(context.Context) error { called = true; return nil }); err != nil || called {
		t.Errorf("after a rate limit the next optional read was made (%v) or failed (%v)", called, err)
	}

	for name, fail := range map[string]error{
		"consent lost": ErrConsentLost,
		"cancelled":    context.Canceled,
		"deadline":     context.DeadlineExceeded,
		"store":        errors.New("store: connection closed"),
	} {
		err := w.stepOptional(ctx, &optionalRun{}, pathInfo, func(context.Context) error { return fail })
		if !errors.Is(err, fail) {
			t.Errorf("%s: stepOptional returned %v, want the error to stop the run", name, err)
		}
	}
	if err := w.stepOptional(ctx, &optionalRun{}, pathInfo, func(context.Context) error { return errBody(pathInfo, "x") }); err != nil {
		t.Errorf("an unreadable body stopped the run: %v", err)
	}
	if n := h.optional(pathInfo, "failed"); n != 1 {
		t.Errorf("optional_reads{info, failed} = %d, want 1", n)
	}
}

// A refused token refresh on the way to an optional read still ends the consent: it is no bank
// answer to the data call.
func TestRefusedRefreshDuringInfoStillEndsTheConsent(t *testing.T) {
	h := newHarness(t)
	h.monzoFixture()
	h.fake.SetScopes(ConnMonzo, "info", "accounts", "balance", "transactions", "direct_debits", "standing_orders", "offline_access")
	w := h.worker(ConnMonzo, nil)
	h.connect(w)
	h.fake.Advance(24 * time.Hour)
	// After the second account's direct debits, the last read before /info: the bank revokes the
	// consent and the access token runs out, so /info needs a refresh, which is refused.
	var dds atomic.Int32
	hook := func(rw http.ResponseWriter, r *http.Request) bool {
		h.fake.ServeHTTP(rw, r)
		if templateOf(r.URL.Path) == pathDirectDebits && dds.Add(1) == 2 {
			h.fake.RevokeConsent(ConnMonzo)
			h.fake.Advance(58 * time.Minute)
		}
		return true
	}
	h.onRequest.Store(&hook)
	before := len(h.fake.Requests())
	if err := w.RunScheduled(context.Background()); !errors.Is(err, ErrConsentLost) {
		t.Fatalf("RunScheduled = %v, want ErrConsentLost", err)
	}
	c := h.consent(Source(ConnMonzo))
	if c.State != store.ConsentLost || *c.EndReason != string(consent.ReasonRefreshRejected) {
		t.Fatalf("consent %s %v, want lost refresh_rejected", c.State, c.EndReason)
	}
	requireRun(t, h.lastRun(Source(ConnMonzo)), KindSync, outcomeFailed, categoryConsentLost)
	for _, r := range h.requests(before) {
		if r.Path == pathInfo {
			t.Error("/info was requested after the refresh was refused")
		}
	}
}

// /info is read at most once a local day for a source, whatever ran: the connect, then two scheduled
// runs the same day, then a run the next day.
func TestInfoIsCalledOncePerLocalDay(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	for range 2 {
		if err := w.RunScheduled(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := h.fake.Count(pathInfo); n != 1 {
		t.Fatalf("/info called %d times on the first day, want 1", n)
	}
	h.fake.Advance(24 * time.Hour)
	if err := w.RunScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.RunScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := h.fake.Count(pathInfo); n != 2 {
		t.Fatalf("/info called %d times over two days, want 2", n)
	}
	if n := h.optional(pathInfo, "ok"); n != 2 {
		t.Errorf("optional_reads{info, ok} = %d, want 2", n)
	}
	// Each answer is in raw, under the run's list fetch.
	var stored int
	if err := h.st.DB().QueryRow(context.Background(), `SELECT count(*) FROM raw.api_response r JOIN raw.fetch f ON f.id = r.fetch_id
		WHERE r.endpoint = $1 AND r.status = 200 AND f.account_key IS NULL AND f.kind IN ('connect', 'sync')`, pathInfo).Scan(&stored); err != nil || stored != 2 {
		t.Fatalf("stored /info answers under a list fetch: %d, %v", stored, err)
	}
}

// Identity data is kept in raw and reaches nothing in core: no core table holds a value from it.
func TestNothingFromInfoReachesCore(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	if h.fake.Count(pathInfo) != 1 {
		t.Fatal("the connect made no /info call")
	}
	ctx := context.Background()
	rows, err := h.st.DB().Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = 'core'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	for _, tbl := range tables {
		for _, needle := range []string{"Made Up Person", "made.up@example.com", "Made Up Street", "ZZ1 1ZZ", "1990-01-01"} {
			var n int
			q := fmt.Sprintf(`SELECT count(*) FROM core.%s x WHERE x::text LIKE '%%' || $1 || '%%'`, tbl)
			if err := h.st.DB().QueryRow(ctx, q, needle).Scan(&n); err != nil || n != 0 {
				t.Errorf("core.%s holds %q in %d rows (%v)", tbl, needle, n, err)
			}
		}
	}
	var raw int
	if err := h.st.DB().QueryRow(ctx, `SELECT count(*) FROM raw.api_response WHERE endpoint = $1 AND convert_from(body, 'UTF8') LIKE '%Made Up Person%'`, pathInfo).Scan(&raw); err != nil || raw != 1 {
		t.Fatalf("raw holds the /info body %d times, %v", raw, err)
	}
}

// truelayertest.AllScopes is exactly what a connect asks for.
func TestFakeAllScopesAreTheScopesAConnectAsksFor(t *testing.T) {
	if got, want := strings.Join(truelayertest.AllScopes, " "), Scopes; got != want {
		t.Fatalf("fake AllScopes = %q, Scopes = %q", got, want)
	}
}
