package billing_test

import (
	"log/slog"
	"testing"

	"github.com/stripe/stripe-go/v87"
	"gorm.io/gorm"
	"paymentwebsite/internal/billing"
)

func TestBillingNewValidation(t *testing.T) {
	validCfg := billing.Config{
		BaseURL:                      "http://127.0.0.1:3000",
		WebhookSecret:                "whsec_test",
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
	dummyClient := stripe.NewClient("sk_test_dummy")
	dummyDB := &gorm.DB{}

	t.Run("nil_db", func(t *testing.T) {
		_, err := billing.New(nil, dummyClient, validCfg, catalog, nil, slog.Default())
		if err == nil {
			t.Fatal("expected error for nil db")
		}
	})

	t.Run("nil_client", func(t *testing.T) {
		_, err := billing.New(dummyDB, nil, validCfg, catalog, nil, slog.Default())
		if err == nil {
			t.Fatal("expected error for nil stripe client")
		}
	})

	t.Run("invalid_config", func(t *testing.T) {
		invalidCfg := validCfg
		invalidCfg.BaseURL = ""
		_, err := billing.New(dummyDB, dummyClient, invalidCfg, catalog, nil, slog.Default())
		if err == nil {
			t.Fatal("expected error for invalid config")
		}
	})

	t.Run("valid_construction", func(t *testing.T) {
		svc, err := billing.New(dummyDB, dummyClient, validCfg, catalog, nil, slog.Default())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if svc == nil {
			t.Fatal("expected non-nil service")
		}
	})
}
