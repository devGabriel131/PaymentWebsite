//go:build integration

package customer

import (
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"paymentwebsite/internal/billing"
	"paymentwebsite/internal/links"
	"paymentwebsite/internal/logging"
	"paymentwebsite/internal/students"
)

func TestPaymentReturnRoute(t *testing.T) {
	db := customerDatabase(t)
	store := links.New(db, nil)
	biller := testCustomerBiller(t, db)

	mux := http.NewServeMux()
	if err := RegisterRoutes(mux, store, nil, biller); err != nil {
		t.Fatal(err)
	}
	handler := logging.Requests(logging.New(io.Discard), mux)

	// 1. Missing query params returns 404
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/payments/return", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for empty query, got %d", rec.Code)
	}

	// 2. Cancelled return returns 200 with "No completado"
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/payments/return?cancelled=1", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for cancelled=1, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No completado") {
		t.Fatalf("expected 'No completado' in cancelled body: %s", body)
	}

	// 3. Unknown session_id returns 404
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/payments/return?session_id=cs_unknown_999", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown session_id, got %d", rec.Code)
	}

	// 4. Known session_id returns 200 with verified status and amount
	tokenHash := make([]byte, 32)
	rand.Read(tokenHash)
	now := time.Now().UTC()
	link := links.Link{
		TokenHash: tokenHash,
		Details: students.Details{
			Name:  "Return Student",
			Email: "return@example.com",
		},
		CreatedAt:        now,
		SubmittedAt:      &now,
		TermsAcceptedAt:  &now,
		ProgramStartDate: "2026-11-01",
		ExpiresAt:        now.Add(7 * 24 * time.Hour),
	}
	if err := db.Create(&link).Error; err != nil {
		t.Fatalf("create link: %v", err)
	}
	student := students.Student{
		SourceLinkHash: tokenHash,
		Details:        link.Details,
		IsActive:       true,
	}
	if err := db.Create(&student).Error; err != nil {
		t.Fatalf("create student: %v", err)
	}
	contract := billing.Contract{
		StudentID:        student.ID,
		Reference:        rand.Text(),
		PlanID:           "full",
		PlanName:         "Pago Único",
		InstallmentCount: 1,
		Currency:         "usd",
		TotalCents:       60000,
	}
	if err := db.Create(&contract).Error; err != nil {
		t.Fatalf("create contract: %v", err)
	}
	sessID := "cs_return_test_123"
	attempt := billing.CheckoutAttempt{
		ContractID:            contract.ID,
		Reference:             rand.Text(),
		PlanID:                "full",
		FirstAmountCents:      60000,
		StripePriceID:         "pr_full",
		IntegrationIdentifier: "tuition_ret123",
		CreationStartedAt:     now,
		StripeSessionID:       &sessID,
		State:                 "paid",
	}
	if err := db.Create(&attempt).Error; err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/payments/return?session_id="+sessID, nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid session_id, got %d: %s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	if !strings.Contains(body, "Pagado") || !strings.Contains(body, "$600.00") {
		t.Fatalf("expected status 'Pagado' and amount '$600.00' in body: %s", body)
	}
	// Never leaks student identity or contact
	if strings.Contains(body, "Return Student") || strings.Contains(body, "return@example.com") {
		t.Fatalf("return page leaked student details: %s", body)
	}
}
