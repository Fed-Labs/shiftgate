package oidc

import (
	"context"
	"testing"
)

// TestValidateIDTokenDemandsANonce proves the validator never treats a missing
// expected nonce as "skip the check": tokens validate only when they carry the
// nonce of the authorization request that started the flow.
func TestValidateIDTokenDemandsANonce(t *testing.T) {
	idp := newFakeIdP(t)
	provider := newTestProvider(t, idp)

	idp.nonce = "nonce-abc"
	if _, err := provider.ValidateIDToken(context.Background(), idp.mintToken(t), ""); err == nil {
		t.Fatal("an ID token validated with no expected nonce")
	}

	idp.nonce = ""
	if _, err := provider.ValidateIDToken(context.Background(), idp.mintToken(t), "nonce-abc"); err == nil {
		t.Fatal("an ID token without a nonce claim verified")
	}

	idp.nonce = "nonce-abc"
	if _, err := provider.ValidateIDToken(context.Background(), idp.mintToken(t), "nonce-abc"); err != nil {
		t.Fatalf("a token carrying the expected nonce did not verify: %v", err)
	}
}
