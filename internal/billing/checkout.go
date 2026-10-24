package billing

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v87"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"paymentwebsite/internal/links"
	"paymentwebsite/internal/logging"
	"paymentwebsite/internal/payments"
	"paymentwebsite/internal/students"
)

// integrationIdentifier is Stripe's non-unique grouping label for this integration's Checkout
// Sessions; a constant keeps idempotent create replays byte-identical.
const integrationIdentifier = "tuition_checkout"

// activeAttemptStates are the nonterminal states guarded by idx_active_checkout_attempt.
var activeAttemptStates = []string{"creating", "open", "processing", "expiring", "uncertain"}

// inFlightAttemptStates are the active states where money may be in transit, blocking contact edits.
var inFlightAttemptStates = []string{"processing", "uncertain", "expiring"}

var (
	forUpdate = clause.Locking{Strength: clause.LockingStrengthUpdate}

	ErrContactLocked    = errors.New("billing: contact is locked")
	errCheckoutMismatch = errors.New("billing: checkout session does not match its attempt")
)

// checkoutStep is what one start transaction decided.
type checkoutStep struct {
	done     *CheckoutResult  // set when no provider call remains
	expire   *CheckoutAttempt // another plan's open attempt, claimed as "expiring"
	changed  bool             // the contract's visible quote changed
	contract Contract
	first    Obligation
	attempt  CheckoutAttempt // new "creating" attempt
}

// StartCheckout creates or resumes the single hosted first-payment attempt for the
// enrollment behind sourceLinkHash. Before funding it may switch plans, but only after
// the previous plan's open Session is confirmed expired.
func (s *Service) StartCheckout(ctx context.Context, sourceLinkHash []byte, planID string) (CheckoutResult, error) {
	if len(sourceLinkHash) != sha256.Size {
		return CheckoutResult{}, ErrUnavailable
	}
	plan, ok := payments.Find(planID)
	if !ok {
		return CheckoutResult{}, payments.ErrUnknownPlan
	}
	callCtx, cancel := context.WithTimeout(ctx, providerBudget)
	defer cancel()

	// Each pass claims at most one stale open attempt; a competing start can only
	// create another by winning a race, and callCtx bounds the retries.
	for {
		step, err := s.beginCheckout(callCtx, sourceLinkHash, plan)
		if err != nil {
			return CheckoutResult{}, err
		}
		if step.changed {
			s.changed()
		}
		if step.expire != nil {
			expired, err := s.expireAttempt(callCtx, step.expire)
			if err != nil {
				return CheckoutResult{}, err
			}
			if !expired {
				return CheckoutResult{Pending: true}, nil
			}
			continue
		}
		if step.done != nil {
			return *step.done, nil
		}
		return s.createSession(callCtx, step.contract, step.first, step.attempt)
	}
}

// UpdateContact updates only the canonical student row for an enrollment, guarded against
// concurrent checkouts and blocked once funded or while payment is in flight.
func (s *Service) UpdateContact(ctx context.Context, sourceLinkHash []byte, details students.Details) error {
	if len(sourceLinkHash) != sha256.Size {
		return ErrUnavailable
	}
	contact := map[string]any{
		"name": details.Name, "email": details.Email, "phone": details.Phone,
		"municipality": details.Municipality, "date_of_birth": details.DateOfBirth,
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var link links.Link
		if err := require(firstRow(tx.Clauses(forUpdate).Scopes(links.Usable), &link,
			"token_hash = ? AND submitted_at IS NOT NULL", sourceLinkHash)); err != nil {
			return err
		}
		var student students.Student
		if err := require(firstRow(tx, &student, "source_link_hash = ?", sourceLinkHash)); err != nil {
			return err
		}
		var contract Contract
		exists, err := firstRow(tx.Clauses(forUpdate), &contract, "student_id = ?", student.ID)
		if err != nil {
			return err
		}
		if exists {
			if contract.FundedAt != nil {
				return ErrContactLocked
			}
			var attempt CheckoutAttempt
			inFlight, err := firstRow(tx.Clauses(forUpdate), &attempt,
				"contract_id = ? AND state IN ?", contract.ID, inFlightAttemptStates)
			if err != nil {
				return err
			}
			if inFlight {
				return ErrContactLocked
			}
		}
		if err := touch(tx, student.ID); err != nil {
			return err
		}
		return tx.Model(&students.Student{}).Where("id = ?", student.ID).Updates(contact).Error
	})
	if err != nil {
		return err
	}
	s.changed()
	s.wakeWorker()
	return nil
}

// beginCheckout runs the locked decision transaction: Link, then Contract, then attempt.
// It persists the new attempt's identity before any Checkout call is made.
func (s *Service) beginCheckout(ctx context.Context, hash []byte, plan payments.Plan) (step checkoutStep, err error) {
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var link links.Link
		if err := require(firstRow(tx.Clauses(forUpdate).Scopes(links.Usable), &link,
			"token_hash = ? AND submitted_at IS NOT NULL AND terms_accepted_at IS NOT NULL", hash)); err != nil {
			return err
		}
		var student students.Student
		if err := require(firstRow(tx, &student, "source_link_hash = ?", hash)); err != nil {
			return err
		}

		var contract Contract
		exists, err := firstRow(tx.Clauses(forUpdate), &contract, "student_id = ?", student.ID)
		if err != nil {
			return err
		}
		if !exists {
			syncGrace := time.Now().UTC().Add(1 * time.Minute)
			contract = Contract{
				StudentID:        student.ID,
				Reference:        rand.Text(),
				PlanID:           plan.ID,
				PlanName:         plan.Name,
				InstallmentCount: len(plan.Amounts),
				TotalCents:       plan.TotalCents(),
				NextSyncAt:       &syncGrace,
			}
			if err := tx.Create(&contract).Error; err != nil {
				return err
			}
			if err := s.createObligations(tx, contract.ID, plan); err != nil {
				return err
			}
			step.changed = true
		} else if contract.FundedAt != nil {
			if contract.PlanID != plan.ID {
				return ErrPlanLocked
			}
			step.done = &CheckoutResult{Funded: true}
			return nil
		} else {
			var attempt CheckoutAttempt
			active, err := firstRow(tx.Clauses(forUpdate), &attempt, "contract_id = ? AND state IN ?", contract.ID, activeAttemptStates)
			if err != nil {
				return err
			}
			if active {
				open := attempt.State == "open"
				switch {
				case open && attempt.PlanID == plan.ID && attempt.CheckoutURL != nil:
					step.done = &CheckoutResult{URL: *attempt.CheckoutURL}
				case open && attempt.PlanID != plan.ID && attempt.StripeSessionID != nil:
					if err := tx.Model(&CheckoutAttempt{}).Where("id = ?", attempt.ID).Update("state", "expiring").Error; err != nil {
						return err
					}
					attempt.State = "expiring"
					step.expire = &attempt
				default:
					step.done = &CheckoutResult{Pending: true}
				}
				return nil
			}
			if contract.PlanID != plan.ID {
				// Every earlier attempt is terminal, so no Session can still collect the old quote.
				// Unfunded contracts have no provider invoices, so the old obligations are plain rows.
				if err := tx.Where("contract_id = ? AND kind = 'tuition'", contract.ID).Delete(&Obligation{}).Error; err != nil {
					return err
				}
				if err := tx.Model(&Contract{}).Where("id = ?", contract.ID).Updates(map[string]any{
					"plan_id":           plan.ID,
					"plan_name":         plan.Name,
					"installment_count": len(plan.Amounts),
					"total_cents":       plan.TotalCents(),
				}).Error; err != nil {
					return err
				}
				contract.PlanID, contract.PlanName = plan.ID, plan.Name
				contract.InstallmentCount, contract.TotalCents = len(plan.Amounts), plan.TotalCents()
				if err := s.createObligations(tx, contract.ID, plan); err != nil {
					return err
				}
				step.changed = true
			}
		}

		var first Obligation
		if err := require(firstRow(tx, &first, "contract_id = ? AND kind = 'tuition' AND number = 1", contract.ID)); err != nil {
			return err
		}

		contractUpdates := map[string]any{}
		if contract.StripeCustomerID == nil {
			// ponytail: the Link and Contract locks stay held across this once-per-enrollment
			// Stripe call so concurrent starts share one Customer; the stable key makes a retry
			// safe, move it after commit if lock waits ever matter.
			params := &stripe.CustomerCreateParams{
				Name:             stripe.String(student.Name),
				Email:            stripe.String(student.Email),
				PreferredLocales: []*string{stripe.String("es")},
				Metadata:         map[string]string{"tuition_contract": contract.Reference},
			}
			params.SetIdempotencyKey(fmt.Sprintf("tuition:%s:customer", contract.Reference))
			customer, err := s.client.V1Customers.Create(ctx, params)
			if err != nil {
				return fmt.Errorf("billing: create stripe customer: %w", err)
			}
			contract.StripeCustomerID = &customer.ID
			contract.SyncedCustomerName = student.Name
			contract.SyncedCustomerEmail = student.Email
			contractUpdates["stripe_customer_id"] = customer.ID
			contractUpdates["synced_customer_name"] = student.Name
			contractUpdates["synced_customer_email"] = student.Email
		}

		attempt := CheckoutAttempt{
			ContractID:        contract.ID,
			Reference:         rand.Text(),
			PlanID:            plan.ID,
			FirstAmountCents:  first.AmountCents,
			StripePriceID:     first.StripePriceID,
			CreationStartedAt: time.Now().UTC(),
			State:             "creating",
		}
		if err := tx.Create(&attempt).Error; err != nil {
			return err
		}
		if len(contractUpdates) > 0 {
			if err := tx.Model(&Contract{}).Where("id = ?", contract.ID).Updates(contractUpdates).Error; err != nil {
				return err
			}
		}
		step.contract, step.first, step.attempt = contract, first, attempt
		return nil
	})
	return step, err
}

// expireAttempt expires the claimed open Session of an abandoned plan. expired is true only
// once the Session is terminally unpaid; otherwise the caller reports the checkout as pending.
// Unresolved outcomes stay "expiring" for the worker, never unlocking a possibly paid quote.
func (s *Service) expireAttempt(ctx context.Context, attempt *CheckoutAttempt) (expired bool, err error) {
	session, err := s.client.V1CheckoutSessions.Expire(ctx, *attempt.StripeSessionID, nil)
	if err != nil {
		// Sessions that already ended reject Expire; their current state decides.
		session, err = s.client.V1CheckoutSessions.Retrieve(ctx, *attempt.StripeSessionID, nil)
	}
	if err == nil {
		saveCtx, cancel := persistCtx(ctx)
		defer cancel()
		switch {
		case session.PaymentStatus == stripe.CheckoutSessionPaymentStatusPaid || session.Status == stripe.CheckoutSessionStatusComplete:
			return false, s.setAttempt(saveCtx, attempt.ID, "expiring", "processing", nil)
		case session.Status == stripe.CheckoutSessionStatusExpired:
			return true, s.setAttempt(saveCtx, attempt.ID, "expiring", "expired", nil)
		}
	}
	s.log.LogAttrs(ctx, slog.LevelWarn, "checkout_expire_unresolved",
		append(logging.ErrorAttrs(err), slog.String("attempt", attempt.Reference))...)
	return false, nil
}

// createSession calls Stripe for the already persisted attempt and records the outcome.
func (s *Service) createSession(ctx context.Context, contract Contract, first Obligation, attempt CheckoutAttempt) (CheckoutResult, error) {
	session, err := s.client.V1CheckoutSessions.Create(ctx, s.sessionParams(contract, first, attempt))
	// Record the outcome even if the browser went away or the budget just ran out.
	saveCtx, cancel := persistCtx(ctx)
	defer cancel()

	if err != nil {
		if outcomeUnknown(err) {
			// A failed write leaves the attempt as it was; the worker resumes it the same way.
			_ = s.setAttempt(saveCtx, attempt.ID, attempt.State, "uncertain", nil)
			s.log.LogAttrs(ctx, slog.LevelWarn, "checkout_create_uncertain",
				append(logging.ErrorAttrs(err), slog.String("attempt", attempt.Reference))...)
			return CheckoutResult{Pending: true}, nil
		}
		return CheckoutResult{}, errors.Join(
			fmt.Errorf("billing: checkout session rejected: %w", err),
			s.setAttempt(saveCtx, attempt.ID, attempt.State, "failed", nil))
	}

	if !sessionMatches(contract, attempt, session) || session.URL == "" {
		// The URL is never exposed, so nobody can pay this Session; keep its ID for staff and webhooks.
		return CheckoutResult{}, errors.Join(errCheckoutMismatch,
			s.setAttempt(saveCtx, attempt.ID, attempt.State, "failed", map[string]any{"stripe_session_id": session.ID}))
	}

	if err := s.setAttempt(saveCtx, attempt.ID, attempt.State, "open", map[string]any{
		"stripe_session_id": session.ID,
		"checkout_url":      session.URL,
	}); err != nil {
		return CheckoutResult{}, err
	}
	return CheckoutResult{URL: session.URL}, nil
}

// sessionParams builds the hosted Checkout request from persisted attempt values only.
func (s *Service) sessionParams(contract Contract, first Obligation, attempt CheckoutAttempt) *stripe.CheckoutSessionCreateParams {
	metadata := map[string]string{
		"tuition_contract":   contract.Reference,
		"tuition_obligation": first.Reference,
		"tuition_attempt":    attempt.Reference,
	}
	base := strings.TrimSuffix(s.cfg.BaseURL, "/")
	params := &stripe.CheckoutSessionCreateParams{
		Mode:                       stripe.String(string(stripe.CheckoutSessionModePayment)),
		Currency:                   stripe.String("usd"),
		Customer:                   stripe.String(*contract.StripeCustomerID),
		ClientReferenceID:          stripe.String(attempt.Reference),
		IntegrationIdentifier:      stripe.String(integrationIdentifier),
		Locale:                     stripe.String("es"),
		PaymentMethodConfiguration: stripe.String(s.cfg.PaymentMethodConfigurationID),
		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{{
			Price:    stripe.String(attempt.StripePriceID),
			Quantity: stripe.Int64(1),
		}},
		InvoiceCreation: &stripe.CheckoutSessionCreateInvoiceCreationParams{
			Enabled:     stripe.Bool(true),
			InvoiceData: &stripe.CheckoutSessionCreateInvoiceCreationInvoiceDataParams{Metadata: metadata},
		},
		AdaptivePricing: &stripe.CheckoutSessionCreateAdaptivePricingParams{Enabled: stripe.Bool(false)},
		SuccessURL:      stripe.String(base + "/payments/return?session_id={CHECKOUT_SESSION_ID}"),
		CancelURL:       stripe.String(base + "/payments/return?cancelled=1"),
	}
	if contract.InstallmentCount > 1 {
		// Only installment plans have later invoices that need the saved card.
		params.PaymentIntentData = &stripe.CheckoutSessionCreatePaymentIntentDataParams{
			SetupFutureUsage: stripe.String(string(stripe.PaymentIntentSetupFutureUsageOffSession)),
			Metadata:         metadata,
		}
		params.CustomText = &stripe.CheckoutSessionCreateCustomTextParams{
			Submit: &stripe.CheckoutSessionCreateCustomTextSubmitParams{
				Message: stripe.String("Primer pago de matrícula. Los pagos restantes se procesarán automáticamente en las fechas acordadas."),
			},
		}
		params.WalletOptions = &stripe.CheckoutSessionCreateWalletOptionsParams{
			Link: &stripe.CheckoutSessionCreateWalletOptionsLinkParams{
				Display: stripe.String(string(stripe.CheckoutSessionWalletOptionsLinkDisplayNever)),
			},
		}
	}
	params.SetIdempotencyKey(fmt.Sprintf("tuition:%s:checkout", attempt.Reference))
	return params
}

// createObligations quotes one tuition obligation per installment with its catalog price.
func (s *Service) createObligations(tx *gorm.DB, contractID uint64, plan payments.Plan) error {
	obligations := make([]Obligation, len(plan.Amounts))
	for i, amount := range plan.Amounts {
		price := s.catalog[priceKey(plan.ID, i)]
		if price == "" {
			return fmt.Errorf("billing: catalog has no price for plan %q installment %d", plan.ID, i+1)
		}
		obligations[i] = Obligation{
			ContractID:            contractID,
			Reference:             rand.Text(),
			Kind:                  "tuition",
			Number:                i + 1,
			AmountCents:           amount,
			AmountRemainingCents:  amount,
			StripePriceID:         price,
			CollectionDisposition: "collect",
		}
	}
	return tx.Create(&obligations).Error
}

// setAttempt moves an attempt between states; from keeps a newer state from being overwritten.
func (s *Service) setAttempt(ctx context.Context, id uint64, from, to string, extra map[string]any) error {
	updates := map[string]any{"state": to}
	maps.Copy(updates, extra)
	return s.db.WithContext(ctx).Model(&CheckoutAttempt{}).Where("id = ? AND state = ?", id, from).Updates(updates).Error
}

// persistCtx outlives the request so work that follows a committed outcome (recording a provider result, syncing a provider update) still runs.
func persistCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

// outcomeUnknown reports whether Stripe may have acted despite err: transport failures,
// timeouts, cancellation, 5xx, rate limits and in-flight idempotency conflicts.
func outcomeUnknown(err error) bool {
	var stripeErr *stripe.Error
	if !errors.As(err, &stripeErr) {
		return true
	}
	code := stripeErr.HTTPStatusCode
	rejected := code >= http.StatusBadRequest && code < http.StatusInternalServerError &&
		code != http.StatusTooManyRequests && code != http.StatusConflict
	return !rejected
}

// firstRow loads the first row matching query; found is false when none match.
func firstRow(db *gorm.DB, dest any, query string, args ...any) (found bool, err error) {
	result := db.Where(query, args...).Limit(1).Find(dest)
	return result.RowsAffected > 0, result.Error
}

// require turns a missing row into ErrUnavailable.
func require(found bool, err error) error {
	if err == nil && !found {
		return ErrUnavailable
	}
	return err
}
