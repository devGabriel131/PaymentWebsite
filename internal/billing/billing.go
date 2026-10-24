package billing

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v87"
	"gorm.io/gorm"
)

// Bounded review reasons.
const (
	ReviewUnresolvedProviderCreate = "unresolved_provider_create"
	ReviewInvoiceMismatch          = "invoice_mismatch"
	ReviewMissingInvoice           = "missing_invoice"
	ReviewPaymentReview            = "payment_review"
	ReviewUnsupportedPaymentMethod = "unsupported_payment_method"
	ReviewConfigurationMismatch    = "configuration_mismatch"
)

// Exported error values.
var (
	ErrUnavailable     = errors.New("resource unavailable")
	ErrPlanLocked      = errors.New("plan is locked")
	ErrPaymentPending  = errors.New("payment is pending")
	ErrNotSuspended    = errors.New("student is not suspended")
	ErrNeedsReview     = errors.New("financial state needs review")
	ErrInvoiceNotReady = errors.New("invoice is not ready")
	ErrStaffRequired   = errors.New("staff identity is required")
)

// CheckoutResult returns the outcome of initiating or resuming a checkout attempt.
type CheckoutResult struct {
	URL     string
	Pending bool
	Funded  bool
}

// SummaryInstallment represents the status of one tuition installment in Summary.
type SummaryInstallment struct {
	ID           uint64
	Number       int
	AmountCents  int64
	DueAt        *time.Time
	Status       string
	PaidAt       *time.Time
	InvoiceReady bool
}

// Summary exposes financial standing, active/suspended status, and review reasons.
type Summary struct {
	PlanID                         string
	PlanName                       string
	TotalCents                     int64
	Funded                         bool
	FundedAt                       *time.Time
	CashPaidCents                  int64
	TuitionPaidCents               int64
	OtherSettlementCents           int64
	RemainingTuitionCents          int64
	OverdueTuitionCents            int64
	ReinstatementFeeRemainingCents int64
	ReinstatementFeeID             uint64
	ReinstatementInvoiceReady      bool
	Installments                   []SummaryInstallment
	IsActive                       bool
	IsSuspended                    bool
	LastSyncedAt                   *time.Time
	ReviewReasons                  []string
}

// ReturnStatus holds customer-facing status returned from Stripe-hosted checkout.
// Contains only payment status and amount; never leaks student identity, contact, or history.
type ReturnStatus struct {
	Status      string
	AmountCents int64
}

// Service manages tuition billing, Stripe operations, webhooks, and background worker reconciliation.
type Service struct {
	db      *gorm.DB
	client  *stripe.Client
	cfg     Config
	catalog Catalog
	changed func()
	log     *slog.Logger
	// wake nudges Run to drain now; buffered 1 so concurrent webhooks coalesce.
	wake chan struct{}
}

// New creates a verified billing service.
func New(db *gorm.DB, client *stripe.Client, cfg Config, catalog Catalog, changed func(), log *slog.Logger) (*Service, error) {
	if db == nil {
		return nil, errors.New("db is required")
	}
	if client == nil {
		return nil, errors.New("stripe client is required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	if changed == nil {
		changed = func() {}
	}
	return &Service{
		db:      db,
		client:  client,
		cfg:     cfg,
		catalog: catalog,
		changed: changed,
		log:     log,
		wake:    make(chan struct{}, 1),
	}, nil
}

// contractFor finds the contract behind a student; unknown or out-of-range IDs are unavailable.
func (s *Service) contractFor(ctx context.Context, studentID uint64) (Contract, error) {
	var c Contract
	if studentID == 0 || studentID > 1<<63-1 {
		return c, ErrUnavailable
	}
	err := require(firstRow(s.db.WithContext(ctx), &c, "student_id = ?", studentID))
	return c, err
}

// withStaffContract runs fn under the contract's workflow lease. A contract a worker holds is
// mid-provider-work, so staff get ErrPaymentPending to retry instead of racing it.
func (s *Service) withStaffContract(ctx context.Context, id uint64, fn func(lease) error) error {
	err := s.withContract(ctx, id, fn)
	if errors.Is(err, errContractBusy) {
		return ErrPaymentPending
	}
	return err
}

// markSynced records a staff-triggered sync the way the worker does, so the next scheduled
// sync accounts for whatever the action just created.
func (s *Service) markSynced(ctx context.Context, l lease) error {
	now := time.Now().UTC()
	next, err := s.nextSync(ctx, l.id, now)
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).Model(&Contract{}).Where("id = ? AND lease_token = ?", l.id, l.token).
		Updates(map[string]any{"last_synced_at": now, "next_sync_at": next}).Error
}

// reinstatable returns the contract's open episode with its contract and obligations, or
// ErrNotSuspended when none is open, or ErrNeedsReview while any review reason is unresolved.
func (s *Service) reinstatable(ctx context.Context, contractID uint64) (ep Suspension, c Contract, obligations []Obligation, err error) {
	db := s.db.WithContext(ctx)
	found, err := firstRow(db, &ep, "contract_id = ? AND closed_at IS NULL", contractID)
	if err != nil {
		return
	}
	if !found {
		err = ErrNotSuspended
		return
	}
	if err = require(firstRow(db, &c, "id = ?", contractID)); err != nil {
		return
	}
	if err = db.Where("contract_id = ?", contractID).Find(&obligations).Error; err != nil {
		return
	}
	if hasReview(c, obligations) {
		err = ErrNeedsReview
	}
	return
}

// RequestReinstatement creates, or resumes, the open episode's single 2500-cent fee invoice for
// staff. Original overdue invoices are refreshed first and are never duplicated onto the fee.
func (s *Service) RequestReinstatement(ctx context.Context, studentID uint64, staff string) (*Summary, error) {
	if strings.TrimSpace(staff) == "" {
		return nil, ErrStaffRequired
	}
	c, err := s.contractFor(ctx, studentID)
	if err != nil {
		return nil, err
	}
	// Refuse early, before any provider work; the decision is repeated after the refresh.
	if _, _, _, err := s.reinstatable(ctx, c.ID); err != nil {
		return nil, err
	}
	err = s.withStaffContract(ctx, c.ID, func(l lease) error {
		if err := s.reconcileContract(ctx, l); err != nil {
			return err
		}
		if err := s.requestFee(ctx, l, staff); err != nil {
			return err
		}
		return s.markSynced(ctx, l)
	})
	if err != nil {
		return nil, err
	}
	return s.ForStudent(ctx, studentID)
}

// requestFee reuses the episode's fee obligation or inserts it, runs the persisted invoice
// stages, then records who requested it first. A repeated request resumes where one stopped
// and never changes RequestedBy or the request time.
func (s *Service) requestFee(ctx context.Context, l lease, staff string) error {
	ep, c, obligations, err := s.reinstatable(ctx, l.id)
	if err != nil {
		return err
	}
	if c.StripeCustomerID == nil {
		return ErrUnavailable
	}
	var fee Obligation
	if i := slices.IndexFunc(obligations, func(o Obligation) bool {
		return o.Kind == "reinstatement" && o.SuspensionID != nil && *o.SuspensionID == ep.ID
	}); i >= 0 {
		fee = obligations[i]
	} else {
		if s.catalog.Reinstatement == "" {
			return errors.New("billing: catalog has no reinstatement price")
		}
		fee = Obligation{
			ContractID:            c.ID,
			Reference:             rand.Text(),
			Kind:                  "reinstatement",
			AmountCents:           ReinstatementFeeCents,
			StripePriceID:         s.catalog.Reinstatement,
			SuspensionID:          &ep.ID,
			CollectionDisposition: "collect",
			AmountRemainingCents:  ReinstatementFeeCents,
		}
		if err := s.leased(ctx, l, func(tx *gorm.DB, _ *Contract) error { return tx.Create(&fee).Error }); err != nil {
			return err
		}
	}
	if err := s.scheduleOne(ctx, l, c, &fee); err != nil {
		if errors.Is(err, errReviewed) {
			return ErrNeedsReview
		}
		return err
	}
	err = s.leased(ctx, l, func(tx *gorm.DB, _ *Contract) error {
		return tx.Model(&Suspension{}).Where("id = ? AND reinstatement_requested_at IS NULL", ep.ID).
			Updates(map[string]any{"reinstatement_requested_at": time.Now().UTC(), "requested_by": staff}).Error
	})
	if err == nil {
		s.changed()
	}
	return err
}

// InvoiceURL returns the current hosted URL of one of the student's invoices. Links expire,
// so it is fetched on demand and never stored.
func (s *Service) InvoiceURL(ctx context.Context, studentID, obligationID uint64) (string, error) {
	c, err := s.contractFor(ctx, studentID)
	if err != nil {
		return "", err
	}
	if obligationID == 0 || obligationID > 1<<63-1 {
		return "", ErrUnavailable
	}
	var o Obligation
	if err := require(firstRow(s.db.WithContext(ctx), &o, "id = ? AND contract_id = ?", obligationID, c.ID)); err != nil {
		return "", err
	}
	if o.StripeInvoiceID == nil {
		return "", ErrInvoiceNotReady
	}
	inv, err := s.client.V1Invoices.Retrieve(ctx, *o.StripeInvoiceID, nil)
	if isNotFound(err) {
		return "", ErrInvoiceNotReady
	}
	if err != nil {
		return "", err
	}
	if inv.Deleted || inv.HostedInvoiceURL == "" {
		return "", ErrInvoiceNotReady
	}
	return inv.HostedInvoiceURL, nil
}

// PortalURL creates a short-lived Stripe customer-portal link for the contract's own Customer.
// It never marks anything paid or reactivates.
func (s *Service) PortalURL(ctx context.Context, studentID uint64, _ string) (string, error) {
	c, err := s.contractFor(ctx, studentID)
	if err != nil {
		return "", err
	}
	if c.StripeCustomerID == nil {
		return "", ErrUnavailable
	}
	session, err := s.client.V1BillingPortalSessions.Create(ctx, &stripe.BillingPortalSessionCreateParams{
		Customer:      stripe.String(*c.StripeCustomerID),
		Configuration: stripe.String(s.cfg.PortalConfigurationID),
		Locale:        stripe.String("es"),
		ReturnURL:     stripe.String(strings.TrimSuffix(s.cfg.BaseURL, "/") + "/management/students/" + strconv.FormatUint(studentID, 10)),
	})
	if err != nil {
		return "", err
	}
	return session.URL, nil
}

// ReconcileStudent refreshes the student's existing Stripe resources for staff: in-flight
// Checkouts, invoices, then deadlines and reactivation. It never creates a fresh charge.
func (s *Service) ReconcileStudent(ctx context.Context, studentID uint64, _ string) (*Summary, error) {
	c, err := s.contractFor(ctx, studentID)
	if err != nil {
		return nil, err
	}
	err = s.withStaffContract(ctx, c.ID, func(l lease) error {
		if err := s.reconcileContract(ctx, l); err != nil {
			return err
		}
		return s.markSynced(ctx, l)
	})
	if err != nil {
		return nil, err
	}
	return s.ForStudent(ctx, studentID)
}
