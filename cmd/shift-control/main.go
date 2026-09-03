package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/controlplane"
	"shift.dev/shift/internal/observability"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "shift-control:", err)
		os.Exit(1)
	}
}

func run() error {
	var configPath string
	var databaseURL string
	var listen string
	var tokenPepper string
	var passwordPepper string
	var stripeWebhookKey string
	var stripeSecretKey string
	var stripePriceFlags stripePriceValues
	var logLevel string
	var showVersion bool
	var printDefault bool
	flag.StringVar(&configPath, "config", "", "path to control-plane JSON configuration")
	flag.StringVar(&databaseURL, "database-url", "", "PostgreSQL connection URL")
	flag.StringVar(&listen, "listen", "", "HTTP listen address")
	flag.StringVar(&tokenPepper, "token-pepper", "", "token hashing pepper")
	flag.StringVar(&passwordPepper, "password-pepper", "", "password hashing pepper")
	flag.StringVar(&stripeWebhookKey, "stripe-webhook-key", "", "Stripe webhook signing secret")
	flag.StringVar(&stripeSecretKey, "stripe-secret-key", "", "Stripe secret API key for outbound checkout and portal sessions")
	flag.Var(&stripePriceFlags, "stripe-price", "Stripe price id for a plan, as plan=price_id (repeatable)")
	flag.StringVar(&logLevel, "log-level", "", "debug, info, warn, or error")
	flag.BoolVar(&showVersion, "version", false, "print version")
	flag.BoolVar(&printDefault, "print-default-config", false, "print default JSON configuration")
	flag.Parse()
	if showVersion {
		fmt.Println("shift-control", config.Version)
		return nil
	}
	if printDefault {
		return json.NewEncoder(os.Stdout).Encode(config.DefaultControlPlane())
	}
	configuration, err := config.LoadControlPlaneUnvalidated(configPath)
	if err != nil {
		return err
	}
	if databaseURL != "" {
		configuration.DatabaseURL = databaseURL
	}
	if listen != "" {
		configuration.Listen = listen
	}
	if tokenPepper != "" {
		configuration.TokenPepper = tokenPepper
	}
	if passwordPepper != "" {
		configuration.PasswordPepper = passwordPepper
	}
	if stripeWebhookKey != "" {
		configuration.StripeWebhookKey = stripeWebhookKey
	}
	if stripeSecretKey != "" {
		configuration.StripeSecretKey = stripeSecretKey
	}
	if len(stripePriceFlags.values) > 0 {
		if configuration.StripePrices == nil {
			configuration.StripePrices = map[string]string{}
		}
		for plan, priceID := range stripePriceFlags.values {
			configuration.StripePrices[plan] = priceID
		}
	}
	if logLevel != "" {
		configuration.LogLevel = logLevel
	}
	if err := configuration.Validate(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server, err := controlplane.Open(ctx, configuration, observability.NewLogger(configuration.LogLevel))
	if err != nil {
		return err
	}
	defer server.Close()
	return server.Run(ctx)
}

// stripePriceValues collects repeated -stripe-price plan=price_id flags.
type stripePriceValues struct {
	values map[string]string
}

func (v *stripePriceValues) String() string {
	parts := make([]string, 0, len(v.values))
	for plan, priceID := range v.values {
		parts = append(parts, plan+"="+priceID)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func (v *stripePriceValues) Set(value string) error {
	plan, priceID, ok := strings.Cut(value, "=")
	if !ok || plan == "" || priceID == "" {
		return fmt.Errorf("stripe price must be plan=price_id, got %q", value)
	}
	if v.values == nil {
		v.values = map[string]string{}
	}
	v.values[plan] = priceID
	return nil
}
