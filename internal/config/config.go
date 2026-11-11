// Package config reads the FINSTORE_ environment variables. There is no
// configuration file.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Variable names. They are names, not credentials.
//
//nolint:gosec // G101 matches the words password, secret and token in names.
const (
	EnvDatabaseURL           = "FINSTORE_DATABASE_URL"
	EnvOwnerDatabaseURL      = "FINSTORE_OWNER_DATABASE_URL"
	EnvSuperuserDatabaseURL  = "FINSTORE_SUPERUSER_DATABASE_URL"
	EnvAppPassword           = "FINSTORE_APP_PASSWORD"
	EnvOwnerPassword         = "FINSTORE_OWNER_PASSWORD"
	EnvListenAddr            = "FINSTORE_LISTEN_ADDR"
	EnvPublicURL             = "FINSTORE_PUBLIC_URL"
	EnvAllowedEmails         = "FINSTORE_ALLOWED_EMAILS"
	EnvAuthHeader            = "FINSTORE_AUTH_HEADER"
	EnvMonzoClientID         = "FINSTORE_MONZO_CLIENT_ID"
	EnvMonzoClientSecret     = "FINSTORE_MONZO_CLIENT_SECRET"
	EnvMonzoAPIURL           = "FINSTORE_MONZO_API_URL"
	EnvMonzoAuthURL          = "FINSTORE_MONZO_AUTH_URL"
	EnvMonzoSyncInterval     = "FINSTORE_MONZO_SYNC_INTERVAL"
	EnvMonzoNightlyAt        = "FINSTORE_MONZO_NIGHTLY_AT"
	EnvMonzoBackfillConc     = "FINSTORE_MONZO_BACKFILL_CONCURRENCY"
	EnvMonzoWindow           = "FINSTORE_MONZO_WINDOW"
	EnvTrueLayerClientID     = "FINSTORE_TRUELAYER_CLIENT_ID"
	EnvTrueLayerClientSecret = "FINSTORE_TRUELAYER_CLIENT_SECRET"
	EnvTrueLayerAuthURL      = "FINSTORE_TRUELAYER_AUTH_URL"
	EnvTrueLayerAPIURL       = "FINSTORE_TRUELAYER_API_URL"
	EnvTimezone              = "FINSTORE_TIMEZONE"
	EnvDumpStatusFile        = "FINSTORE_DUMP_STATUS_FILE"
	EnvOTLPEndpoint          = "FINSTORE_OTLP_ENDPOINT"
	EnvOTLPInstanceID        = "FINSTORE_OTLP_INSTANCE_ID"
	EnvOTLPToken             = "FINSTORE_OTLP_TOKEN"
	EnvEnvironment           = "FINSTORE_ENVIRONMENT"
	EnvLogLevel              = "FINSTORE_LOG_LEVEL"
	// EnvOTelSDKDisabled is the OpenTelemetry standard switch, not a FINSTORE_ variable: the
	// word true gives no-op telemetry providers.
	EnvOTelSDKDisabled = "OTEL_SDK_DISABLED"
)

// TimeOfDay is a wall-clock time, in the configured zone, without a date.
type TimeOfDay struct{ Hour, Minute int }

// Config holds every setting. Its formatted and logged forms redact secrets.
type Config struct {
	DatabaseURL          string
	OwnerDatabaseURL     string
	SuperuserDatabaseURL string
	AppPassword          string
	OwnerPassword        string

	ListenAddr    string
	PublicURL     string
	AllowedEmails []string
	AuthHeader    string

	MonzoClientID     string
	MonzoClientSecret string
	MonzoAPIURL       string
	MonzoAuthURL      string
	MonzoSyncInterval time.Duration
	MonzoNightlyAt    TimeOfDay
	MonzoBackfillConc int
	MonzoWindow       time.Duration

	// TrueLayer is configured when its client id and secret are both set; Load refuses one
	// without the other.
	TrueLayerClientID     string
	TrueLayerClientSecret string
	TrueLayerAuthURL      string
	TrueLayerAPIURL       string

	Timezone       *time.Location
	DumpStatusFile string

	OTLPEndpoint   string
	OTLPInstanceID string
	OTLPToken      string
	Environment    string
	// OTelSDKDisabled is set by OTEL_SDK_DISABLED=true: every telemetry provider is a no-op.
	OTelSDKDisabled bool

	LogLevel slog.Level
}

// FromEnv loads the configuration from the process environment.
func FromEnv() (Config, error) { return Load(os.Getenv) }

// Load builds a Config from getenv, applying defaults. Every variable that is
// set must parse, whatever the command; presence is checked by Require.
func Load(getenv func(string) string) (Config, error) {
	get := func(name, def string) string {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			return v
		}
		return def
	}
	c := Config{
		DatabaseURL:           get(EnvDatabaseURL, ""),
		OwnerDatabaseURL:      get(EnvOwnerDatabaseURL, ""),
		SuperuserDatabaseURL:  get(EnvSuperuserDatabaseURL, ""),
		AppPassword:           get(EnvAppPassword, ""),
		OwnerPassword:         get(EnvOwnerPassword, ""),
		ListenAddr:            get(EnvListenAddr, ":8080"),
		PublicURL:             get(EnvPublicURL, ""),
		AuthHeader:            get(EnvAuthHeader, "X-Auth-Request-Email"),
		MonzoClientID:         get(EnvMonzoClientID, ""),
		MonzoClientSecret:     get(EnvMonzoClientSecret, ""),
		MonzoAPIURL:           get(EnvMonzoAPIURL, "https://api.monzo.com"),
		MonzoAuthURL:          get(EnvMonzoAuthURL, "https://auth.monzo.com/"),
		TrueLayerClientID:     get(EnvTrueLayerClientID, ""),
		TrueLayerClientSecret: get(EnvTrueLayerClientSecret, ""),
		TrueLayerAuthURL:      get(EnvTrueLayerAuthURL, "https://auth.truelayer.com"),
		TrueLayerAPIURL:       get(EnvTrueLayerAPIURL, "https://api.truelayer.com"),
		DumpStatusFile:        get(EnvDumpStatusFile, ""),
		OTLPEndpoint:          get(EnvOTLPEndpoint, ""),
		OTLPInstanceID:        get(EnvOTLPInstanceID, ""),
		OTLPToken:             get(EnvOTLPToken, ""),
		Environment:           get(EnvEnvironment, "dev"),
		OTelSDKDisabled:       strings.EqualFold(get(EnvOTelSDKDisabled, ""), "true"),
	}
	for _, e := range strings.Split(getenv(EnvAllowedEmails), ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			c.AllowedEmails = append(c.AllowedEmails, e)
		}
	}

	var errs []error
	bad := func(name string, err error) {
		errs = append(errs, fmt.Errorf("%s: %w", name, err))
	}
	var err error
	if c.MonzoSyncInterval, err = parsePositiveDuration(get(EnvMonzoSyncInterval, "1h")); err != nil {
		bad(EnvMonzoSyncInterval, err)
	}
	if c.MonzoWindow, err = parsePositiveDuration(get(EnvMonzoWindow, "240s")); err != nil {
		bad(EnvMonzoWindow, err)
	}
	if c.MonzoNightlyAt, err = parseTimeOfDay(get(EnvMonzoNightlyAt, "03:20")); err != nil {
		bad(EnvMonzoNightlyAt, err)
	}
	if c.MonzoBackfillConc, err = strconv.Atoi(get(EnvMonzoBackfillConc, "6")); err != nil || c.MonzoBackfillConc < 1 {
		bad(EnvMonzoBackfillConc, errors.New("must be a positive integer"))
	}
	if c.Timezone, err = time.LoadLocation(get(EnvTimezone, "Europe/London")); err != nil {
		bad(EnvTimezone, errors.New("unknown time zone"))
	}
	if (c.TrueLayerClientID == "") != (c.TrueLayerClientSecret == "") {
		errs = append(errs, fmt.Errorf("%s and %s must be set together, or neither", EnvTrueLayerClientID, EnvTrueLayerClientSecret))
	}
	if c.LogLevel, err = parseLevel(get(EnvLogLevel, "info")); err != nil {
		bad(EnvLogLevel, err)
	}
	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return c, nil
}

// TrueLayerEnabled reports whether the TrueLayer connections are configured.
func (c Config) TrueLayerEnabled() bool {
	return c.TrueLayerClientID != "" && c.TrueLayerClientSecret != ""
}

// parse errors deliberately drop the text of the input, which could be a value.
func parsePositiveDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, errors.New("must be a positive duration such as 90s or 1h")
	}
	return d, nil
}

func parseTimeOfDay(s string) (TimeOfDay, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return TimeOfDay{}, errors.New("must be HH:MM")
	}
	return TimeOfDay{Hour: t.Hour(), Minute: t.Minute()}, nil
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, errors.New("must be debug, info, warn or error")
}

// requirements lists, in table order, the variables with no default that each
// command needs.
var requirements = map[string][]string{
	"serve": {
		EnvDatabaseURL, EnvPublicURL, EnvAllowedEmails,
		EnvMonzoClientID, EnvMonzoClientSecret,
	},
	"migrate":      {EnvOwnerDatabaseURL},
	"renormalise":  {EnvOwnerDatabaseURL},
	"bootstrap":    {EnvSuperuserDatabaseURL, EnvAppPassword, EnvOwnerPassword},
	"import":       {EnvDatabaseURL},
	"account":      {EnvDatabaseURL},
	"load-capture": {EnvDatabaseURL},
	"coverage":     {EnvDatabaseURL},
	"healthcheck":  {},
}

// Require returns an error naming the first variable command needs and does
// not have. It never includes a value.
func (c Config) Require(command string) error {
	names, ok := requirements[command]
	if !ok {
		return fmt.Errorf("unknown command %q", command)
	}
	for _, name := range names {
		if c.value(name) == "" {
			return fmt.Errorf("%s is required by %s", name, command)
		}
	}
	return nil
}

func (c Config) value(name string) string {
	switch name {
	case EnvDatabaseURL:
		return c.DatabaseURL
	case EnvOwnerDatabaseURL:
		return c.OwnerDatabaseURL
	case EnvSuperuserDatabaseURL:
		return c.SuperuserDatabaseURL
	case EnvAppPassword:
		return c.AppPassword
	case EnvOwnerPassword:
		return c.OwnerPassword
	case EnvPublicURL:
		return c.PublicURL
	case EnvAllowedEmails:
		return strings.Join(c.AllowedEmails, ",")
	case EnvMonzoClientID:
		return c.MonzoClientID
	case EnvMonzoClientSecret:
		return c.MonzoClientSecret
	}
	return ""
}

// String formats the configuration with every secret and address redacted. It
// backs %v, %+v and %s; GoString covers %#v.
func (c Config) String() string {
	set := func(v string) string {
		if v == "" {
			return "unset"
		}
		return "[redacted]"
	}
	return fmt.Sprintf("Config{listen=%q environment=%q log_level=%s sync_interval=%s nightly_at=%02d:%02d "+
		"backfill_concurrency=%d window=%s otlp_export=%t database_url=%s owner_database_url=%s "+
		"superuser_database_url=%s app_password=%s owner_password=%s monzo_client_secret=%s truelayer_client_secret=%s otlp_token=%s allowed_emails=%d}",
		c.ListenAddr, c.Environment, c.LogLevel, c.MonzoSyncInterval, c.MonzoNightlyAt.Hour, c.MonzoNightlyAt.Minute,
		c.MonzoBackfillConc, c.MonzoWindow, c.OTLPEndpoint != "", set(c.DatabaseURL), set(c.OwnerDatabaseURL),
		set(c.SuperuserDatabaseURL), set(c.AppPassword), set(c.OwnerPassword), set(c.MonzoClientSecret), set(c.TrueLayerClientSecret), set(c.OTLPToken),
		len(c.AllowedEmails))
}

// GoString keeps %#v from dumping the fields.
func (c Config) GoString() string { return c.String() }

// LogValue keeps slog from reflecting over the fields.
func (c Config) LogValue() slog.Value { return slog.StringValue(c.String()) }
