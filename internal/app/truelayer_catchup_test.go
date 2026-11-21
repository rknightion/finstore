package app_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/app"
	"github.com/rknightion/finstore/internal/monzotest"
	"github.com/rknightion/finstore/internal/scheduler"
	"github.com/rknightion/finstore/internal/testpg"
	"github.com/rknightion/finstore/internal/truelayer"
	"github.com/rknightion/finstore/internal/truelayertest"
)

// zoneNowIsNoon is a whole-hour zone in which it is now between 12:00 and 12:59. The scheduled
// TrueLayer syncs are at 06:20 and 18:20 in the service's zone, so in this one the most recent slot
// is 06:20, five to seven hours ago, and no scheduled slot can fall inside the test.
func zoneNowIsNoon(t *testing.T) *time.Location {
	t.Helper()
	offset := 12 - time.Now().UTC().Hour() // hours east of UTC that put the clock at 12
	if offset > 12 {
		offset -= 24
	}
	name := "Etc/GMT"
	if offset > 0 {
		name = fmt.Sprintf("Etc/GMT-%d", offset) // the Etc zones count the other way
	} else if offset < 0 {
		name = fmt.Sprintf("Etc/GMT+%d", -offset)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("no tz database: %v", err)
	}
	return loc
}

// TestServeCatchesUpAMissedTrueLayerSync starts the service over a database whose Lloyds consent
// was last synced at different times against the most recent slot, with the scheduler on: it
// runs one sync, through the scheduled path, when the last success is before that slot, and none
// (no run and no bank call) when it is after, when the consent is lost or expired, or when a
// connection has no consent.
func TestServeCatchesUpAMissedTrueLayerSync(t *testing.T) {
	super := testpg.Start(t)
	now := time.Now()

	mfake := monzotest.New(monzotest.WithClock(time.Now))
	msrv := mfake.Start()
	t.Cleanup(msrv.Close)
	tfake := truelayertest.New(truelayertest.WithClock(time.Now))
	tsrv := httptest.NewServer(tfake)
	t.Cleanup(tsrv.Close)
	tfake.AddAccount(truelayer.ConnLloyds, truelayertest.Account{ID: "tl-acc-catchup", Type: "TRANSACTION",
		DisplayName: "Joint", SortCode: "11-22-33", Number: "12345678", Opening: 50000})
	tfake.AddTransaction(truelayer.ConnLloyds, "tl-acc-catchup", truelayertest.Transaction{
		Date: now.AddDate(0, 0, -3), Amount: -999, Description: "MADE UP CAFE"})

	cfg := tlEnv(t, super, msrv.URL, tsrv.URL)
	cfg.Timezone = zoneNowIsNoon(t)
	w := &tlWorld{
		world: &world{
			t: t, super: super, cfg: cfg, fake: mfake, stderr: &syncBuffer{},
			client: &http.Client{
				Timeout:       30 * time.Second,
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			},
		},
		tl: tfake, workers: map[string]*truelayer.Worker{},
	}
	if code, _ := w.command(nil, app.Bootstrap); code != app.ExitOK {
		t.Fatalf("bootstrap: %s", w.stderr.String())
	}
	if code, _ := w.command(nil, app.Migrate); code != app.ExitOK {
		t.Fatalf("migrate: %s", w.stderr.String())
	}

	// start runs the service until the returned stop. jobs gives the job names it registered, and
	// may be called only once stop has returned: the scheduler's hook runs on the service's goroutine.
	start := func(hooks app.Hooks) (jobs func() []string, stop func()) {
		var registered []string
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		listening := make(chan net.Addr, 1)
		done := make(chan int, 1)
		hooks.Listening = func(a net.Addr) { listening <- a }
		hooks.TrueLayerWorker = func(tw *truelayer.Worker) {
			w.mu.Lock()
			w.workers[tw.Source()] = tw
			w.mu.Unlock()
		}
		hooks.Scheduler = func(s *scheduler.Scheduler) { registered = s.Names() }
		go func() {
			done <- app.Serve(ctx, app.Env{Cfg: cfg, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: w.stderr, Now: time.Now}, &hooks)
		}()
		select {
		case a := <-listening:
			w.addr = a.String()
		case code := <-done:
			cancel()
			t.Fatalf("serve exited %d before listening: %s", code, w.stderr.String())
		case <-time.After(30 * time.Second):
			cancel()
			t.Fatal("serve did not start listening")
		}
		var stopped atomic.Bool
		stop = func() {
			if !stopped.CompareAndSwap(false, true) {
				return
			}
			cancel()
			select {
			case code := <-done:
				if code != app.ExitOK {
					t.Errorf("serve exit %d: %s", code, w.stderr.String())
				}
			case <-time.After(60 * time.Second):
				t.Fatal("serve did not stop")
			}
		}
		t.Cleanup(stop)
		return func() []string { return registered }, stop
	}

	// Connect Lloyds, with the scheduler off. The connect run's success is the consent's last sync.
	_, stop := start(app.Hooks{NoSchedule: true})
	link := w.connectLink("/connect/truelayer/ob-lloyds")
	code, state, err := tfake.Authorise(link.String())
	if err != nil {
		t.Fatal(err)
	}
	if resp := w.get("/callback/truelayer?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), false); resp.StatusCode != 303 {
		t.Fatalf("Lloyds callback: %d: %s", resp.StatusCode, w.stderr.String())
	}
	w.worker(srcLloyds).Wait()
	stop()
	if outcome := w.queryText(`SELECT outcome FROM sync.run WHERE source = $1 AND kind = 'connect'`, srcLloyds); outcome != "ok" {
		t.Fatalf("the Lloyds connect run ended %q: %s", outcome, w.stderr.String())
	}

	syncRuns := func(source string) int64 {
		return w.queryInt(`SELECT count(*) FROM sync.run WHERE source = $1 AND kind = 'sync'`, source)
	}
	lastSync := func(ago time.Duration) {
		w.queryInt(`UPDATE sync.consent SET last_sync_ok_at = now() - make_interval(secs => $2) WHERE source = $1 AND is_current RETURNING id`,
			srcLloyds, ago.Seconds())
	}
	// slotJobs counts the Lloyds jobs registered: two for the slots, and one more for a catch-up,
	// which runs under the missed slot's name.
	slotJobs := func(jobs []string) int {
		return len(slices.DeleteFunc(slices.Clone(jobs), func(j string) bool { return !strings.HasPrefix(j, srcLloyds+" ") }))
	}
	// idle starts the service and gives a catch-up, which would start within a millisecond, time to
	// show itself; it reports the Lloyds jobs registered.
	idle := func() int {
		t.Helper()
		jobs, stop := start(app.Hooks{CatchUpJitter: time.Millisecond})
		time.Sleep(1500 * time.Millisecond)
		stop()
		return slotJobs(jobs())
	}
	requests := len(tfake.Requests())
	requireQuiet := func(why string, jobs int) {
		t.Helper()
		if jobs != 2 {
			t.Errorf("%s: %d Lloyds jobs registered, want the two slots only", why, jobs)
		}
		if n := syncRuns(srcLloyds) + syncRuns(srcTLMonzo); n != 0 {
			t.Errorf("%s: %d sync runs, want none", why, n)
		}
		if n := len(tfake.Requests()); n != requests {
			t.Errorf("%s: %d bank calls, want none", why, n-requests)
		}
	}

	// AC3: a consent that is lost or expired is not caught up however old its last sync is, and
	// Monzo (TrueLayer), never connected, has nothing to catch up.
	for name, end := range map[string]string{
		"lost":    `state = 'lost', end_reason = 'access_denied'`,
		"expired": `state = 'expired', end_reason = 'expected_expiry_passed'`,
	} {
		lastSync(36 * time.Hour)
		w.queryInt(`UPDATE sync.consent SET `+end+`, ended_at = now() WHERE source = $1 AND is_current RETURNING id`, srcLloyds)
		requireQuiet("a "+name+" consent", idle())
	}
	w.queryInt(`UPDATE sync.consent SET state = 'active', end_reason = NULL, ended_at = NULL WHERE source = $1 AND is_current RETURNING id`, srcLloyds)

	// AC2: a last sync after the most recent slot (06:20, five to seven hours ago) makes no extra sync.
	lastSync(4 * time.Hour)
	requireQuiet("a sync 4 hours ago", idle())

	// AC1: a last sync before that slot, though only eight hours old, is caught up, once.
	lastSync(8 * time.Hour)
	jobs, stop := start(app.Hooks{CatchUpJitter: time.Millisecond})
	deadline := time.Now().Add(30 * time.Second)
	for w.queryInt(`SELECT count(*) FROM sync.run WHERE source = $1 AND kind = 'sync' AND outcome IS NOT NULL`, srcLloyds) != 1 {
		if time.Now().After(deadline) {
			stop()
			t.Fatalf("no catch-up sync finished within 30 s of start: %s", w.stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(time.Second) // a second sync, if there were one, would be under way
	stop()
	if got := slotJobs(jobs()); got != 3 {
		t.Errorf("%d Lloyds jobs registered, want the two slots and the catch-up", got)
	}
	if n := syncRuns(srcLloyds); n != 1 {
		t.Errorf("%d Lloyds sync runs after the start, want 1", n)
	}
	if outcome := w.queryText(`SELECT outcome FROM sync.run WHERE source = $1 AND kind = 'sync'`, srcLloyds); outcome != "ok" {
		t.Errorf("the catch-up run ended %q, want ok", outcome)
	}
	if n := syncRuns(srcTLMonzo); n != 0 {
		t.Errorf("%d Monzo (TrueLayer) sync runs, want none: it has no consent", n)
	}
	if len(tfake.Requests()) == requests {
		t.Error("the catch-up made no bank call")
	}
	if n := w.queryInt(`SELECT count(*) FROM sync.consent WHERE source = $1 AND is_current AND last_sync_ok_at > now() - interval '1 hour'`, srcLloyds); n != 1 {
		t.Errorf("the consent's last successful sync was not brought up to date")
	}

	// It does not repeat: the next start finds the sync current.
	requests = len(tfake.Requests())
	runs := syncRuns(srcLloyds)
	registered := idle()
	if registered != 2 || syncRuns(srcLloyds) != runs || len(tfake.Requests()) != requests {
		t.Errorf("a start after the catch-up ran again: %d Lloyds jobs, %d runs, %d bank calls", registered, syncRuns(srcLloyds)-runs, len(tfake.Requests())-requests)
	}
}
