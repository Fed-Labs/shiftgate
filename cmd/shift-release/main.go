// Command shift-release publishes SHIFT releases: it generates release-signing
// keys, signs release documents, assembles feeds, and verifies a finished feed
// the same way an installer will. It exists so that no release ever reaches a
// machine unsigned and no signing key ever leaves this process.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"shift.dev/shift/internal/persistence"
	"shift.dev/shift/internal/update"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "shift-release:", err)
		os.Exit(1)
	}
}

func run(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return usage()
	}
	subcommand, arguments := arguments[0], arguments[1:]
	switch subcommand {
	case "keygen":
		return keygen(arguments, output)
	case "trusted-key":
		return trustedKey(arguments, output)
	case "sign":
		return sign(arguments, output)
	case "feed":
		return feed(arguments, output)
	case "verify":
		return verify(arguments, output)
	default:
		return usage()
	}
}

func usage() error {
	return errors.New(`usage: shift-release COMMAND

commands:
  keygen     Generate a release-signing key pair
  trusted-key Print the trusted-key entry for a public key
  sign       Sign one release document
  feed       Assemble signed releases into a feed
  verify     Verify a feed against trusted keys`)
}

// keygen generates an Ed25519 release-signing key pair. The private key is
// written with owner-only permissions because it can ship a release to every
// machine that trusts it; the public key is written for distribution.
func keygen(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("keygen", flag.ContinueOnError)
	privatePath := flags.String("private-key", "", "where to write the PKCS#8 private key")
	publicPath := flags.String("public-key", "", "where to write the public key")
	comment := flags.String("comment", "", "note recorded with the trusted-key entry")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *privatePath == "" || *publicPath == "" {
		return errors.New("keygen requires --private-key and --public-key")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return err
	}
	publicDER, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		return err
	}
	if err := persistence.WriteFile(*privatePath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0o600); err != nil {
		return err
	}
	if err := persistence.WriteFile(*publicPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}), 0o644); err != nil {
		return err
	}
	entry := update.TrustedKey{Comment: *comment}
	if entry.PublicKeyPEM, err = publicPEM(*publicPath); err != nil {
		return err
	}
	if entry.ID, err = update.TrustedKeyID(entry.PublicKeyPEM); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(output, "Private key written to %s\nPublic key written to %s\nTrusted-key entry:\n%s\n", *privatePath, *publicPath, encoded)
	return nil
}

// trustedKey prints the trusted-key entry for one public key, so an operator can
// add a signer to a fleet configuration without hand-writing JSON.
func trustedKey(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("trusted-key", flag.ContinueOnError)
	publicPath := flags.String("public-key", "", "path to the public key PEM")
	comment := flags.String("comment", "", "note recorded with the entry")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *publicPath == "" {
		return errors.New("trusted-key requires --public-key")
	}
	entry := update.TrustedKey{Comment: *comment}
	var err error
	if entry.PublicKeyPEM, err = publicPEM(*publicPath); err != nil {
		return err
	}
	if entry.ID, err = update.TrustedKeyID(entry.PublicKeyPEM); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(output, string(encoded))
	return nil
}

// sign signs one release document with one key. Signing the same release with
// several keys (by running sign once per key and merging) is how a feed reaches
// a higher signature threshold.
func sign(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("sign", flag.ContinueOnError)
	keyPath := flags.String("key", "", "path to the private key PEM")
	releasePath := flags.String("release", "", "path to the release document to sign")
	outputPath := flags.String("out", "", "where to write the signed release")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *keyPath == "" || *releasePath == "" || *outputPath == "" {
		return errors.New("sign requires --key, --release, and --out")
	}
	privatePEM, err := os.ReadFile(*keyPath)
	if err != nil {
		return err
	}
	signer, err := update.NewSigner(string(privatePEM))
	if err != nil {
		return err
	}
	var release update.Release
	if err := persistence.ReadJSON(*releasePath, &release); err != nil {
		return fmt.Errorf("read release: %w", err)
	}
	signed, err := signer.SignRelease(release)
	if err != nil {
		return err
	}
	if err := persistence.WriteJSON(*outputPath, signed, 0o644); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(output, "Release %s (%s) signed by key %s and written to %s.\n", signed.Release.Version, signed.Release.Channel, signer.KeyID(), *outputPath)
	return nil
}

// feed assembles signed releases into a feed document. Every release must be on
// the feed's channel, and the finished feed is validated with the same code an
// installer will run, so a feed that is wrong never leaves the publishing host.
func feed(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("feed", flag.ContinueOnError)
	channel := flags.String("channel", string(update.ChannelStable), "the feed's release channel")
	outputPath := flags.String("out", "", "where to write the feed")
	revoke := multiFlag{}
	flags.Var(&revoke, "revoke", "version to withdraw; repeatable")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	signedPaths := flags.Args()
	if *outputPath == "" || len(signedPaths) == 0 {
		return errors.New("feed requires --out and at least one signed release path")
	}
	for _, signedPath := range signedPaths {
		if strings.HasPrefix(signedPath, "-") {
			return fmt.Errorf("%q looks like a flag, not a release path; flags must come before the release paths", signedPath)
		}
	}
	document := update.Feed{
		Version:     feedSchemaVersion,
		Channel:     update.Channel(*channel),
		GeneratedAt: time.Now().UTC(),
		Revoked:     revoke,
	}
	for _, signedPath := range signedPaths {
		var signed update.SignedRelease
		if err := persistence.ReadJSON(signedPath, &signed); err != nil {
			return fmt.Errorf("read signed release %s: %w", signedPath, err)
		}
		document.Releases = append(document.Releases, signed)
	}
	if err := document.Validate(); err != nil {
		return fmt.Errorf("the assembled feed is not publishable: %w", err)
	}
	if err := persistence.WriteJSON(*outputPath, document, 0o644); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(output, "Feed for channel %s with %d release(s) written to %s.\n", document.Channel, len(document.Releases), *outputPath)
	return nil
}

// verify checks a finished feed against a trusted-key file, exactly as an
// installer's key ring will, and prints which key signed each release. It is the
// last step before publication and the first step when debugging a machine that
// refuses a feed.
func verify(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	feedPath := flags.String("feed", "", "path to the feed document")
	keysPath := flags.String("keys", "", "path to the trusted-key JSON array")
	threshold := flags.Int("threshold", 1, "how many distinct trusted signatures each release needs")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *feedPath == "" || *keysPath == "" {
		return errors.New("verify requires --feed and --keys")
	}
	var document update.Feed
	if err := persistence.ReadJSON(*feedPath, &document); err != nil {
		return fmt.Errorf("read feed: %w", err)
	}
	var keys []update.TrustedKey
	if err := persistence.ReadJSON(*keysPath, &keys); err != nil {
		return fmt.Errorf("read trusted keys: %w", err)
	}
	ring, err := update.NewKeyRing(keys, *threshold, time.Now().UTC())
	if err != nil {
		return err
	}
	if err := document.Validate(); err != nil {
		return err
	}
	failed := false
	for _, signed := range document.Releases {
		signedBy, err := ring.Verify(signed)
		if err != nil {
			failed = true
			_, _ = fmt.Fprintf(output, "UNTRUSTED %s: %v\n", signed.Release.Version, err)
			continue
		}
		_, _ = fmt.Fprintf(output, "trusted   %s signed by %s\n", signed.Release.Version, strings.Join(signedBy, ", "))
	}
	if failed {
		return errors.New("one or more releases did not meet the signature threshold")
	}
	_, _ = fmt.Fprintf(output, "All %d release(s) meet a threshold of %d.\n", len(document.Releases), *threshold)
	return nil
}

// feedSchemaVersion mirrors the feed layout the update package reads; it is
// restated here so the tool refuses to build a feed of a layout it cannot
// itself verify.
const feedSchemaVersion = 1

// publicPEM reads a public key and checks it really is one.
func publicPEM(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(content)
	if block == nil || block.Type != "PUBLIC KEY" {
		return "", fmt.Errorf("%s is not a public key PEM", path)
	}
	return string(content), nil
}

// multiFlag collects a repeatable string flag, which is how --revoke accepts
// several withdrawn versions.
type multiFlag []string

func (values *multiFlag) String() string { return strings.Join(*values, ",") }

func (values *multiFlag) Set(value string) error {
	*values = append(*values, value)
	return nil
}
