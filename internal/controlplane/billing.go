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

	"shift.dev/shift/internal/billing"
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
	Items            stripeItems       `json:"items"`
}

// stripeItems carries the subscription's billed line items: the base plan
// price, any add-on lines, and — for flat-with-included plans — the dedicated
// extra-seat line (quantity = extras only) the seat sync updates.
type stripeItems struct {
	Data []stripeSubscriptionItem `json:"data"`
}

type stripeSubscriptionItem struct {
	ID       string                  `json:"id"`
	Quantity int                     `json:"quantity"`
	Price    stripeSubscriptionPrice `json:"price"`
}

type stripeSubscriptionPrice struct {
	ID         string `json:"id"`
	UnitAmount int64  `json:"unit_amount"`
}

// SeatPurchase returns the purchased seat ceiling and the subscription item
// the seat-quantity sync updates. The ceiling of record is the checkout
// session's seats metadata (included + extra seats); it never comes from a
// line item's quantity, which collapses the moment quantities are synced.
// The sync target is the dedicated extra-seat line — never the base plan
// price, whose price Stripe multiplies — except on legacy per-seat plans that
// bill seats on the base line. knownPriceIDs lists the configured base and
// add-on price ids so the generated extra-seat price is recognizable.
func (s stripeSubscription) SeatPurchase(plan billing.Plan, knownPriceIDs map[string]bool) (itemID string, seats int) {
	seats = int(parseMetadataInt(s.Metadata["seats"], 0))
	if seats > 0 {
		return s.seatItemID(plan, knownPriceIDs), seats
	}
	// No seats metadata (subscriptions predating it): only a per-seat
	// subscription — or a negotiated tier outside the catalog — carries a
	// meaningful base-line quantity. Writing the base-line quantity of a
	// flat-with-included subscription (always 1) would collapse its purchased
	// ceiling to the included seats, so the stored ceiling is left untouched.
	if plan.PerSeat || plan.Key == "" {
		if len(s.Items.Data) > 0 {
			return s.Items.Data[0].ID, s.Items.Data[0].Quantity
		}
	}
	return "", 0
}

// seatItemID names the line item the seat sync must update: the last line
// matching the plan's per-seat overage amount (add-ons may share the unit
// price and precede it), else the first line whose price is not a configured
// base or add-on price. The base plan line is never returned for
// flat-with-included plans — pushing a seat quantity onto it multiplies the
// base fee — and an empty result makes the sync a no-op instead.
func (s stripeSubscription) seatItemID(plan billing.Plan, knownPriceIDs map[string]bool) string {
	if plan.PerSeat {
		if len(s.Items.Data) > 0 {
			return s.Items.Data[0].ID
		}
		return ""
	}
	seatUnit := billing.SeatOverageUnitAmount(plan, s.Metadata["interval"])
	extrasItem, unknownItem := "", ""
	for _, item := range s.Items.Data {
		if knownPriceIDs[item.Price.ID] {
			continue
		}
		if unknownItem == "" {
			unknownItem = item.ID
		}
		if seatUnit > 0 && item.Price.UnitAmount == seatUnit {
			extrasItem = item.ID
		}
	}
	if extrasItem != "" {
		return extrasItem
	}
	return unknownItem
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
