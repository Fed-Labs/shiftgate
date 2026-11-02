package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/relay"
)

// shift-relay enforces its authorization surface — a durable leg store
// (ticket claim + one-use consume), a client-CA binding the outer identity to
// machine certificates, and a ticket audience — but every input has a
// sensible default, so the documented startup and the systemd unit both come
// up without a long flag list: the database URL and audience fall back to
// their environment variables (and the audience defaults to
// SHIFT_RELAY_ENDPOINT, the exact value the control plane stamps into
// tickets), the client CA is looked for in the fleet's conventional CA
// locations, and when no PostgreSQL URL is provisioned a durable file leg
// store keeps the one-use ticket promise across restarts. Anything genuinely
// missing is reported once, in full, naming the flag and its environment
// variable — an operator never needs the docs in hand to learn what is absent.
// The HMAC ticket alone is capturable; the store is what makes a captured
// ticket one-use, the client CA is what binds a leg to the machine the ticket
// names, and the TTL check plus per-leg claims close the replay window.
func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Println("shift-relay", config.Version)
		return
	}
	flags := flag.NewFlagSet("shift-relay", flag.ExitOnError)
	listen := flags.String("listen", defaultListen(), "TLS relay listen address (SHIFT_RELAY_LISTEN; defaults to :$PORT when the platform supplies PORT)")
	cert := flags.String("cert", os.Getenv("SHIFT_RELAY_CERT"), "TLS certificate PEM or file path (SHIFT_RELAY_CERT)")
	key := flags.String("key", os.Getenv("SHIFT_RELAY_KEY"), "TLS private key PEM or file path (SHIFT_RELAY_KEY)")
	secret := flags.String("signing-key", os.Getenv("SHIFT_RELAY_SIGNING_KEY"), "relay ticket signing secret (SHIFT_RELAY_SIGNING_KEY)")
	databaseURL := flags.String("database-url", firstNonEmpty(os.Getenv("SHIFT_DATABASE_URL"), os.Getenv("SHIFT_RELAY_DATABASE_URL")), "PostgreSQL URL for durable ticket claim/consume (SHIFT_DATABASE_URL); empty uses the built-in durable file leg store")
	legStorePath := flags.String("leg-store", firstNonEmpty(os.Getenv("SHIFT_RELAY_LEG_STORE"), "/var/lib/shift-relay/legstore.json"), "leg-store file used when no database URL is provisioned (SHIFT_RELAY_LEG_STORE)")
	clientCA := flags.String("client-ca", os.Getenv("SHIFT_RELAY_CLIENT_CA"), "machine client CA PEM or file path (SHIFT_RELAY_CLIENT_CA)")
	audience := flags.String("audience", firstNonEmpty(os.Getenv("SHIFT_RELAY_AUDIENCE"), os.Getenv("SHIFT_RELAY_ENDPOINT")), "ticket audience this relay accepts (SHIFT_RELAY_AUDIENCE, default SHIFT_RELAY_ENDPOINT — the value the control plane stamps into tickets)")
	flags.Parse(os.Args[1:])

	var missing []string
	if *cert == "" || *key == "" || len(*secret) < 32 {
		missing = append(missing, "--cert and --key (TLS certificate and key PEM) plus a 32+ byte signing key (--signing-key or SHIFT_RELAY_SIGNING_KEY)")
	}
	if strings.TrimSpace(*audience) == "" {
		missing = append(missing, "--audience (or SHIFT_RELAY_AUDIENCE): tickets must be bound to the relay that will honor them; use the same value the control plane has in SHIFT_RELAY_ENDPOINT")
	}
	caValue := strings.TrimSpace(*clientCA)
	if caValue == "" {
		caValue = firstExistingFile("/etc/shift/tls/ca.pem", "/etc/shift/tls/machine-ca.pem", "/etc/shift-relay/ca.pem")
	}
	if caValue == "" {
		missing = append(missing, "--client-ca (or SHIFT_RELAY_CLIENT_CA): without a machine client CA the relay cannot bind legs to certified machine identities")
	}
	if len(missing) > 0 {
		slog.Error("shift-relay cannot start; missing required inputs:\n  " + strings.Join(missing, "\n  "))
		os.Exit(2)
	}

	caPEM, err := readPEMOrFile(caValue)
	if err != nil {
		slog.Error("read client CA", "source", sourceLabel(caValue), "error", err)
		os.Exit(1)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caPEM) {
		slog.Error("client CA file has no certificates", "source", sourceLabel(caValue))
		os.Exit(2)
	}

	var store relay.LegStore
	var meter relay.TransferMeter
	if strings.TrimSpace(*databaseURL) != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		databaseStore, openErr := database.Open(ctx, *databaseURL)
		if openErr != nil {
			cancel()
			slog.Error("open ticket store", "error", openErr)
			os.Exit(1)
		}
		if pingErr := databaseStore.Ping(ctx); pingErr != nil {
			cancel()
			slog.Error("ticket store unreachable", "error", pingErr)
			os.Exit(1)
		}
		cancel()
		defer databaseStore.Close()
		store = databaseStore
		meter = databaseStore
	} else {
		// No PostgreSQL provisioned: the built-in file leg store keeps the
		// one-use claim/consume state durably on this node. Bandwidth metering
		// (which needs the shared database) is the one thing it cannot do.
		fileStore, openErr := relay.OpenFileLegStore(*legStorePath)
		if openErr != nil {
			slog.Error("open file leg store", "path", *legStorePath, "error", openErr)
			os.Exit(1)
		}
		store = fileStore
		slog.Info("no database URL provisioned; using durable file leg store (bandwidth metering disabled)", "path", *legStorePath)
	}

	server := relay.NewServer(relay.Signer{Key: []byte(*secret)}, 256)
	server.Store = store
	server.Meter = meter
	server.Audience = strings.TrimSpace(*audience)
	// Inner agent mTLS remains the machine identity boundary; the relay ticket
	// authorizes a short-lived leg and is never a substitute for peer TLS.
	tlsServer := &http.Server{Addr: *listen, Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	certificate, err := loadKeyPair(*cert, *key)
	if err != nil {
		slog.Error("load certificate", "error", err)
		os.Exit(1)
	}
	tlsServer.TLSConfig = &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		// Every leg must present a machine certificate the fleet CA signed;
		// the header identity is only accepted when it matches the certified
		// key (see relay.CertifiedMachine).
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  clientCAs,
	}
	slog.Info("shift relay listening", "address", *listen, "audience", *audience)
	if err := tlsServer.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("relay stopped", "error", err)
		os.Exit(1)
	}
}

// firstNonEmpty returns the first non-blank argument.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// firstExistingFile returns the first path that exists, for the conventional
// locations a fleet's machine client CA is installed in.
func firstExistingFile(paths ...string) string {
	for _, path := range paths {
		if information, err := os.Stat(path); err == nil && information.Mode().IsRegular() {
			return path
		}
	}
	return ""
}

// defaultListen resolves the relay's listen address: the explicit environment
// first, then the platform-as-a-service PORT convention (Heroku-style —
// the platform's edge proxy is the TLS terminator), then the built-in
// all-interfaces default.
func defaultListen() string {
	if value := os.Getenv("SHIFT_RELAY_LISTEN"); value != "" {
		return value
	}
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return ":9443"
}

// readPEMOrFile returns the material value names: inline PEM (any value with a
// PEM BEGIN line — deployment environments that carry TLS material as
// variables rather than files) or the contents of the file the value names.
func readPEMOrFile(value string) ([]byte, error) {
	if strings.Contains(value, "-----BEGIN") {
		return []byte(value), nil
	}
	return os.ReadFile(value)
}

// sourceLabel names where a PEM-or-file value resolved from, for error
// messages that must not echo certificate or key material.
func sourceLabel(value string) string {
	if strings.Contains(value, "-----BEGIN") {
		return "inline PEM"
	}
	return value
}

// loadKeyPair loads a TLS key pair whose halves are each PEM-or-file, the same
// contract as the machine client CA above.
func loadKeyPair(certValue, keyValue string) (tls.Certificate, error) {
	certPEM, err := readPEMOrFile(strings.TrimSpace(certValue))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("read certificate (%s): %w", sourceLabel(strings.TrimSpace(certValue)), err)
	}
	keyPEM, err := readPEMOrFile(strings.TrimSpace(keyValue))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("read private key (%s): %w", sourceLabel(strings.TrimSpace(keyValue)), err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parse key pair: %w", err)
	}
	return pair, nil
}
