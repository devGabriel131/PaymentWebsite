//go:build integration

package billing_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"paymentwebsite/internal/billing"
	"paymentwebsite/internal/testdb"
)

func signWebhookPayload(t *testing.T, payload []byte, secret string, timestamp time.Time) string {
	t.Helper()
	ts := timestamp.Unix()
	signedPayload := fmt.Sprintf("%d.%s", ts, string(payload))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signedPayload))
	sig := hex.EncodeToString(mac.Sum(nil))
	return fmt.Sprintf("t=%d,v1=%s", ts, sig)
}

func makeStripeEventJSON(id, eventType, resourceID string, livemode bool, apiVersion string) []byte {
	evt := map[string]any{
		"id":          id,
		"object":      "event",
		"type":        eventType,
		"livemode":    livemode,
		"api_version": apiVersion,
		"created":     time.Now().Unix(),
		"data": map[string]any{
			"object": map[string]any{
				"id":     resourceID,
				"object": "checkout.session",
			},
		},
	}
	b, _ := json.Marshal(evt)
	return b
}

func TestWebhookAuthenticityAndReplay(t *testing.T) {
	mock := newMockStripeServer()
	defer mock.Close()

	secret := "whsec_test_secret"
	cfg := billing.Config{
		BaseURL:                      "http://127.0.0.1:3000",
		WebhookSecret:                secret,
		PaymentMethodConfigurationID: "pmc_test",
		PortalConfigurationID:        "bpc_test",
		Live:                         false,
	}
	catalog := billing.Catalog{
		Full:          "pr_full",
		ThreeFirst:    "pr_three_1",
		ThreeLater:    "pr_three_2",
		SixFirst:      "pr_six_1",
		SixLater:      "pr_six_2",
		Reinstatement: "pr_reinstatement",
	}

	db := testdb.Open(t, billing.Models()...)
	svc, err := billing.New(db, mock.Client(), cfg, catalog, nil, nil)
	if err != nil {
		t.Fatalf("billing.New: %v", err)
	}

	const apiVersion = "2026-09-30.endive"

	// 1. Valid signed event inserts into billing_webhook_events and returns 200
	t.Run("valid_event_stored", func(t *testing.T) {
		payload := makeStripeEventJSON("evt_test_1", "checkout.session.completed", "cs_test_1", false, apiVersion)
		sig := signWebhookPayload(t, payload, secret, time.Now())

		req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", bytes.NewReader(payload))
		req.Header.Set("Stripe-Signature", sig)
		rec := httptest.NewRecorder()

		svc.Webhook(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
		}

		var event billing.WebhookEvent
		if err := db.Where("id = 'evt_test_1'").First(&event).Error; err != nil {
			t.Fatalf("expected event stored in DB: %v", err)
		}
		if event.EventType != "checkout.session.completed" || event.ResourceID != "cs_test_1" {
			t.Fatalf("unexpected stored event fields: %+v", event)
		}
	})

	// 2. Duplicate event delivery returns 200 without error
	t.Run("duplicate_event_returns_200", func(t *testing.T) {
		payload := makeStripeEventJSON("evt_test_1", "checkout.session.completed", "cs_test_1", false, apiVersion)
		sig := signWebhookPayload(t, payload, secret, time.Now())

		req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", bytes.NewReader(payload))
		req.Header.Set("Stripe-Signature", sig)
		rec := httptest.NewRecorder()

		svc.Webhook(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200 on duplicate, got %d", rec.Code)
		}
	})

	// 3. Bad signature rejected with 400
	t.Run("bad_signature_rejected", func(t *testing.T) {
		payload := makeStripeEventJSON("evt_test_bad_sig", "checkout.session.completed", "cs_test_bad", false, apiVersion)
		sig := signWebhookPayload(t, payload, "wrong_secret", time.Now())

		req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", bytes.NewReader(payload))
		req.Header.Set("Stripe-Signature", sig)
		rec := httptest.NewRecorder()

		svc.Webhook(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected status 400 for bad signature, got %d", rec.Code)
		}
	})

	// 4. Expired signature (> 5 minutes old) rejected with 400
	t.Run("expired_signature_rejected", func(t *testing.T) {
		payload := makeStripeEventJSON("evt_test_expired", "checkout.session.completed", "cs_test_exp", false, apiVersion)
		sig := signWebhookPayload(t, payload, secret, time.Now().Add(-10*time.Minute))

		req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", bytes.NewReader(payload))
		req.Header.Set("Stripe-Signature", sig)
		rec := httptest.NewRecorder()

		svc.Webhook(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected status 400 for expired signature, got %d", rec.Code)
		}
	})

	// 5. Wrong mode (livemode=true in sandbox) rejected with 400
	t.Run("wrong_mode_rejected", func(t *testing.T) {
		payload := makeStripeEventJSON("evt_test_livemode", "checkout.session.completed", "cs_test_live", true, apiVersion)
		sig := signWebhookPayload(t, payload, secret, time.Now())

		req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", bytes.NewReader(payload))
		req.Header.Set("Stripe-Signature", sig)
		rec := httptest.NewRecorder()

		svc.Webhook(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected status 400 for livemode mismatch, got %d", rec.Code)
		}
	})

	// 6. Oversized payload (> 1 MiB) rejected with 413
	t.Run("oversized_payload_rejected", func(t *testing.T) {
		hugePayload := make([]byte, 1024*1024+10)
		sig := signWebhookPayload(t, hugePayload, secret, time.Now())

		req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", bytes.NewReader(hugePayload))
		req.Header.Set("Stripe-Signature", sig)
		rec := httptest.NewRecorder()

		svc.Webhook(rec, req)

		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("expected status 413 for oversized body, got %d", rec.Code)
		}
	})

	// 7. Non-allowlisted event returns 200 without storing in DB
	t.Run("non_allowlisted_event_ignored", func(t *testing.T) {
		payload := makeStripeEventJSON("evt_unrelated_1", "payout.paid", "po_123", false, apiVersion)
		sig := signWebhookPayload(t, payload, secret, time.Now())

		req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", bytes.NewReader(payload))
		req.Header.Set("Stripe-Signature", sig)
		rec := httptest.NewRecorder()

		svc.Webhook(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", rec.Code)
		}

		var count int64
		db.Model(&billing.WebhookEvent{}).Where("id = 'evt_unrelated_1'").Count(&count)
		if count != 0 {
			t.Fatalf("expected unrelated event not to be stored, got count %d", count)
		}
	})
}
