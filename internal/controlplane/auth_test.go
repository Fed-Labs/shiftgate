package controlplane

import (
	"strings"
	"testing"
	"time"
)

func TestPasswordHashAndVerify(t *testing.T) {
	pepper := strings.Repeat("p", 48)
	hash, err := hashPassword("correct horse battery staple", pepper)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("unexpected password hash format %q", hash)
	}
	if !verifyPassword("correct horse battery staple", hash, pepper) {
		t.Fatal("password did not verify")
	}
	if verifyPassword("wrong password", hash, pepper) {
		t.Fatal("wrong password verified")
	}
	if verifyPassword("correct horse battery staple", hash, strings.Repeat("q", 48)) {
		t.Fatal("wrong pepper verified")
	}
}

func TestTokenParsingAndExpiry(t *testing.T) {
	token, _, err := newToken(accessTokenPrefix)
	if err != nil {
		t.Fatal(err)
	}
	parsed, ok := parseBearer("Bearer " + token)
	if !ok || parsed != token {
		t.Fatalf("bearer token was not parsed: %q %v", parsed, ok)
	}
	if _, ok := parseBearer("Basic " + token); ok {
		t.Fatal("basic authorization was accepted")
	}
	now := time.Now()
	if !validTokenExpiry(now, now.Add(10*time.Second)) || validTokenExpiry(now, now.Add(time.Second)) {
		t.Fatal("token expiry policy is incorrect")
	}
}

func TestAPIKeyScopesAreResourceAware(t *testing.T) {
	if !scopeAllows([]string{"machines"}, RoleViewer, "machines") || !scopeAllows([]string{"machines"}, RoleOperator, "machines") {
		t.Fatal("machines scope did not grant machine access")
	}
	if scopeAllows([]string{"machines"}, RoleViewer, "workloads") || scopeAllows([]string{"machines"}, RoleOperator, "migrations") {
		t.Fatal("resource-specific scope escaped its resource")
	}
	if !scopeAllows([]string{"read"}, RoleViewer, "billing") || scopeAllows([]string{"read"}, RoleOperator, "machines") {
		t.Fatal("read scope role boundary is incorrect")
	}
	if !scopeAllows([]string{"operate"}, RoleOperator, "checkpoints") || scopeAllows([]string{"operate"}, RoleAdmin, "administration") {
		t.Fatal("operate scope role boundary is incorrect")
	}
	if !scopeAllows([]string{"admin"}, RoleAdmin, "administration") {
		t.Fatal("admin scope did not grant administrative access")
	}
}
