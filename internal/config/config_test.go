package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":8080" || c.AuthHeader != "X-Auth-Request-Email" ||
		c.MonzoAPIURL != "https://api.monzo.com" || c.MonzoAuthURL != "https://auth.monzo.com/" ||
		c.Environment != "dev" || c.DumpStatusFile != "" || c.OTLPEndpoint != "" {
		t.Errorf("string defaults wrong: %v", c)
	}
	if c.MonzoSyncInterval != time.Hour || c.MonzoWindow != 240*time.Second || c.MonzoBackfillConc != 6 {
		t.Errorf("numeric defaults wrong: %v", c)
	}
	if c.MonzoNightlyAt != (TimeOfDay{3, 20}) {
		t.Errorf("nightly default = %+v", c.MonzoNightlyAt)
	}
	if c.Timezone.String() != "Europe/London" || c.LogLevel != slog.LevelInfo {
		t.Errorf("zone/level defaults wrong: %v %v", c.Timezone, c.LogLevel)
	}
}

func TestLoadParsesValues(t *testing.T) {
	c, err := Load(env(map[string]string{
		EnvAllowedEmails:     " A@Example.com, ,b@EXAMPLE.COM ",
		EnvMonzoSyncInterval: "15m",
		EnvMonzoWindow:       "90s",
		EnvMonzoNightlyAt:    "23:05",
		EnvMonzoBackfillConc: "2",
		EnvTimezone:          "America/New_York",
		EnvLogLevel:          "WARN",
		EnvEnvironment:       "prod",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.AllowedEmails, "|"); got != "a@example.com|b@example.com" {
		t.Errorf("emails = %q", got)
	}
	if c.MonzoSyncInterval != 15*time.Minute || c.MonzoWindow != 90*time.Second ||
		c.MonzoNightlyAt != (TimeOfDay{23, 5}) || c.MonzoBackfillConc != 2 ||
		c.Timezone.String() != "America/New_York" || c.LogLevel != slog.LevelWarn || c.Environment != "prod" {
		t.Errorf("parsed config wrong: %v", c)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	const marker = "BADVALUE-MARKER"
	tests := []struct {
		name, key, val string
	}{
		{"interval", EnvMonzoSyncInterval, marker},
		{"interval zero", EnvMonzoSyncInterval, "0s"},
		{"window", EnvMonzoWindow, "-5s"},
		{"nightly text", EnvMonzoNightlyAt, marker},
		{"nightly hour", EnvMonzoNightlyAt, "24:00"},
		{"nightly seconds", EnvMonzoNightlyAt, "03:20:00"},
		{"concurrency text", EnvMonzoBackfillConc, marker},
		{"concurrency zero", EnvMonzoBackfillConc, "0"},
		{"timezone", EnvTimezone, marker},
		{"level", EnvLogLevel, marker},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(env(map[string]string{tt.key: tt.val}))
			if err == nil {
				t.Fatal("want error")
			}
			if !strings.Contains(err.Error(), tt.key) {
				t.Errorf("error %q does not name %s", err, tt.key)
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("error leaks the value: %q", err)
			}
		})
	}
}

func TestRequire(t *testing.T) {
	full := map[string]string{
		EnvDatabaseURL:          "x",
		EnvOwnerDatabaseURL:     "x",
		EnvSuperuserDatabaseURL: "x",
		EnvAppPassword:          "x",
		EnvOwnerPassword:        "x",
		EnvPublicURL:            "x",
		EnvAllowedEmails:        "a@example.com",
		EnvMonzoClientID:        "x",
		EnvMonzoClientSecret:    "x",
	}
	without := func(keys ...string) map[string]string {
		m := map[string]string{}
		for k, v := range full {
			m[k] = v
		}
		for _, k := range keys {
			delete(m, k)
		}
		return m
	}
	tests := []struct {
		command string
		env     map[string]string
		wantVar string // empty: no error
	}{
		{"serve", full, ""},
		{"serve", without(EnvDatabaseURL), EnvDatabaseURL},
		{"serve", without(EnvPublicURL), EnvPublicURL},
		{"serve", without(EnvAllowedEmails), EnvAllowedEmails},
		{"serve", without(EnvMonzoClientID), EnvMonzoClientID},
		{"serve", without(EnvMonzoClientSecret), EnvMonzoClientSecret},
		{"serve", without(EnvPublicURL, EnvMonzoClientSecret), EnvPublicURL},
		{"serve", without(EnvOwnerDatabaseURL, EnvSuperuserDatabaseURL, EnvAppPassword), ""},
		{"migrate", full, ""},
		{"migrate", without(EnvOwnerDatabaseURL), EnvOwnerDatabaseURL},
		{"migrate", without(EnvDatabaseURL), ""},
		{"renormalise", without(EnvOwnerDatabaseURL), EnvOwnerDatabaseURL},
		{"bootstrap", without(EnvSuperuserDatabaseURL), EnvSuperuserDatabaseURL},
		{"bootstrap", without(EnvAppPassword), EnvAppPassword},
		{"bootstrap", without(EnvOwnerPassword), EnvOwnerPassword},
		{"bootstrap", without(EnvDatabaseURL, EnvOwnerDatabaseURL), ""},
		{"import", without(EnvDatabaseURL), EnvDatabaseURL},
		{"account", without(EnvDatabaseURL), EnvDatabaseURL},
		{"load-capture", without(EnvDatabaseURL), EnvDatabaseURL},
		{"coverage", without(EnvDatabaseURL), EnvDatabaseURL},
		{"healthcheck", nil, ""},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s without %s", tt.command, tt.wantVar), func(t *testing.T) {
			c, err := Load(env(tt.env))
			if err != nil {
				t.Fatal(err)
			}
			err = c.Require(tt.command)
			switch {
			case tt.wantVar == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tt.wantVar != "" && (err == nil || !strings.Contains(err.Error(), tt.wantVar)):
				t.Errorf("error = %v, want it to name %s", err, tt.wantVar)
			}
		})
	}
	if err := (Config{}).Require("nonsense"); err == nil {
		t.Error("unknown command accepted")
	}
}

func TestRequireNamesOnlyFirstMissing(t *testing.T) {
	err := Config{}.Require("bootstrap")
	if err == nil || !strings.Contains(err.Error(), EnvSuperuserDatabaseURL) || strings.Contains(err.Error(), EnvAppPassword) {
		t.Errorf("error = %v", err)
	}
}

func TestSecretsAreRedacted(t *testing.T) {
	secrets := map[string]string{
		EnvDatabaseURL:           "SECRET-MARKER-db",
		EnvOwnerDatabaseURL:      "SECRET-MARKER-owner",
		EnvSuperuserDatabaseURL:  "SECRET-MARKER-super",
		EnvAppPassword:           "SECRET-MARKER-apppw",
		EnvOwnerPassword:         "SECRET-MARKER-ownerpw",
		EnvMonzoClientSecret:     "SECRET-MARKER-client",
		EnvTrueLayerClientID:     "client-id-test",
		EnvTrueLayerClientSecret: "SECRET-MARKER-truelayer",
		EnvOTLPToken:             "SECRET-MARKER-token",
		EnvAllowedEmails:         "SECRET-MARKER-email@example.com",
	}
	c, err := Load(env(secrets))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("loaded", "config", c)
	slog.New(slog.NewTextHandler(&buf, nil)).Info("loaded", "config", c)
	outputs := map[string]string{
		"%v": fmt.Sprintf("%v", c), "%+v": fmt.Sprintf("%+v", c), "%#v": fmt.Sprintf("%#v", c),
		"%s": fmt.Sprintf("%.500s", c), "ptr %+v": fmt.Sprintf("%+v", &c),
		"String": c.String(), "slog": buf.String(),
	}
	for name, out := range outputs {
		if strings.Contains(out, "SECRET-MARKER") {
			t.Errorf("%s output leaks a secret: %s", name, out)
		}
	}
	if !strings.Contains(c.String(), "truelayer_client_secret=[redacted]") {
		t.Errorf("String does not say the TrueLayer secret is set: %s", c.String())
	}
}

func TestTrueLayerDefaultsAndBothOrNeither(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.TrueLayerEnabled() {
		t.Error("TrueLayer enabled with neither setting")
	}
	if c.TrueLayerAuthURL != "https://auth.truelayer.com" || c.TrueLayerAPIURL != "https://api.truelayer.com" {
		t.Errorf("TrueLayer URL defaults wrong: %q %q", c.TrueLayerAuthURL, c.TrueLayerAPIURL)
	}
	if !strings.Contains(c.String(), "truelayer_client_secret=unset") {
		t.Errorf("String does not say the TrueLayer secret is unset: %s", c.String())
	}
	c, err = Load(env(map[string]string{
		EnvTrueLayerClientID: "client-id-test", EnvTrueLayerClientSecret: "secret-test",
		EnvTrueLayerAuthURL: "https://auth.truelayer.test", EnvTrueLayerAPIURL: "https://api.truelayer.test",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !c.TrueLayerEnabled() || c.TrueLayerClientID != "client-id-test" || c.TrueLayerClientSecret != "secret-test" ||
		c.TrueLayerAuthURL != "https://auth.truelayer.test" || c.TrueLayerAPIURL != "https://api.truelayer.test" {
		t.Errorf("TrueLayer settings not read: %v", c)
	}
	for name, only := range map[string]string{"id only": EnvTrueLayerClientID, "secret only": EnvTrueLayerClientSecret} {
		_, err := Load(env(map[string]string{only: "SECRET-MARKER-half"}))
		if err == nil {
			t.Errorf("%s: Load accepted half of the TrueLayer credentials", name)
			continue
		}
		if !strings.Contains(err.Error(), EnvTrueLayerClientID) || !strings.Contains(err.Error(), EnvTrueLayerClientSecret) {
			t.Errorf("%s: error does not name both settings: %v", name, err)
		}
		if strings.Contains(err.Error(), "SECRET-MARKER") {
			t.Errorf("%s: error carries the value: %v", name, err)
		}
	}
}
