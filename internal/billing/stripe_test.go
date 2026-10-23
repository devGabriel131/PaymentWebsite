package billing_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v87"
	"paymentwebsite/internal/billing"
)

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     billing.Config
		wantErr bool
	}{
		{
			name: "valid_live",
			cfg: billing.Config{
				BaseURL:                      "https://payments.example.com",
				WebhookSecret:                "whsec_test123",
				PaymentMethodConfigurationID: "pmc_123",
				PortalConfigurationID:        "bpc_123",
				Live:                         true,
			},
			wantErr: false,
		},
		{
			name: "valid_sandbox_localhost",
			cfg: billing.Config{
				BaseURL:                      "http://127.0.0.1:3000",
				WebhookSecret:                "whsec_test123",
				PaymentMethodConfigurationID: "pmc_123",
				PortalConfigurationID:        "bpc_123",
				Live:                         false,
			},
			wantErr: false,
		},
		{
			name: "live_http_rejected",
			cfg: billing.Config{
				BaseURL:                      "http://payments.example.com",
				WebhookSecret:                "whsec_test123",
				PaymentMethodConfigurationID: "pmc_123",
				PortalConfigurationID:        "bpc_123",
				Live:                         true,
			},
			wantErr: true,
		},
		{
			name: "sandbox_remote_http_rejected",
			cfg: billing.Config{
				BaseURL:                      "http://remote.example.com",
				WebhookSecret:                "whsec_test123",
				PaymentMethodConfigurationID: "pmc_123",
				PortalConfigurationID:        "bpc_123",
				Live:                         false,
			},
			wantErr: true,
		},
		{
			name: "path_prefix_rejected",
			cfg: billing.Config{
				BaseURL:                      "https://payments.example.com/prefix",
				WebhookSecret:                "whsec_test123",
				PaymentMethodConfigurationID: "pmc_123",
				PortalConfigurationID:        "bpc_123",
				Live:                         true,
			},
			wantErr: true,
		},
		{
			name: "query_string_rejected",
			cfg: billing.Config{
				BaseURL:                      "https://payments.example.com?foo=bar",
				WebhookSecret:                "whsec_test123",
				PaymentMethodConfigurationID: "pmc_123",
				PortalConfigurationID:        "bpc_123",
				Live:                         true,
			},
			wantErr: true,
		},
		{
			name: "missing_webhook_secret",
			cfg: billing.Config{
				BaseURL:                      "https://payments.example.com",
				WebhookSecret:                "",
				PaymentMethodConfigurationID: "pmc_123",
				PortalConfigurationID:        "bpc_123",
				Live:                         true,
			},
			wantErr: true,
		},
		{
			name: "missing_payment_method_configuration_id",
			cfg: billing.Config{
				BaseURL:                      "https://payments.example.com",
				WebhookSecret:                "whsec_test123",
				PaymentMethodConfigurationID: "",
				PortalConfigurationID:        "bpc_123",
				Live:                         true,
			},
			wantErr: true,
		},
		{
			name: "missing_portal_configuration_id",
			cfg: billing.Config{
				BaseURL:                      "https://payments.example.com",
				WebhookSecret:                "whsec_test123",
				PaymentMethodConfigurationID: "pmc_123",
				PortalConfigurationID:        "",
				Live:                         true,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("cfg.Validate() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfigFromEnv(t *testing.T) {
	cleanEnv := func() {
		os.Unsetenv("STRIPE_API_KEY")
		os.Unsetenv("STRIPE_MODE")
		os.Unsetenv("PUBLIC_BASE_URL")
		os.Unsetenv("STRIPE_WEBHOOK_SECRET")
		os.Unsetenv("STRIPE_PAYMENT_METHOD_CONFIGURATION_ID")
		os.Unsetenv("STRIPE_PORTAL_CONFIGURATION_ID")
	}
	defer cleanEnv()

	t.Run("missing_api_key", func(t *testing.T) {
		cleanEnv()
		_, _, err := billing.LoadConfigFromEnv()
		if err == nil {
			t.Fatal("expected error for missing STRIPE_API_KEY")
		}
	})

	t.Run("invalid_mode", func(t *testing.T) {
		cleanEnv()
		os.Setenv("STRIPE_API_KEY", "sk_test_123")
		os.Setenv("STRIPE_MODE", "staging")
		_, _, err := billing.LoadConfigFromEnv()
		if err == nil {
			t.Fatal("expected error for invalid STRIPE_MODE")
		}
	})

	t.Run("mismatched_key_prefix_sandbox", func(t *testing.T) {
		cleanEnv()
		os.Setenv("STRIPE_API_KEY", "sk_live_123")
		os.Setenv("STRIPE_MODE", "sandbox")
		_, _, err := billing.LoadConfigFromEnv()
		if err == nil {
			t.Fatal("expected error for sk_live_ key in sandbox mode")
		}
	})

	t.Run("mismatched_key_prefix_live", func(t *testing.T) {
		cleanEnv()
		os.Setenv("STRIPE_API_KEY", "sk_test_123")
		os.Setenv("STRIPE_MODE", "live")
		_, _, err := billing.LoadConfigFromEnv()
		if err == nil {
			t.Fatal("expected error for sk_test_ key in live mode")
		}
	})

	t.Run("valid_sandbox_config", func(t *testing.T) {
		cleanEnv()
		os.Setenv("STRIPE_API_KEY", "rk_test_abc123")
		os.Setenv("STRIPE_MODE", "sandbox")
		os.Setenv("PUBLIC_BASE_URL", "http://localhost:3000")
		os.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_mock")
		os.Setenv("STRIPE_PAYMENT_METHOD_CONFIGURATION_ID", "pmc_mock")
		os.Setenv("STRIPE_PORTAL_CONFIGURATION_ID", "bpc_mock")

		cfg, key, err := billing.LoadConfigFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if key != "rk_test_abc123" {
			t.Errorf("got key %q, want rk_test_abc123", key)
		}
		if cfg.Live {
			t.Error("got Live=true, want Live=false")
		}
	})
}

func mockStripePrice(id, lookupKey string, cents int64, currency string, active, livemode bool, billingScheme stripe.PriceBillingScheme, priceType stripe.PriceType) map[string]any {
	return map[string]any{
		"id":             id,
		"object":         "price",
		"active":         active,
		"billing_scheme": string(billingScheme),
		"currency":       currency,
		"livemode":       livemode,
		"lookup_key":     lookupKey,
		"type":           string(priceType),
		"unit_amount":    cents,
	}
}

func TestLoadCatalogValidation(t *testing.T) {
	validPrices := []map[string]any{
		mockStripePrice("pr_full", billing.LookupKeyFullInitial, 60000, "usd", true, false, stripe.PriceBillingSchemePerUnit, stripe.PriceTypeOneTime),
		mockStripePrice("pr_three_1", billing.LookupKeyThreeInitial, 20834, "usd", true, false, stripe.PriceBillingSchemePerUnit, stripe.PriceTypeOneTime),
		mockStripePrice("pr_three_2", billing.LookupKeyThreeLater, 20833, "usd", true, false, stripe.PriceBillingSchemePerUnit, stripe.PriceTypeOneTime),
		mockStripePrice("pr_six_1", billing.LookupKeySixInitial, 10835, "usd", true, false, stripe.PriceBillingSchemePerUnit, stripe.PriceTypeOneTime),
		mockStripePrice("pr_six_2", billing.LookupKeySixLater, 10833, "usd", true, false, stripe.PriceBillingSchemePerUnit, stripe.PriceTypeOneTime),
		mockStripePrice("pr_reinstatement", billing.LookupKeyReinstatement, 2500, "usd", true, false, stripe.PriceBillingSchemePerUnit, stripe.PriceTypeOneTime),
	}

	createClient := func(handler http.HandlerFunc) (*stripe.Client, *httptest.Server) {
		ts := httptest.NewServer(handler)
		maxRetries := int64(0)
		backends := stripe.NewBackendsWithConfig(&stripe.BackendConfig{
			URL:               stripe.String(ts.URL),
			HTTPClient:        ts.Client(),
			MaxNetworkRetries: &maxRetries,
			LeveledLogger:     &stripe.LeveledLogger{Level: stripe.LevelNull},
		})
		return stripe.NewClient("sk_test_mock", stripe.WithBackends(backends)), ts
	}

	t.Run("valid_catalog", func(t *testing.T) {
		client, ts := createClient(func(w http.ResponseWriter, r *http.Request) {
			resp := map[string]any{
				"object":   "list",
				"data":     validPrices,
				"has_more": false,
			}
			_ = json.NewEncoder(w).Encode(resp)
		})
		defer ts.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		catalog, err := billing.LoadCatalog(ctx, client, false)
		if err != nil {
			t.Fatalf("unexpected error loading catalog: %v", err)
		}
		if catalog.Full != "pr_full" || catalog.ThreeFirst != "pr_three_1" || catalog.ThreeLater != "pr_three_2" ||
			catalog.SixFirst != "pr_six_1" || catalog.SixLater != "pr_six_2" || catalog.Reinstatement != "pr_reinstatement" {
			t.Fatalf("catalog price IDs mismatched: %+v", catalog)
		}
	})

	t.Run("missing_price", func(t *testing.T) {
		incompletePrices := validPrices[:5] // omit reinstatement
		client, ts := createClient(func(w http.ResponseWriter, r *http.Request) {
			resp := map[string]any{
				"object":   "list",
				"data":     incompletePrices,
				"has_more": false,
			}
			_ = json.NewEncoder(w).Encode(resp)
		})
		defer ts.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		_, err := billing.LoadCatalog(ctx, client, false)
		if err == nil {
			t.Fatal("expected error when price is missing from catalog")
		}
	})

	t.Run("wrong_unit_amount", func(t *testing.T) {
		corruptedPrices := make([]map[string]any, len(validPrices))
		copy(corruptedPrices, validPrices)
		corruptedPrices[0] = mockStripePrice("pr_full", billing.LookupKeyFullInitial, 59999, "usd", true, false, stripe.PriceBillingSchemePerUnit, stripe.PriceTypeOneTime)

		client, ts := createClient(func(w http.ResponseWriter, r *http.Request) {
			resp := map[string]any{
				"object":   "list",
				"data":     corruptedPrices,
				"has_more": false,
			}
			_ = json.NewEncoder(w).Encode(resp)
		})
		defer ts.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		_, err := billing.LoadCatalog(ctx, client, false)
		if err == nil {
			t.Fatal("expected error when price amount does not match plan registry")
		}
	})

	t.Run("wrong_livemode", func(t *testing.T) {
		corruptedPrices := make([]map[string]any, len(validPrices))
		copy(corruptedPrices, validPrices)
		corruptedPrices[0] = mockStripePrice("pr_full", billing.LookupKeyFullInitial, 60000, "usd", true, true, stripe.PriceBillingSchemePerUnit, stripe.PriceTypeOneTime)

		client, ts := createClient(func(w http.ResponseWriter, r *http.Request) {
			resp := map[string]any{
				"object":   "list",
				"data":     corruptedPrices,
				"has_more": false,
			}
			_ = json.NewEncoder(w).Encode(resp)
		})
		defer ts.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		_, err := billing.LoadCatalog(ctx, client, false)
		if err == nil {
			t.Fatal("expected error when price livemode does not match configuration")
		}
	})
}
