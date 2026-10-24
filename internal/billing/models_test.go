//go:build integration

package billing_test

import (
	"crypto/rand"
	"testing"
	"time"

	"paymentwebsite/internal/billing"
	"paymentwebsite/internal/students"
	"paymentwebsite/internal/testdb"
)

func TestBillingModelsMigration(t *testing.T) {
	allModels := append([]any{&students.Student{}}, billing.Models()...)
	db := testdb.Open(t, allModels...)

	// Verify we can insert a student and contract
	student := students.Student{
		SourceLinkHash: make([]byte, 32),
		Details: students.Details{
			Name:         "Juan del Pueblo",
			Email:        "juan@example.com",
			Phone:        "+17875551234",
			Municipality: "San Juan",
		},
		IsActive: true,
	}
	for i := range student.SourceLinkHash {
		student.SourceLinkHash[i] = byte(i + 1)
	}
	if err := db.Create(&student).Error; err != nil {
		t.Fatalf("create student: %v", err)
	}

	contractRef := rand.Text()
	contract := billing.Contract{
		StudentID:        student.ID,
		Reference:        contractRef,
		PlanID:           "three",
		PlanName:         "Plan de 3 Pagos Mensuales",
		InstallmentCount: 3,
		TotalCents:       62500,
	}
	if err := db.Create(&contract).Error; err != nil {
		t.Fatalf("create contract: %v", err)
	}

	// Verify obligation creation
	obligation := billing.Obligation{
		ContractID:    contract.ID,
		Reference:     rand.Text(),
		Kind:          "tuition",
		Number:        1,
		AmountCents:   20834,
		StripePriceID: "price_test_three_first",
	}
	if err := db.Create(&obligation).Error; err != nil {
		t.Fatalf("create obligation: %v", err)
	}

	// Verify checkout attempt creation with nonterminal state
	attempt := billing.CheckoutAttempt{
		ContractID:        contract.ID,
		Reference:         rand.Text(),
		PlanID:            "three",
		FirstAmountCents:  20834,
		StripePriceID:     "price_test_three_first",
		CreationStartedAt: time.Now().UTC(),
		State:             "creating",
	}
	if err := db.Create(&attempt).Error; err != nil {
		t.Fatalf("create checkout attempt: %v", err)
	}

	// Verify partial unique index prevents second active attempt for same contract
	competingAttempt := billing.CheckoutAttempt{
		ContractID:        contract.ID,
		Reference:         rand.Text(),
		PlanID:            "three",
		FirstAmountCents:  20834,
		StripePriceID:     "price_test_three_first",
		CreationStartedAt: time.Now().UTC(),
		State:             "open",
	}
	if err := db.Create(&competingAttempt).Error; err == nil {
		t.Fatalf("expected error creating second nonterminal attempt on same contract, got nil")
	}

	// Verify suspension partial index
	suspension := billing.Suspension{
		ContractID:                         contract.ID,
		TriggeringTuitionInstallmentNumber: 1,
		StartedAt:                          time.Now().UTC(),
	}
	if err := db.Create(&suspension).Error; err != nil {
		t.Fatalf("create suspension: %v", err)
	}
	secondActiveSuspension := billing.Suspension{
		ContractID:                         contract.ID,
		TriggeringTuitionInstallmentNumber: 2,
		StartedAt:                          time.Now().UTC(),
	}
	if err := db.Create(&secondActiveSuspension).Error; err == nil {
		t.Fatalf("expected error creating second open suspension on same contract, got nil")
	}
}
