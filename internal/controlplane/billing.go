package controlplane

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type StripeEvent struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Created int64           `json:"created"`
	Data    json.RawMessage `json:"data"`
}

type stripeData struct {
	Object json.RawMessage `json:"object"`
}

type stripeSubscription struct {
	ID               string            `json:"id"`
	Customer         string            `json:"customer"`
	Status           string            `json:"status"`
	CurrentPeriodEnd int64             `json:"current_period_end"`
	Metadata         map[string]string `json:"metadata"`
}

func VerifyStripeSignature(payload []byte, header, secret string, now time.Time, tolerance time.Duration) error {
	if len(payload) == 0 || strings.TrimSpace(secret) == "" {
		return errors.New("stripe webhook secret and payload are required")
	}
	if tolerance <= 0 {
		tolerance = 5 * time.Minute
	}
	var timestamp int64
	var signatures []string
	for _, part := range strings.Split(header, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch key {
		case "t":
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return errors.New("stripe signature timestamp is invalid")
			}
			timestamp = parsed
		case "v1":
			if len(value) == 64 {
				signatures = append(signatures, value)
			}
		}
	}
	if timestamp == 0 || len(signatures) == 0 {
		return errors.New("stripe signature is incomplete")
	}
	stampTime := time.Unix(timestamp, 0)
	if delta := now.Sub(stampTime); delta > tolerance || delta < -tolerance {
		return errors.New("stripe signature timestamp is outside the allowed tolerance")
	}
	signed := []byte(strconv.FormatInt(timestamp, 10) + "." + string(payload))
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(signed)
	expected := mac.Sum(nil)
	for _, value := range signatures {
		provided, err := hex.DecodeString(value)
		if err == nil && hmac.Equal(expected, provided) {
			return nil
		}
	}
	return errors.New("stripe signature verification failed")
}

func parseStripeSubscription(event StripeEvent) (stripeSubscription, error) {
	var data stripeData
	if err := json.Unmarshal(event.Data, &data); err != nil {
		return stripeSubscription{}, fmt.Errorf("decode stripe event data: %w", err)
	}
	var subscription stripeSubscription
	if err := json.Unmarshal(data.Object, &subscription); err != nil {
		return stripeSubscription{}, fmt.Errorf("decode stripe subscription: %w", err)
	}
	if subscription.ID == "" {
		return stripeSubscription{}, errors.New("stripe subscription id is missing")
	}
	return subscription, nil
}
