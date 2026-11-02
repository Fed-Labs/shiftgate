package controlplane

import (
	"bytes"
	"strings"
	"testing"
)

// TestNormalizeEmail is the email canonicalization contract: case-folded and
// trimmed output, and a strict rejection of anything that is not a plausible
// address — this is the boundary that later feeds login lookups and SCIM
// matching.
func TestNormalizeEmail(t *testing.T) {
	valid := map[string]string{
		"User@Example.COM":               "user@example.com",
		"  padded@example.test  ":        "padded@example.test",
		"first.last+tag@sub.example.org": "first.last+tag@sub.example.org",
	}
	for input, want := range valid {
		got, err := normalizeEmail(input)
		if err != nil {
			t.Fatalf("normalizeEmail(%q) failed: %v", input, err)
		}
		if got != want {
			t.Fatalf("normalizeEmail(%q) = %q, want %q", input, got, want)
		}
	}
	invalid := []string{
		"",
		"ab",
		"no-at-sign.example",
		"@nouser.example",
		"user@",
		"user@@example.com",
		"user name@example.com",
		"user@nodot",
		"user@example.com\nadmin@example.com",
		strings.Repeat("a", 321) + "@example.com",
	}
	for _, input := range invalid {
		if got, err := normalizeEmail(input); err == nil {
			t.Fatalf("normalizeEmail(%q) = %q, want an error", input, got)
		}
	}
}

// TestTokenDigestIsKeyedAndDeterministic: digests are the stored half of
// every bearer token and API key, so they must be stable per (pepper, token),
// separated by either input, and compact enough to index.
func TestTokenDigestIsKeyedAndDeterministic(t *testing.T) {
	pepper := "pepper-one-at-least-thirty-two-bytes-long"
	digest := tokenDigest(pepper, "tok_abcdef")
	if len(digest) != 32 {
		t.Fatalf("digest length = %d, want 32 (HMAC-SHA256)", len(digest))
	}
	if !bytes.Equal(digest, tokenDigest(pepper, "tok_abcdef")) {
		t.Fatal("digest is not deterministic")
	}
	if bytes.Equal(digest, tokenDigest(pepper, "tok_abcdeg")) {
		t.Fatal("different tokens produced the same digest")
	}
	if bytes.Equal(digest, tokenDigest("pepper-two-at-least-thirty-two-bytes-long", "tok_abcdef")) {
		t.Fatal("different peppers produced the same digest")
	}
	if bytes.Equal(tokenDigest("", "tok"), tokenDigest("x", "tok")) {
		t.Fatal("empty pepper behaves like a real one")
	}
}

// TestSecureCompareString: constant-time comparison answers — equality only
// for exact matches, including the empty string, and never panics on length
// mismatches.
func TestSecureCompareString(t *testing.T) {
	if !secureCompareString("same", "same") {
		t.Fatal("equal strings compared unequal")
	}
	if !secureCompareString("", "") {
		t.Fatal("empty strings compared unequal")
	}
	if secureCompareString("same", "diff") {
		t.Fatal("different strings compared equal")
	}
	if secureCompareString("short", "shorter") {
		t.Fatal("different-length strings compared equal")
	}
	if secureCompareString("", "x") {
		t.Fatal("empty and non-empty compared equal")
	}
}
