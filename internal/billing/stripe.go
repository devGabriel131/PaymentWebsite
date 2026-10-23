package billing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v87"

	"paymentwebsite/internal/payments"
)

const (
	LookupKeyFullInitial   = "asvab_full_initial_usd"
	LookupKeyThreeInitial  = "asvab_three_initial_usd"
	LookupKeyThreeLater    = "asvab_three_remaining_usd"
	LookupKeySixInitial    = "asvab_six_initial_usd"
	LookupKeySixLater      = "asvab_six_remaining_usd"
	LookupKeyReinstatement = "asvab_reinstatement_usd"

	ReinstatementFeeCents = 2500
)

// Config carries server-mode Stripe configuration.
type Config struct {
	BaseURL                      string
	WebhookSecret                string
	PaymentMethodConfigurationID string
	PortalConfigurationID        string
	Live                         bool
}

// Validate validates that Config adheres to trusted base URL and provider requirements.
func (c Config) Validate() error {
	if c.BaseURL == "" {
		return errors.New("PUBLIC_BASE_URL is required")
	}
	parsed, err := url.Parse(c.BaseURL)
	if err != nil || parsed.Host == "" {
		return errors.New("PUBLIC_BASE_URL must be an absolute URL with host")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("PUBLIC_BASE_URL must not contain userinfo, query, or fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return errors.New("PUBLIC_BASE_URL must not contain a path prefix")
	}
	isLocalhost := parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "::1"
	if c.Live {
		if parsed.Scheme != "https" {
			return errors.New("PUBLIC_BASE_URL must use https in live mode")
		}
	} else {
		if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLocalhost) {
			return errors.New("PUBLIC_BASE_URL must use https except localhost in sandbox mode")
		}
	}
	if strings.TrimSpace(c.WebhookSecret) == "" {
		return errors.New("STRIPE_WEBHOOK_SECRET is required")
	}
	if strings.TrimSpace(c.PaymentMethodConfigurationID) == "" {
		return errors.New("STRIPE_PAYMENT_METHOD_CONFIGURATION_ID is required")
	}
	if strings.TrimSpace(c.PortalConfigurationID) == "" {
		return errors.New("STRIPE_PORTAL_CONFIGURATION_ID is required")
	}
	return nil
}

// LoadConfigFromEnv reads and validates Stripe configuration from environment variables.
func LoadConfigFromEnv() (Config, string, error) {
	apiKey := os.Getenv("STRIPE_API_KEY")
	if apiKey == "" {
		return Config{}, "", errors.New("STRIPE_API_KEY is required")
	}
	mode := os.Getenv("STRIPE_MODE")
	if mode != "sandbox" && mode != "live" {
		return Config{}, "", errors.New("STRIPE_MODE must be exactly 'sandbox' or 'live'")
	}
	live := mode == "live"
	if live {
		if !strings.HasPrefix(apiKey, "rk_live_") && !strings.HasPrefix(apiKey, "sk_live_") {
			return Config{}, "", errors.New("STRIPE_API_KEY prefix must be rk_live_ or sk_live_ in live mode")
		}
	} else {
		if !strings.HasPrefix(apiKey, "rk_test_") && !strings.HasPrefix(apiKey, "sk_test_") {
			return Config{}, "", errors.New("STRIPE_API_KEY prefix must be rk_test_ or sk_test_ in sandbox mode")
		}
	}

	cfg := Config{
		BaseURL:                      os.Getenv("PUBLIC_BASE_URL"),
		WebhookSecret:                os.Getenv("STRIPE_WEBHOOK_SECRET"),
		PaymentMethodConfigurationID: os.Getenv("STRIPE_PAYMENT_METHOD_CONFIGURATION_ID"),
		PortalConfigurationID:        os.Getenv("STRIPE_PORTAL_CONFIGURATION_ID"),
		Live:                         live,
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, "", err
	}
	return cfg, apiKey, nil
}

// Catalog holds resolved Stripe Price IDs for finite tuition plans and reinstatement.
type Catalog struct {
	Full          string
	ThreeFirst    string
	ThreeLater    string
	SixFirst      string
	SixLater      string
	Reinstatement string
}

// NewStripeClient creates a client configured with 5-second timeouts, at most 2 retries,
// and silent logging so raw SDK payloads never leak into application logs.
func NewStripeClient(apiKey string) *stripe.Client {
	httpClient := &http.Client{
		Timeout: 5 * time.Second,
	}
	maxRetries := int64(2)
	backends := stripe.NewBackendsWithConfig(&stripe.BackendConfig{
		HTTPClient:        httpClient,
		MaxNetworkRetries: &maxRetries,
		LeveledLogger:     &stripe.LeveledLogger{Level: stripe.LevelNull},
	})
	return stripe.NewClient(apiKey, stripe.WithBackends(backends))
}

// LoadCatalog queries Stripe for all 6 catalog prices in a single batched lookup
// and validates currency, unit amount, billing scheme, and livemode.
func LoadCatalog(ctx context.Context, client *stripe.Client, live bool) (Catalog, error) {
	fullPlan, ok := payments.Find("full")
	if !ok || len(fullPlan.Amounts) < 1 {
		return Catalog{}, errors.New("payment plan 'full' not found in registry")
	}
	threePlan, ok := payments.Find("three")
	if !ok || len(threePlan.Amounts) < 2 {
		return Catalog{}, errors.New("payment plan 'three' not found in registry")
	}
	sixPlan, ok := payments.Find("six")
	if !ok || len(sixPlan.Amounts) < 2 {
		return Catalog{}, errors.New("payment plan 'six' not found in registry")
	}

	expectedAmounts := map[string]int64{
		LookupKeyFullInitial:   int64(fullPlan.Amounts[0]),
		LookupKeyThreeInitial:  int64(threePlan.Amounts[0]),
		LookupKeyThreeLater:    int64(threePlan.Amounts[1]),
		LookupKeySixInitial:    int64(sixPlan.Amounts[0]),
		LookupKeySixLater:      int64(sixPlan.Amounts[1]),
		LookupKeyReinstatement: int64(ReinstatementFeeCents),
	}

	keys := []*string{
		stripe.String(LookupKeyFullInitial),
		stripe.String(LookupKeyThreeInitial),
		stripe.String(LookupKeyThreeLater),
		stripe.String(LookupKeySixInitial),
		stripe.String(LookupKeySixLater),
		stripe.String(LookupKeyReinstatement),
	}

	params := &stripe.PriceListParams{
		LookupKeys: keys,
	}

	iter := client.V1Prices.List(ctx, params)
	resolved := make(map[string]*stripe.Price)
	for price, err := range iter.All(ctx) {
		if err != nil {
			return Catalog{}, fmt.Errorf("list prices from stripe: %w", err)
		}
		if price == nil || price.LookupKey == "" {
			continue
		}
		if _, exists := resolved[price.LookupKey]; exists {
			return Catalog{}, fmt.Errorf("conflicting duplicate price found for lookup key %q", price.LookupKey)
		}
		resolved[price.LookupKey] = price
	}

	for lookupKey, expectedCents := range expectedAmounts {
		price, found := resolved[lookupKey]
		if !found {
			return Catalog{}, fmt.Errorf("missing stripe price for lookup key %q", lookupKey)
		}
		if !price.Active {
			return Catalog{}, fmt.Errorf("stripe price %q (%s) is not active", lookupKey, price.ID)
		}
		if price.Livemode != live {
			return Catalog{}, fmt.Errorf("stripe price %q livemode (%t) does not match configured live mode (%t)", lookupKey, price.Livemode, live)
		}
		if price.Currency != stripe.CurrencyUSD {
			return Catalog{}, fmt.Errorf("stripe price %q has currency %q, expected usd", lookupKey, price.Currency)
		}
		if price.BillingScheme != stripe.PriceBillingSchemePerUnit {
			return Catalog{}, fmt.Errorf("stripe price %q has billing scheme %q, expected per_unit", lookupKey, price.BillingScheme)
		}
		if price.Type != stripe.PriceTypeOneTime {
			return Catalog{}, fmt.Errorf("stripe price %q has type %q, expected one_time", lookupKey, price.Type)
		}
		if price.UnitAmount != expectedCents {
			return Catalog{}, fmt.Errorf("stripe price %q has unit amount %d cents, expected %d cents", lookupKey, price.UnitAmount, expectedCents)
		}
	}

	return Catalog{
		Full:          resolved[LookupKeyFullInitial].ID,
		ThreeFirst:    resolved[LookupKeyThreeInitial].ID,
		ThreeLater:    resolved[LookupKeyThreeLater].ID,
		SixFirst:      resolved[LookupKeySixInitial].ID,
		SixLater:      resolved[LookupKeySixLater].ID,
		Reinstatement: resolved[LookupKeyReinstatement].ID,
	}, nil
}
