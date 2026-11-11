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
	// AttrProcessRole is "serve" for the long-running service and "cli" for a one-off command.
	AttrProcessRole = "finstore.process.role"
	// AttrCommand names the CLI command a process runs, such as "account add". It is absent on serve.
	AttrCommand = "finstore.command"
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

// Metric names declared for the telemetry work that follows the telemetry core. Later changes
// record them; none is recorded yet except where noted.
const (
	// MetricBankRequestDuration is a histogram (s) of one bank attempt, by source, endpoint and
	// status_class ("no_response" when the attempt got no HTTP response).
	MetricBankRequestDuration = "finstore.bank.request.duration"
	// MetricBankRetries counts bank retries by source, endpoint and reason (AttrReason).
	MetricBankRetries = "finstore.bank.retries"
	// MetricSyncRunDuration is a histogram (s) of a finished run, by source, kind and outcome.
	MetricSyncRunDuration = "finstore.sync.run.duration"
	// MetricSchedulerJobRuns counts scheduler job runs by job and outcome.
	MetricSchedulerJobRuns = "finstore.scheduler.job.runs"
	// MetricSchedulerJobDuration is a histogram (s) of a scheduler job run, by job.
	MetricSchedulerJobDuration = "finstore.scheduler.job.duration"
	// MetricSchedulerJobLastOK is 1 or 0 for the job's last run, held in the serve process.
	MetricSchedulerJobLastOK = "finstore.scheduler.job.last_ok"
	// MetricWriterLockHeld is an observable gauge (s) of how long the writer lock has been held,
	// by holder (AttrHolder). It reports nothing while the lock is free.
	MetricWriterLockHeld = "finstore.store.writer_lock.held"
	// MetricProcessStartTime is the Unix time serve started. Recorded by serve only.
	MetricProcessStartTime = "finstore.process.start.time"
	// MetricTelemetryExportFailures counts failed exports by signal (AttrSignal).
	MetricTelemetryExportFailures = "finstore.telemetry.export.failures"
	// MetricTelemetrySpanAttributesDropped counts span attribute keys the exporter allow-list
	// removed, by key (AttrKey).
	MetricTelemetrySpanAttributesDropped = "finstore.telemetry.span_attributes.dropped"
	// MetricSyncLastRunOK is 1 when the latest finished run of a source and kind ended ok, else 0.
	MetricSyncLastRunOK = "finstore.sync.last_run.ok"
	// MetricSyncLastRunTime is when that run finished (Unix seconds).
	MetricSyncLastRunTime = "finstore.sync.last_run.time"
	// MetricSyncLastRateLimitedTime is when a source's newest rate-limited run finished.
	MetricSyncLastRateLimitedTime = "finstore.sync.last_rate_limited.time"
	// MetricConsentHistoryOK is 1 when the current consent's history fetch ended ok, else 0.
	MetricConsentHistoryOK = "finstore.consent.history_ok"
	// MetricConnectUnanswered counts connect attempts that expired unused in the last 24 hours.
	MetricConnectUnanswered = "finstore.connect.unanswered"
	// MetricMCPToolCalls counts MCP tool calls by tool (AttrTool), outcome (AttrOutcome, one of the
	// MCPOutcome values) and client (AttrClient, the grant group the caller belongs to). Latency
	// comes from the HTTP route histogram and the tool call span; there is no MCP histogram.
	MetricMCPToolCalls = "finstore.mcp.tool.calls"
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
	// AttrResult is how an optional read ended: ok, unsupported, refused, rate_limited, failed or
	// ResultSCARequired.
	AttrResult = "result"
	// AttrErrorCategory is a finished run's error category on finstore.sync.runs, present only
	// when the run recorded one: a fixed category word, never text from a response.
	AttrErrorCategory = "error_category"
	// AttrReason is why a bank call was retried: rate_limited, server_error, timeout or network.
	AttrReason = "reason"
	// AttrJob is a scheduler job's registered name, on the scheduler metrics. It is the same key as
	// the span attribute SpanJob and is not called plain "job" on purpose: Grafana Cloud's OTLP
	// gateway sets the Prometheus job label from the service, so a metric attribute named job is
	// overwritten and every job reads as one series. finstore.job is queried as finstore_job.
	AttrJob = SpanJob
	// AttrHolder is who holds the writer lock; the values are fixed words chosen in code.
	AttrHolder = "holder"
	// AttrSignal is the telemetry signal an export carried: traces, metrics or logs.
	AttrSignal = "signal"
	// AttrKey is a span attribute key the exporter allow-list removed. Keys come from code, never
	// from data.
	AttrKey = "key"
	// AttrTool is an MCP tool's name, one of the MCPTool values: a name the caller sent that is
	// not one of them is MCPToolUnknown, so the series stay bounded.
	AttrTool = "tool"
	// AttrClient is the tailnet grant group a caller of /mcp belongs to, as the grant names it.
	AttrClient = "client"
)

// ResultSCARequired is the AttrResult of a TrueLayer mandate read, during a scheduled sync, that
// the bank refused with 403 sca_exceeded: its answer is protected and readable only shortly after
// the owner authenticates. It is expected on every scheduled sync of a bank that does this, and is
// neither a failure nor a refusal of the consent.
const ResultSCARequired = "sca_required"

// StatusClassNoResponse is the status_class of a bank attempt with no usable HTTP response: none
// arrived (a transport failure or a timeout), its body could not be read, or its status was
// outside 100 to 599. finstore.bank.requests calls the same thing "none".
const StatusClassNoResponse = "no_response"

// Values of AttrReason: why a bank call was retried.
const (
	RetryReasonRateLimited = "rate_limited"
	RetryReasonServerError = "server_error"
	RetryReasonTimeout     = "timeout"
	RetryReasonNetwork     = "network"
)

// Signal values for AttrSignal.
const (
	SignalTraces  = "traces"
	SignalMetrics = "metrics"
	SignalLogs    = "logs"
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
	// The MCP tool call line: the tool, the grant group of the caller (LogClient) and its tailnet
	// address when the proxy supplies one (LogPeer), whether the result was cut short, the curated
	// tool's input as one JSON string (LogArgs) and the SQL of a query_sql call (LogSQL), both cut
	// to a fixed length.
	LogTool      = "tool"
	LogClient    = "client"
	LogPeer      = "peer"
	LogTruncated = "truncated"
	LogArgs      = "args"
	LogSQL       = "sql"
	// LogStack is the stack of a recovered panic: code locations, never the panic value.
	LogStack = "stack"
)

var logFields = []string{
	LogMsg, LogLevel, LogTime, LogSource, LogEndpoint, LogStatus, LogDurationMS,
	LogKind, LogOutcome, LogErrorCategory, LogRunID, LogFetchID, LogAccount,
	LogCount, LogPages, LogRows, LogState, LogTool, LogClient, LogPeer, LogTruncated, LogArgs, LogSQL, LogStack,
}

// LogFields returns the log field keys a log line may carry.
func LogFields() []string { return slices.Clone(logFields) }

// IsLogField reports whether key is an allowed log field.
func IsLogField(key string) bool { return slices.Contains(logFields, key) }

// Span names. Every span a finstore package starts is named here; libraries name their own.
const (
	SpanConnect           = "connect"
	SpanCallback          = "callback"
	SpanSchedulerJob      = "scheduler.job"
	SpanSyncRun           = "sync.run"
	SpanMonzoFetchRange   = "monzo.fetch_range"
	SpanResume            = "resume"
	SpanWriterLock        = "store.writer_lock"
	SpanLedgerCatchUp     = "ledger.catch_up"
	SpanRenormaliseCreate = "ledger.renormalise.create"
	SpanRenormaliseCatch  = "ledger.renormalise.catch_up"
	SpanRenormaliseDiff   = "ledger.renormalise.diff"
	SpanRenormaliseSwap   = "ledger.renormalise.swap"
	SpanCaptureDirectory  = "capture.directory"
	SpanCaptureFetch      = "capture.fetch"
)

// SpanMCPToolsCallPrefix starts the name of an MCP tool call's span: "tools/call overview". The
// OpenTelemetry convention for an MCP server span names it "{mcp.method.name} {target}" with the
// tool as the target; a call whose tool is not one of the MCPTool values is named SpanMCPMethodToolsCall
// alone, because a name the caller chose is not a low-cardinality target.
const (
	SpanMCPMethodToolsCall = "tools/call"
	SpanMCPToolsCallPrefix = SpanMCPMethodToolsCall + " "
)

// SpanBankCallPrefix starts the name of a logical bank call's span: "bank GET /accounts". An
// attempt's span is named "GET /accounts", with no prefix.
const SpanBankCallPrefix = "bank "

// SpanCommandPrefix starts the name of a CLI command's root span: "finstore account add".
const SpanCommandPrefix = "finstore "

// SpanMCPQuery names every database span of the MCP reader (store.ForAgentSQL): a constant, so the
// text of an agent's query never becomes a span name.
const SpanMCPQuery = "mcp.query"

// Span attribute keys that are not finstore's own. The names follow the OpenTelemetry semantic
// conventions; the ones libraries set are listed so the exporter allow-list can keep them.
const (
	SpanHTTPRequestMethod         = "http.request.method"
	SpanHTTPRequestMethodOriginal = "http.request.method_original"
	SpanHTTPResponseStatusCode    = "http.response.status_code"
	SpanHTTPRoute                 = "http.route"
	SpanURLPath                   = "url.path"
	SpanURLScheme                 = "url.scheme"
	SpanURLTemplate               = "url.template"
	SpanURLFull                   = "url.full"
	SpanServerAddress             = "server.address"
	SpanServerPort                = "server.port"
	SpanNetworkPeerAddress        = "network.peer.address"
	SpanNetworkPeerPort           = "network.peer.port"
	SpanNetworkProtocolName       = "network.protocol.name"
	SpanNetworkProtocolVersion    = "network.protocol.version"
	SpanClientAddress             = "client.address"
	SpanUserAgentOriginal         = "user_agent.original"
	SpanHTTPRequestResendCount    = "http.request.resend_count"
	SpanHTTPRequestBodySize       = "http.request.body.size"
	SpanHTTPResponseBodySize      = "http.response.body.size"
	SpanErrorType                 = "error.type"
	SpanDBSystemName              = "db.system.name"
	SpanDBNamespace               = "db.namespace"
	SpanDBQueryText               = "db.query.text"
	SpanDBOperationName           = "db.operation.name"
	SpanDBCollectionName          = "db.collection.name"
	SpanDBOperationBatchSize      = "db.operation.batch.size"
	SpanUserName                  = "user.name"
	SpanPgxOperationType          = "pgx.operation.type"
	SpanPgxSQLState               = "pgx.sql_state"
	// SpanPgxRowsAffected is a count; SpanPgxPrepareStmtName is "stmtcache_" and a hash of
	// constant SQL (finstore never names a statement itself). Neither holds a value.
	SpanPgxRowsAffected    = "pgx.rows_affected"
	SpanPgxPrepareStmtName = "pgx.prepare_stmt.name"
	SpanProcessExitCode    = "process.exit.code"
)

// MCP span attribute keys, as the OpenTelemetry semantic conventions for MCP and GenAI tool
// execution name them. The registry released as semantic-conventions v1.45.0 lists them as moved to
// open-telemetry/semantic-conventions-genai, which has made no release; the names are the ones both
// carry.
const (
	SpanMCPMethodName      = "mcp.method.name"
	SpanMCPProtocolVersion = "mcp.protocol.version"
	SpanGenAIToolName      = "gen_ai.tool.name"
	SpanGenAIOperationName = "gen_ai.operation.name"
)

// GenAIOperationExecuteTool is gen_ai.operation.name on a tool call span.
const GenAIOperationExecuteTool = "execute_tool"

// ErrorTypeToolError is error.type on a tool call span whose result is a tool error.
const ErrorTypeToolError = "tool_error"

// finstore's own span attribute keys.
const (
	SpanSource         = "finstore.source"
	SpanConnectOutcome = "finstore.connect.outcome"
	SpanJob            = "finstore.job"
	SpanJobOutcome     = "finstore.job.outcome"
	SpanRunKind        = "finstore.run.kind"
	SpanRunID          = "finstore.run.id"
	SpanRunOutcome     = "finstore.run.outcome"
	SpanRunPages       = "finstore.run.pages"
	SpanRunRows        = "finstore.run.rows"
	SpanRangeStart     = "finstore.range.start"
	SpanRangeEnd       = "finstore.range.end"
	SpanBankCode       = "finstore.bank.code"
	SpanLockHolder     = "finstore.lock.holder"
	SpanLockWaitMS     = "finstore.lock.wait_ms"
	SpanCount          = "finstore.count"
	SpanCommand        = AttrCommand
	// SpanMCPClient is the grant group of the caller; SpanMCPRows is how many rows or items the
	// call returned; SpanMCPTruncated is whether the result was cut at a limit. None holds SQL or
	// a value from a row.
	SpanMCPClient    = "finstore.mcp.client"
	SpanMCPRows      = "finstore.mcp.rows"
	SpanMCPTruncated = "finstore.mcp.truncated"
)

// Values of finstore.connect.outcome on a connect or callback span.
const (
	// ConnectCrossSite: a connect post that did not come from the status page (400).
	ConnectCrossSite = "cross_site"
	// ConnectStartFailed: the attempt could not be started or gave no usable bank link (500).
	ConnectStartFailed = "start_failed"
	// ConnectStarted: the page that moves the browser on to the bank was served (200).
	ConnectStarted = "started"
	// ConnectInvalidLink: the callback had no state or no code (400).
	ConnectInvalidLink = "invalid_link"
	// ConnectBankDeclined: the shared callback came back with the bank's error parameter (400).
	ConnectBankDeclined = "bank_declined"
	// ConnectRefused: the state was unknown, expired, used or another source's (400).
	ConnectRefused = "refused"
	// ConnectFailed: the attempt could not be read (503) or the exchange failed (502). The span's
	// error.type says which (ErrorAttemptRead or ErrorExchange).
	ConnectFailed = "failed"
	// ConnectExchanged: the connector succeeded (303).
	ConnectExchanged = "exchanged"
)

// SourceUnknown is finstore.source on the shared TrueLayer callback until the attempt's source has
// been read, and for a request that never reaches it.
const SourceUnknown = "unknown"

// Values of error.type on a connect or callback span that failed.
const (
	ErrorAttemptRead = "attempt_read"
	ErrorExchange    = "exchange"
)

// Values of finstore.job.outcome on a scheduler.job span and of the outcome attribute on
// finstore.scheduler.job.runs.
const (
	JobOK    = "ok"
	JobError = "error"
	JobPanic = "panic"
	// JobCancelled: the job returned an error after the scheduler's context was cancelled, which is
	// serve shutting down, not a failure. It is counted but never changes last_ok and never marks
	// the span failed.
	JobCancelled = "cancelled"
)

// spanAttributes is the span exporter's allow-list. pgx.query.parameters is deliberately absent,
// as is every header, query string and exception key: a library upgrade that adds a key shows up
// as a dropped-key count, never as a leak.
var spanAttributes = []string{
	SpanHTTPRequestMethod, SpanHTTPRequestMethodOriginal, SpanHTTPResponseStatusCode, SpanHTTPRoute,
	SpanURLPath, SpanURLScheme, SpanURLTemplate, SpanURLFull, SpanServerAddress, SpanServerPort,
	SpanNetworkPeerAddress, SpanNetworkPeerPort, SpanNetworkProtocolName, SpanNetworkProtocolVersion,
	SpanClientAddress, SpanUserAgentOriginal, SpanHTTPRequestResendCount, SpanHTTPRequestBodySize,
	SpanHTTPResponseBodySize, SpanErrorType, SpanDBSystemName, SpanDBNamespace, SpanDBQueryText,
	SpanDBOperationName, SpanDBCollectionName, SpanDBOperationBatchSize, SpanUserName,
	SpanPgxOperationType, SpanPgxSQLState, SpanPgxRowsAffected, SpanPgxPrepareStmtName, SpanProcessExitCode,
	SpanSource, SpanConnectOutcome, SpanJob, SpanJobOutcome, SpanRunKind, SpanRunID, SpanRunOutcome,
	SpanRunPages, SpanRunRows, SpanRangeStart, SpanRangeEnd, SpanBankCode, SpanLockHolder,
	SpanLockWaitMS, SpanCount, SpanCommand,
	SpanMCPMethodName, SpanMCPProtocolVersion, SpanGenAIToolName, SpanGenAIOperationName,
	SpanMCPClient, SpanMCPRows, SpanMCPTruncated,
}

// SpanAttributes returns the span attribute keys the exporter allows.
func SpanAttributes() []string { return slices.Clone(spanAttributes) }

// IsSpanAttribute reports whether key may leave the process on a span.
func IsSpanAttribute(key string) bool { return slices.Contains(spanAttributes, key) }

// IsStatusCategory reports whether word is a fixed error category that may be a span's status
// description: the bank client's categories, the run categories, the ledger's rejection
// categories and database_error. Anything else in a status is free text, which the exporter
// replaces.
func IsStatusCategory(word string) bool { return slices.Contains(statusCategories, word) }

// statusCategories are the words finstore code passes to telemetry.Fail. The bank client's
// categories and the run, ledger and connect categories are fixed words; none carries anything
// from a response, a file or a request.
var statusCategories = []string{
	// bankhttp.Category.
	"timeout", "network", "rate_limited", "server_error", "unauthorised",
	"forbidden_insufficient_permissions", "forbidden_verification_required", "bad_request",
	"unexpected_status",
	// Run categories (monzo, truelayer, consent).
	"history_failed", "consent_lost", "superseded", "interrupted", "card_key_shared", "cards_refused",
	"window_closed", "timestamp_group_over_page", "backfill_started", "unexpected_body", "cancelled", "internal",
	// Import and ledger rejections.
	"check_failed", "account_mismatch", "unknown_format", "rejected_before",
	// Connect and callback failures.
	"attempt_read", "exchange",
	// MCP tool call outcomes that fail a span (timeout and error are listed above).
	"invalid", "busy", "refused",
	// Other.
	"database_error", "error",
}

// MCP tool names. A tool call span, metric and log line name the tool as one of these, or as
// MCPToolUnknown for a call to a tool the server does not have.
const (
	MCPToolOverview           = "overview"
	MCPToolSearchTransactions = "search_transactions"
	MCPToolSpendingSummary    = "spending_summary"
	MCPToolDescribeSchema     = "describe_schema"
	MCPToolQuerySQL           = "query_sql"
	MCPToolUnknown            = "unknown"
)

var mcpTools = []string{
	MCPToolOverview, MCPToolSearchTransactions, MCPToolSpendingSummary, MCPToolDescribeSchema, MCPToolQuerySQL,
}

// MCPTools returns the names of the five tools, in the order the server lists them.
func MCPTools() []string { return slices.Clone(mcpTools) }

// IsMCPTool reports whether name is one of the server's tools.
func IsMCPTool(name string) bool { return slices.Contains(mcpTools, name) }

// MCPClientUnknown is the client of a tool call that reached the server without a grant group: the
// gate always supplies one, so this is a call made in process.
const MCPClientUnknown = "unknown"

// Outcomes of an MCP tool call: the outcome attribute of finstore.mcp.tool.calls, the outcome log
// field, and, for every one but ok, the status description of the tool call span. MCPOutcomeTimeout
// and MCPOutcomeError are also in the status category list.
const (
	// MCPOutcomeOK: the call succeeded, truncated or not.
	MCPOutcomeOK = "ok"
	// MCPOutcomeInvalid: the input failed validation, or named no such tool.
	MCPOutcomeInvalid = "invalid"
	// MCPOutcomeBusy: the wait for the one database connection ran out.
	MCPOutcomeBusy = "busy"
	// MCPOutcomeTimeout: the statement or the call as a whole ran out of time.
	MCPOutcomeTimeout = "timeout"
	// MCPOutcomeRefused: Postgres refused the statement for a privilege, a read-only transaction or
	// its syntax (a second statement included).
	MCPOutcomeRefused = "refused"
	// MCPOutcomeError: anything else.
	MCPOutcomeError = "error"
)
