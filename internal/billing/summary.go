package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"time"

	"github.com/stripe/stripe-go/v87"

	"paymentwebsite/internal/students"
)

// ForLink returns the summary for the student enrolled from sourceLinkHash.
func (s *Service) ForLink(ctx context.Context, sourceLinkHash []byte) (*Summary, error) {
	if len(sourceLinkHash) != sha256.Size {
		return nil, ErrUnavailable
	}
	var student students.Student
	found, err := firstRow(s.db.WithContext(ctx), &student, "source_link_hash = ?", sourceLinkHash)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrUnavailable
	}
	return s.forStudent(ctx, student)
}

// UpdateCustomerContact updates the Stripe Customer's name and email for the student
// enrolled from sourceLinkHash, if the contract already has a StripeCustomerID.
// It runs on its own 5s context: the caller has already committed the edit, so a
// disconnected client must not drop the sync.
// ponytail: best-effort, a failed update is logged by the caller and never retried; stamp
// the contract and let the billing worker retry if Stripe drift ever matters.
func (s *Service) UpdateCustomerContact(ctx context.Context, sourceLinkHash []byte, name, email string) error {
	if len(sourceLinkHash) != sha256.Size {
		return nil
	}
	ctx, cancel := persistCtx(ctx)
	defer cancel()
	var student students.Student
	found, err := firstRow(s.db.WithContext(ctx), &student, "source_link_hash = ?", sourceLinkHash)
	if err != nil || !found {
		return err
	}
	var contract Contract
	found, err = firstRow(s.db.WithContext(ctx), &contract, "student_id = ?", student.ID)
	if err != nil || !found || contract.StripeCustomerID == nil || *contract.StripeCustomerID == "" {
		return err
	}
	_, err = s.client.V1Customers.Update(ctx, *contract.StripeCustomerID, &stripe.CustomerUpdateParams{
		Name:  stripe.String(name),
		Email: stripe.String(email),
	})
	return err
}

// ForStudent returns the financial summary, or nil when the student or contract does not exist.
func (s *Service) ForStudent(ctx context.Context, studentID uint64) (*Summary, error) {
	if studentID == 0 || studentID > 1<<63-1 {
		return nil, nil
	}
	var student students.Student
	if found, err := firstRow(s.db.WithContext(ctx), &student, "id = ?", studentID); err != nil || !found {
		return nil, err
	}
	return s.forStudent(ctx, student)
}

// FundedTimes maps hex link token hash -> contract FundedAt for funded enrollments.
func (s *Service) FundedTimes(ctx context.Context, sourceHashes [][]byte) (map[string]time.Time, error) {
	times := map[string]time.Time{}
	if len(sourceHashes) == 0 {
		return times, nil
	}
	var rows []struct {
		SourceLinkHash []byte
		FundedAt       time.Time
	}
	err := s.db.WithContext(ctx).Model(&Contract{}).
		Select("students.source_link_hash, billing_contracts.funded_at").
		Joins("JOIN students ON students.id = billing_contracts.student_id").
		Where("students.source_link_hash IN ? AND billing_contracts.funded_at IS NOT NULL", sourceHashes).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		times[hex.EncodeToString(row.SourceLinkHash)] = row.FundedAt
	}
	return times, nil
}

// IsFunded reports whether the enrollment from sourceLinkHash has a confirmed first payment.
// A link without a student or contract has paid nothing.
func (s *Service) IsFunded(ctx context.Context, sourceLinkHash []byte) (bool, error) {
	if len(sourceLinkHash) != sha256.Size {
		return false, nil
	}
	var count int64
	err := s.db.WithContext(ctx).Model(&Contract{}).
		Joins("JOIN students ON students.id = billing_contracts.student_id").
		Where("students.source_link_hash = ? AND billing_contracts.funded_at IS NOT NULL", sourceLinkHash).
		Count(&count).Error
	return count > 0, err
}

func (s *Service) forStudent(ctx context.Context, student students.Student) (*Summary, error) {
	var contract Contract
	if found, err := firstRow(s.db.WithContext(ctx), &contract, "student_id = ?", student.ID); err != nil || !found {
		return nil, err
	}
	var obligations []Obligation
	if err := s.db.WithContext(ctx).Where("contract_id = ?", contract.ID).Order("number ASC").Find(&obligations).Error; err != nil {
		return nil, err
	}
	var payments []Payment
	if err := s.db.WithContext(ctx).Where("contract_id = ?", contract.ID).Find(&payments).Error; err != nil {
		return nil, err
	}
	var suspension Suspension
	suspended, err := firstRow(s.db.WithContext(ctx), &suspension, "contract_id = ? AND closed_at IS NULL", contract.ID)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	summary := &Summary{
		PlanID:       contract.PlanID,
		PlanName:     contract.PlanName,
		TotalCents:   contract.TotalCents,
		Funded:       contract.FundedAt != nil,
		FundedAt:     contract.FundedAt,
		IsActive:     student.IsActive,
		IsSuspended:  suspended,
		LastSyncedAt: contract.LastSyncedAt,
	}
	for _, p := range payments {
		summary.CashPaidCents += p.GrossReceivedCents - p.GrossRefundedCents
	}
	addReason := func(reason *string) {
		if reason != nil && !slices.Contains(summary.ReviewReasons, *reason) {
			summary.ReviewReasons = append(summary.ReviewReasons, *reason)
		}
	}
	addReason(contract.AttentionReason)
	for _, o := range obligations {
		summary.OtherSettlementCents += o.OtherSettlementCents
		addReason(o.ReviewReason)
		overdue := o.DueAt != nil && o.DueAt.Before(now)
		switch o.Kind {
		case "reinstatement":
			if o.CollectionDisposition == "collect" { // a corrected fee is no longer owed
				summary.ReinstatementFeeRemainingCents += o.AmountRemainingCents
				if o.ID > summary.ReinstatementFeeID { // the newest fee belongs to the current episode
					summary.ReinstatementFeeID, summary.ReinstatementInvoiceReady = o.ID, o.invoiceReady()
				}
			}
		case "tuition":
			summary.RemainingTuitionCents += o.AmountRemainingCents
			summary.TuitionPaidCents += o.AmountPaidCents
			if overdue {
				summary.OverdueTuitionCents += o.AmountRemainingCents
			}
			status := "scheduled"
			switch {
			case o.FirstSettledAt != nil || o.AmountRemainingCents == 0:
				status = "paid"
			case o.FailureOrActionRequired:
				status = "action_required"
			case overdue:
				status = "overdue"
			}
			summary.Installments = append(summary.Installments, SummaryInstallment{
				ID:           o.ID,
				Number:       o.Number,
				AmountCents:  o.AmountCents,
				DueAt:        o.DueAt,
				Status:       status,
				PaidAt:       o.FirstSettledAt,
				InvoiceReady: o.invoiceReady(),
			})
		}
	}
	return summary, nil
}

// invoiceReady reports whether the obligation has a finalized Stripe invoice with a hosted page.
func (o Obligation) invoiceReady() bool {
	return o.StripeInvoiceID != nil && !slices.Contains([]string{"", "draft", "deleted"}, deref(o.LatestStripeStatus))
}

// ReturnStatus maps a Checkout session to a customer-safe outcome. It exposes only status and
// amount, never student identity, contact, or payment history.
func (s *Service) ReturnStatus(ctx context.Context, sessionID string) (ReturnStatus, error) {
	if sessionID == "" {
		return ReturnStatus{}, ErrUnavailable
	}
	var attempt CheckoutAttempt
	found, err := firstRow(s.db.WithContext(ctx), &attempt, "stripe_session_id = ?", sessionID)
	if err != nil {
		return ReturnStatus{}, err
	}
	if !found {
		return ReturnStatus{}, ErrUnavailable
	}
	status := "pending"
	switch attempt.State {
	case "paid":
		status = "paid"
	case "expired", "failed":
		status = "not_completed"
	}
	return ReturnStatus{Status: status, AmountCents: attempt.FirstAmountCents}, nil
}
