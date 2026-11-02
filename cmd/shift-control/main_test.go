package main

import (
	"strings"
	"testing"

	"shift.dev/shift/internal/config"
)

func TestFlagOverridesBeatEnvironmentDefaults(t *testing.T) {
	t.Setenv("SHIFT_DATABASE_URL", "postgres://env.example:5432/shift")
	t.Setenv("SHIFT_CONTROL_LISTEN", "127.0.0.1:8090")
	configuration, err := config.LoadControlPlaneUnvalidated("")
	if err != nil {
		t.Fatal(err)
	}
	if configuration.DatabaseURL != "postgres://env.example:5432/shift" {
		t.Fatalf("environment database URL = %q", configuration.DatabaseURL)
	}
	(controlPlaneOverrides{
		databaseURL: "postgres://flag.example:5432/shift",
		listen:      "127.0.0.1:9090",
		logLevel:    "debug",
	}).apply(&configuration)
	if configuration.DatabaseURL != "postgres://flag.example:5432/shift" {
		t.Fatalf("-database-url did not override the environment default: %q", configuration.DatabaseURL)
	}
	if configuration.Listen != "127.0.0.1:9090" {
		t.Fatalf("-listen override lost: %q", configuration.Listen)
	}
	if configuration.LogLevel != "debug" {
		t.Fatalf("-log-level override lost: %q", configuration.LogLevel)
	}
}

// TestDatabaseURLFlagAloneSatisfiesStartup is the regression guard for the
// flag being unusable: the loader used to reject an empty URL before the
// -database-url override was applied, so CLI-wired databases never started.
func TestDatabaseURLFlagAloneSatisfiesStartup(t *testing.T) {
	t.Setenv("SHIFT_DATABASE_URL", "")
	configuration, err := config.LoadControlPlaneUnvalidated("")
	if err != nil {
		t.Fatalf("loader refused a configuration the -database-url flag is about to complete: %v", err)
	}
	(controlPlaneOverrides{
		databaseURL:    "postgres://flag.example:5432/shift",
		tokenPepper:    strings.Repeat("token", 12),
		passwordPepper: strings.Repeat("password", 8),
	}).apply(&configuration)
	if err := configuration.Validate(); err != nil {
		t.Fatalf("flag-supplied database URL rejected: %v", err)
	}
}

func TestEmptyDatabaseURLRejectedAtValidation(t *testing.T) {
	t.Setenv("SHIFT_DATABASE_URL", "")
	configuration, err := config.LoadControlPlaneUnvalidated("")
	if err != nil {
		t.Fatal(err)
	}
	(controlPlaneOverrides{
		tokenPepper:    strings.Repeat("t", 32),
		passwordPepper: strings.Repeat("p", 32),
	}).apply(&configuration)
	err = configuration.Validate()
	if err == nil || !strings.Contains(err.Error(), "database URL is required") {
		t.Fatalf("empty database URL not rejected by Validate: %v", err)
	}
}

func TestStripeSecretsComeFromEnvironmentNotJustFlags(t *testing.T) {
	t.Setenv("SHIFT_STRIPE_SECRET_KEY", "sk_live_from_env")
	t.Setenv("SHIFT_STRIPE_WEBHOOK_KEY", "whsec_from_env")
	configuration, err := config.LoadControlPlaneUnvalidated("")
	if err != nil {
		t.Fatal(err)
	}
	if configuration.StripeSecretKey != "sk_live_from_env" || configuration.StripeWebhookKey != "whsec_from_env" {
		t.Fatalf("SHIFT_STRIPE_SECRET_KEY / SHIFT_STRIPE_WEBHOOK_KEY not mapped: %q / %q",
			configuration.StripeSecretKey, configuration.StripeWebhookKey)
	}
	(controlPlaneOverrides{
		stripeSecretKey:  "sk_live_from_flag",
		stripeWebhookKey: "whsec_from_flag",
	}).apply(&configuration)
	if configuration.StripeSecretKey != "sk_live_from_flag" || configuration.StripeWebhookKey != "whsec_from_flag" {
		t.Fatal("flags must still override the environment (development use)")
	}
}

func TestLogLevelValidationRejectsTypos(t *testing.T) {
	base := config.DefaultControlPlane()
	base.DatabaseURL = "postgres://localhost:5432/shift"
	base.TokenPepper = strings.Repeat("token", 12)
	base.PasswordPepper = strings.Repeat("password", 8)
	for _, accepted := range []string{"debug", "info", "warn", "warning", "error", "Warn", "INFO", ""} {
		configuration := base
		configuration.LogLevel = accepted
		if err := configuration.Validate(); err != nil {
			t.Fatalf("log_level %q rejected: %v", accepted, err)
		}
	}
	for _, rejected := range []string{"trace", "warn ", "dbg", "debug2"} {
		configuration := base
		configuration.LogLevel = rejected
		if err := configuration.Validate(); err == nil {
			t.Fatalf("log_level %q silently degraded to info instead of failing", rejected)
		}
	}
}

// TestRemoteAgentDispatchDefaultsClosed guards the documented same-host
// dispatch model: managed migrations must not silently fall back to a
// machine's remote agent URL unless an operator opts in.
func TestRemoteAgentDispatchDefaultsClosed(t *testing.T) {
	if config.DefaultControlPlane().AllowRemoteAgentDispatch {
		t.Fatal("AllowRemoteAgentDispatch defaults to true; the documented dispatch model is the same-host socket only")
	}
}
