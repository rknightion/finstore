package truelayer

import (
	"context"

	"github.com/rknightion/finstore/internal/semconv"
)

// How an optional read ended, as finstore.truelayer.optional_reads counts it.
const (
	resultOK          = "ok"
	resultUnsupported = "unsupported"
	resultRefused     = "refused"
	resultRateLimited = "rate_limited"
	resultFailed      = "failed"
)

// optionalRun is the state of one run's optional reads: after a rate limit the rest are not made.
type optionalRun struct{ limited bool }

// stepOptional makes one optional read: a call whose answer never reaches a ledger table. Nothing
// a bank answers to it changes the run's outcome or ends the consent, because a permanently failing
// optional read must not leave every run partial (a partial run never sets last_sync_ok, which the
// stale-sync alert reads) and a working ledger feed must not be lost over optional data. The answer
// is counted and logged by result:
//
//   - 501, or a bank code that says the endpoint is unsupported: info level, unsupported;
//   - 401, 403 or access_denied: warning, refused. The consent is not ended;
//   - 429 or the daily limit: warning, rate_limited, and no further optional read this run;
//   - any other bank failure or unreadable body: warning, failed.
//
// Anything else that stops a run (a store failure, a failed or refused token refresh, a lost
// consent, cancellation, the deadline) is returned, and stops the run as it does for a ledger read.
func (w *Worker) stepOptional(ctx context.Context, run *optionalRun, endpoint string, fn func(context.Context) error) error {
	if run.limited {
		return nil
	}
	err := fn(ctx)
	if err == nil {
		w.metrics.OptionalRead(ctx, w.source, endpoint, resultOK)
		return nil
	}
	rateLimited := !isTokenCall(err) && isRateLimited(err)
	if stops(err) && !rateLimited && !isRefused(err) {
		return err
	}
	var result, msg string
	switch {
	case rateLimited:
		result, msg = resultRateLimited, "truelayer optional read rate limited"
		run.limited = true
	case isUnsupported(err):
		result, msg = resultUnsupported, "truelayer optional read unsupported"
	case isRefused(err):
		result, msg = resultRefused, "truelayer optional read refused"
	default:
		result, msg = resultFailed, "truelayer optional read failed"
	}
	w.metrics.OptionalRead(ctx, w.source, endpoint, result)
	cat := category(err)
	if result == resultRefused || result == resultUnsupported {
		// category would call a refusal consent_lost, which an optional read never causes.
		cat = result
	}
	attrs := []any{semconv.LogEndpoint, endpoint, semconv.LogErrorCategory, cat}
	if result == resultUnsupported {
		w.log.InfoContext(ctx, msg, attrs...)
	} else {
		w.log.WarnContext(ctx, msg, attrs...)
	}
	return nil
}
