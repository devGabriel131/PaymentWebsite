//go:build integration

package billing_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	"paymentwebsite/internal/billing"
	"paymentwebsite/internal/links"
	"paymentwebsite/internal/students"
)

func TestFundedTimes(t *testing.T) {
	mock := newMockStripeServer()
	defer mock.Close()
	db, svc, fundedHash, fundedID := setupTestEnvironment(t, mock)
	ctx := context.Background()

	empty, err := svc.FundedTimes(ctx, nil)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty input = %v, %v; want empty map, nil", empty, err)
	}

	unfundedHash := make([]byte, 32)
	rand.Read(unfundedHash)
	unfundedLink := links.Link{
		TokenHash: unfundedHash,
		Details:   students.Details{Name: "Pedro Rivera", Email: "pedro@example.com", Phone: "+17875550000", Municipality: "Ponce"},
		CreatedAt: time.Now().UTC(),
		ExpiresAt: time.Now().UTC().Add(7 * 24 * time.Hour),
	}
	if err := db.Create(&unfundedLink).Error; err != nil {
		t.Fatalf("create link: %v", err)
	}
	unfunded := students.Student{
		SourceLinkHash: unfundedHash,
		Details:        unfundedLink.Details,
		IsActive:       true,
	}
	if err := db.Create(&unfunded).Error; err != nil {
		t.Fatalf("create student: %v", err)
	}
	fundedAt := time.Date(2026, 10, 1, 15, 4, 5, 0, time.UTC)
	for _, c := range []billing.Contract{
		{StudentID: fundedID, FundedAt: &fundedAt},
		{StudentID: unfunded.ID},
	} {
		c.Reference, c.PlanID, c.PlanName = rand.Text(), "three", "Plan de 3 Pagos Mensuales"
		c.InstallmentCount, c.Currency, c.TotalCents = 3, "usd", 62500
		if err := db.Create(&c).Error; err != nil {
			t.Fatalf("create contract: %v", err)
		}
	}

	unknownHash := make([]byte, 32)
	rand.Read(unknownHash)
	got, err := svc.FundedTimes(ctx, [][]byte{fundedHash, unfundedHash, unknownHash})
	if err != nil {
		t.Fatalf("FundedTimes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FundedTimes = %v; want only the funded link", got)
	}
	if at, ok := got[hex.EncodeToString(fundedHash)]; !ok || !at.Equal(fundedAt) {
		t.Fatalf("funded time = %v, %v; want %v", at, ok, fundedAt)
	}
}

func TestUpdateCustomerContactSurvivesCancelledRequest(t *testing.T) {
	mock := newMockStripeServer()
	defer mock.Close()
	db, svc, tokenHash, studentID := setupTestEnvironment(t, mock)

	cusID := "cus_edit_test"
	contract := billing.Contract{
		StudentID:        studentID,
		Reference:        rand.Text(),
		PlanID:           "three",
		PlanName:         "Plan de 3 Pagos Mensuales",
		InstallmentCount: 3,
		Currency:         "usd",
		TotalCents:       62500,
		StripeCustomerID: &cusID,
	}
	if err := db.Create(&contract).Error; err != nil {
		t.Fatalf("create contract: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := svc.UpdateCustomerContact(ctx, tokenHash, "Ana Rivera", "ana@example.com")
	if err != nil {
		t.Fatalf("UpdateCustomerContact: %v", err)
	}

	var updates []customerUpdate
	mock.with(func() {
		updates = append(updates, mock.customerUpdates...)
	})
	want := customerUpdate{ID: "cus_edit_test", Name: "Ana Rivera", Email: "ana@example.com"}
	if len(updates) != 1 || updates[0] != want {
		t.Fatalf("customerUpdates = %v; want [%v]", updates, want)
	}
}
