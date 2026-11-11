// Package semconv owns every metric name, attribute key and log field the
// service emits. Nothing else declares one.
package semconv

import "slices"

// ServiceName is the value of the service.name resource attribute.
const ServiceName = "finstore"

// Resource attribute keys.
const (
	AttrServiceName           = "service.name"
	AttrServiceVersion        = "service.version"
	AttrServiceInstanceID     = "service.instance.id"
	AttrDeploymentEnvironment = "deployment.environment.name"
)

// Metric names.
const (
	MetricSyncLastSuccessTime    = "finstore.sync.last_success.time"
	MetricSyncRuns               = "finstore.sync.runs"
	MetricBankRequests           = "finstore.bank.requests"
	MetricConsentState           = "finstore.consent.state"
	MetricConsentExpiryTime      = "finstore.consent.expiry.time"
	MetricReconciliationMismatch = "finstore.reconciliation.mismatch"
	MetricImportLastTime         = "finstore.import.last.time"
	MetricDumpLastSuccessTime    = "finstore.dump.last_success.time"
	MetricUp                     = "finstore.up"
	MetricMandatesUnresolved     = "finstore.mandates.unresolved"
	// MetricTrueLayerOptionalReads counts TrueLayer reads whose answer never reaches the ledger, by
	// endpoint and result (AttrResult).
	MetricTrueLayerOptionalReads = "finstore.truelayer.optional_reads"
	// MetricTrueLayerCardsUnlisted is how many card accounts of a source its newest card list left out.
	MetricTrueLayerCardsUnlisted = "finstore.truelayer.cards_unlisted"
)

// Metric attribute keys.
const (
	AttrSource      = "source"
	AttrKind        = "kind"
	AttrOutcome     = "outcome"
	AttrEndpoint    = "endpoint"
	AttrStatusClass = "status_class"
	AttrState       = "state"
	AttrAccount     = "account"
	// AttrResult is how an optional read ended: ok, unsupported, refused, rate_limited or failed.
	AttrResult = "result"
	// AttrErrorCategory is a finished run's error category on finstore.sync.runs, present only
	// when the run recorded one: a fixed category word, never text from a response.
	AttrErrorCategory = "error_category"
)

// Log field keys.
const (
	LogMsg           = "msg"
	LogLevel         = "level"
	LogTime          = "time"
	LogSource        = "source"
	LogEndpoint      = "endpoint"
	LogStatus        = "status"
	LogDurationMS    = "duration_ms"
	LogKind          = "kind"
	LogOutcome       = "outcome"
	LogErrorCategory = "error_category"
	LogRunID         = "run_id"
	LogFetchID       = "fetch_id"
	LogAccount       = "account"
	LogCount         = "count"
	LogPages         = "pages"
	LogRows          = "rows"
	LogState         = "state"
)

var logFields = []string{
	LogMsg, LogLevel, LogTime, LogSource, LogEndpoint, LogStatus, LogDurationMS,
	LogKind, LogOutcome, LogErrorCategory, LogRunID, LogFetchID, LogAccount,
	LogCount, LogPages, LogRows, LogState,
}

// LogFields returns the log field keys a log line may carry.
func LogFields() []string { return slices.Clone(logFields) }

// IsLogField reports whether key is an allowed log field.
func IsLogField(key string) bool { return slices.Contains(logFields, key) }
