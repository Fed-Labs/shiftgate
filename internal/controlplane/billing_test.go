package controlplane

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"testing"
	"time"
)

func TestVerifyStripeSignature(t *testing.T) {
	secret := "whsec_test_secret"
	payload := []byte(`{"id":"evt_123","type":"customer.subscription.updated"}`)
	timestamp := time.Now().Unix()
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(strconv.FormatInt(timestamp, 10) + "." + string(payload)))
	header := "t=" + strconv.FormatInt(timestamp, 10) + ",v1=" + hex.EncodeToString(mac.Sum(nil))
	if err := VerifyStripeSignature(payload, header, secret, time.Unix(timestamp, 0), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := VerifyStripeSignature(payload, header, "wrong", time.Unix(timestamp, 0), time.Minute); err == nil {
		t.Fatal("invalid Stripe secret was accepted")
	}
	oldHeader := "t=" + strconv.FormatInt(timestamp-3600, 10) + ",v1=" + hex.EncodeToString(mac.Sum(nil))
	if err := VerifyStripeSignature(payload, oldHeader, secret, time.Unix(timestamp, 0), time.Minute); err == nil {
		t.Fatal("stale Stripe signature was accepted")
	}
}
