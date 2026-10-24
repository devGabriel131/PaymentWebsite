//go:build integration

package billing_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"paymentwebsite/internal/billing"
	"paymentwebsite/internal/billing/billingtest"
	"paymentwebsite/internal/students"
	"paymentwebsite/internal/testdb"
)

type workerMockServer struct {
	mux           *http.ServeMux
	initialAmount int64 // fixed at construction

	mu                    sync.Mutex
	paidAtUnix            int64
	invoicesCreated       int64
	invoiceItemsCreated   int64
	refundedCents         int64
	earlyWarningsReturned []map[string]any
	invoicesByID          map[string]map[string]any
}

func newWorkerMockServer(paidAt time.Time, initialAmount int64) *workerMockServer {
	wms := &workerMockServer{
		paidAtUnix:    paidAt.Unix(),
		initialAmount: initialAmount,
		invoicesByID:  make(map[string]map[string]any),
	}

	mux := http.NewServeMux()

	// 1. GET /v1/checkout/sessions/{id}
	mux.HandleFunc("GET /v1/checkout/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		resp := map[string]any{
			"id":             id,
			"object":         "checkout.session",
			"currency":       "usd",
			"mode":           "payment",
			"payment_status": "paid",
			"status":         "complete",
			"amount_total":   wms.initialAmount,
			"customer":       "cus_worker_test",
			"invoice": map[string]any{
				"id": "in_initial_paid",
			},
			"payment_intent": map[string]any{
				"id":     "pi_worker_test",
				"status": "succeeded",
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	// 2. GET /v1/invoices/{id}
	mux.HandleFunc("GET /v1/invoices/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		wms.mu.Lock()
		inv, exists := wms.invoicesByID[id]
		wms.mu.Unlock()

		if exists {
			_ = json.NewEncoder(w).Encode(inv)
			return
		}

		if id == "in_initial_paid" {
			resp := map[string]any{
				"id":          "in_initial_paid",
				"object":      "invoice",
				"status":      "paid",
				"currency":    "usd",
				"amount_paid": wms.initialAmount,
				"customer":    "cus_worker_test",
				"status_transitions": map[string]any{
					"paid_at": wms.paidAtUnix,
				},
				"lines": map[string]any{
					"object": "list",
					"data": []map[string]any{
						{
							"amount":   wms.initialAmount,
							"currency": "usd",
							"metadata": map[string]any{
								"tuition_obligation": "ref_first_obl",
							},
						},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		http.Error(w, `{"error":{"message":"invoice not found"}}`, http.StatusNotFound)
	})

	// 3. GET /v1/invoice_payments
	mux.HandleFunc("GET /v1/invoice_payments", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		invID := r.FormValue("invoice")
		wms.mu.Lock()
		inv, exists := wms.invoicesByID[invID]
		wms.mu.Unlock()
		if !exists && invID != "in_initial_paid" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "has_more": false, "data": []any{}})
			return
		}
		if exists && inv["status"] != "paid" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "has_more": false, "data": []any{}})
			return
		}
		piID := "pi_worker_test"
		if invID != "in_initial_paid" && invID != "" {
			piID = "pi_" + invID
		}
		resp := map[string]any{
			"object":   "list",
			"has_more": false,
			"data": []map[string]any{
				{
					"id":          "invp_" + invID,
					"object":      "invoice_payment",
					"status":      "paid",
					"amount_paid": wms.initialAmount,
					"payment": map[string]any{
						"type": "payment_intent",
						"payment_intent": map[string]any{
							"id": piID,
						},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	// 4. GET /v1/payment_intents/{id}
	mux.HandleFunc("GET /v1/payment_intents/{id}", func(w http.ResponseWriter, r *http.Request) {
		chID := "ch_mock_1"
		if r.PathValue("id") != "pi_worker_test" {
			chID = "ch_" + r.PathValue("id")
		}
		wms.mu.Lock()
		refunded := wms.refundedCents
		wms.mu.Unlock()
		resp := map[string]any{
			"id":              r.PathValue("id"),
			"object":          "payment_intent",
			"status":          "succeeded",
			"amount_received": wms.initialAmount,
			"currency":        "usd",
			"payment_method": map[string]any{
				"id":       "pm_card_test",
				"customer": "cus_worker_test",
			},
			"latest_charge": map[string]any{
				"id":              chID,
				"amount_refunded": refunded,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	// 5. GET /v1/customers/{id}
	mux.HandleFunc("GET /v1/customers/{id}", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"id":       r.PathValue("id"),
			"object":   "customer",
			"livemode": false,
			"invoice_settings": map[string]any{
				"default_payment_method": map[string]any{
					"id": "pm_card_test",
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	// 6. POST /v1/customers/{id}
	mux.HandleFunc("POST /v1/customers/{id}", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"id":       r.PathValue("id"),
			"object":   "customer",
			"livemode": false,
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	// 6.5 GET /v1/payment_methods/{id}
	mux.HandleFunc("GET /v1/payment_methods/{id}", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"id":       r.PathValue("id"),
			"object":   "payment_method",
			"type":     "card",
			"customer": "cus_worker_test",
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	// 7. POST /v1/invoices (Stage 1 creation)
	mux.HandleFunc("POST /v1/invoices", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&wms.invoicesCreated, 1)
		invID := fmt.Sprintf("in_sched_%d", atomic.LoadInt64(&wms.invoicesCreated))

		inv := map[string]any{
			"id":           invID,
			"object":       "invoice",
			"status":       "draft",
			"currency":     "usd",
			"customer":     "cus_worker_test",
			"amount_due":   20833,
			"auto_advance": false,
			"lines": map[string]any{
				"object": "list",
				"data":   []map[string]any{},
			},
		}

		wms.mu.Lock()
		wms.invoicesByID[invID] = inv
		wms.mu.Unlock()

		_ = json.NewEncoder(w).Encode(inv)
	})

	// 8. POST /v1/invoiceitems (Stage 2 creation)
	mux.HandleFunc("POST /v1/invoiceitems", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&wms.invoiceItemsCreated, 1)
		_ = r.ParseForm()
		invID := r.FormValue("invoice")
		amount := int64(20833)
		price := r.FormValue("pricing[price]")
		if price == "" {
			price = r.FormValue("price")
		}
		if price == "pr_reinstatement" {
			amount = 2500
		}

		wms.mu.Lock()
		if inv, ok := wms.invoicesByID[invID]; ok {
			inv["lines"] = map[string]any{
				"object": "list",
				"data": []map[string]any{
					{
						"id":       "il_mock",
						"amount":   amount,
						"currency": "usd",
					},
				},
			}
			inv["total"] = amount
			inv["amount_due"] = amount
		}
		wms.mu.Unlock()

		resp := map[string]any{
			"id":      fmt.Sprintf("ii_sched_%d", atomic.LoadInt64(&wms.invoiceItemsCreated)),
			"object":  "invoiceitem",
			"invoice": invID,
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	// 9. POST /v1/invoices/{id} (Stage 3 arming)
	mux.HandleFunc("POST /v1/invoices/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		wms.mu.Lock()
		inv := wms.invoicesByID[id]
		wms.mu.Unlock()

		resp := map[string]any{
			"id":           id,
			"object":       "invoice",
			"status":       "draft",
			"currency":     "usd",
			"auto_advance": true,
			"lines":        inv["lines"],
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	// 10. GET /v1/radar/early_fraud_warnings
	mux.HandleFunc("GET /v1/radar/early_fraud_warnings", func(w http.ResponseWriter, r *http.Request) {
		wms.mu.Lock()
		data := wms.earlyWarningsReturned
		wms.mu.Unlock()
		if data == nil {
			data = []map[string]any{}
		}
		resp := map[string]any{
			"object":   "list",
			"has_more": false,
			"data":     data,
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	// 11. GET /v1/refunds/{id}
	mux.HandleFunc("GET /v1/refunds/{id}", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"id":     r.PathValue("id"),
			"object": "refund",
			"payment_intent": map[string]any{
				"id": "pi_worker_test",
			},
			"charge": map[string]any{
				"id": "ch_mock_1",
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	// 12. GET /v1/charges/{id}
	mux.HandleFunc("GET /v1/charges/{id}", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"id":     r.PathValue("id"),
			"object": "charge",
			"payment_intent": map[string]any{
				"id": "pi_worker_test",
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	// 13. POST /v1/invoices/{id}/finalize
	mux.HandleFunc("POST /v1/invoices/{id}/finalize", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		wms.mu.Lock()
		inv := wms.invoicesByID[id]
		if inv == nil {
			inv = map[string]any{"id": id, "object": "invoice", "currency": "usd"}
		}
		inv["status"] = "open"
		inv["hosted_invoice_url"] = "https://invoice.stripe.com/test_" + id
		wms.invoicesByID[id] = inv
		wms.mu.Unlock()
		_ = json.NewEncoder(w).Encode(inv)
	})

	// 14. POST /v1/invoices/{id}/send
	mux.HandleFunc("POST /v1/invoices/{id}/send", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		wms.mu.Lock()
		inv := wms.invoicesByID[id]
		wms.mu.Unlock()
		_ = json.NewEncoder(w).Encode(inv)
	})

	// 15. POST /v1/invoices/{id}/void
	mux.HandleFunc("POST /v1/invoices/{id}/void", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		wms.mu.Lock()
		inv := wms.invoicesByID[id]
		if inv != nil {
			inv["status"] = "void"
		}
		wms.mu.Unlock()
		_ = json.NewEncoder(w).Encode(inv)
	})

	// 16. DELETE /v1/invoices/{id}
	mux.HandleFunc("DELETE /v1/invoices/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		wms.mu.Lock()
		delete(wms.invoicesByID, id)
		wms.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "object": "invoice", "deleted": true})
	})

	wms.mux = mux
	return wms
}

func TestScheduledInvoiceCreationRecoversEveryBoundary(t *testing.T) {
	paidAt := time.Date(2026, time.November, 15, 4, 0, 0, 0, time.UTC)
	mock := newWorkerMockServer(paidAt, 20834)

	allModels := append([]any{&students.Student{}}, billing.Models()...)
	db := testdb.Open(t, allModels...)

	tokenHash := make([]byte, 32)
	rand.Read(tokenHash)
	now := time.Now().UTC()
	student := students.Student{
		SourceLinkHash: tokenHash,
		Details: students.Details{
			Name:  "Test Student",
			Email: "test@example.com",
		},
		IsActive: true,
	}
	if err := db.Create(&student).Error; err != nil {
		t.Fatalf("create student: %v", err)
	}
	contractRef := rand.Text()
	cusID := "cus_worker_test"
	contract := billing.Contract{
		StudentID:        student.ID,
		Reference:        contractRef,
		PlanID:           "three",
		PlanName:         "Plan de 3 Pagos Mensuales",
		InstallmentCount: 3,
		TotalCents:       62500,
		StripeCustomerID: &cusID,
	}
	db.Create(&contract)

	// Create 3 tuition obligations
	obl1 := billing.Obligation{
		ContractID:            contract.ID,
		Reference:             "ref_first_obl",
		Kind:                  "tuition",
		Number:                1,
		AmountCents:           20834,
		AmountRemainingCents:  20834,
		StripePriceID:         "pr_three_1",
		CollectionDisposition: "collect",
	}
	obl2 := billing.Obligation{
		ContractID:            contract.ID,
		Reference:             "ref_second_obl",
		Kind:                  "tuition",
		Number:                2,
		AmountCents:           20833,
		AmountRemainingCents:  20833,
		StripePriceID:         "pr_three_2",
		CollectionDisposition: "collect",
	}
	obl3 := billing.Obligation{
		ContractID:            contract.ID,
		Reference:             "ref_third_obl",
		Kind:                  "tuition",
		Number:                3,
		AmountCents:           20833,
		AmountRemainingCents:  20833,
		StripePriceID:         "pr_three_2",
		CollectionDisposition: "collect",
	}
	db.Create(&obl1)
	db.Create(&obl2)
	db.Create(&obl3)

	sessID := "cs_worker_sess"
	attempt := billing.CheckoutAttempt{
		ContractID:        contract.ID,
		Reference:         rand.Text(),
		PlanID:            "three",
		FirstAmountCents:  20834,
		StripePriceID:     "pr_three_1",
		CreationStartedAt: now,
		StripeSessionID:   &sessID,
		State:             "open",
	}
	db.Create(&attempt)

	svc := billingtest.New(t, db, mock.mux)

	// Insert checkout.session.completed webhook event into DB
	evt := billing.WebhookEvent{
		ID:         "evt_checkout_completed",
		EventType:  "checkout.session.completed",
		ResourceID: sessID,
	}
	db.Create(&evt)

	// One worker pass: the event funds the contract, then the same pass creates the later invoices
	svc.DrainForTest(t.Context())

	// Verify contract is now funded
	var updatedContract billing.Contract
	db.First(&updatedContract, contract.ID)
	if updatedContract.FundedAt == nil {
		t.Fatal("expected contract.FundedAt to be set")
	}
	if !updatedContract.FundedAt.Equal(paidAt) {
		t.Fatalf("expected FundedAt %v, got %v", paidAt, *updatedContract.FundedAt)
	}

	// Verify attempt is now terminal "paid"
	var updatedAttempt billing.CheckoutAttempt
	db.First(&updatedAttempt, attempt.ID)
	if updatedAttempt.State != "paid" {
		t.Fatalf("expected attempt state 'paid', got %q", updatedAttempt.State)
	}

	// Verify exactly two remaining scheduled invoices were created (for plan "three")
	var remainingObligations []billing.Obligation
	db.Where("contract_id = ? AND number > 1", contract.ID).Order("number ASC").Find(&remainingObligations)
	if len(remainingObligations) != 2 {
		t.Fatalf("expected 2 remaining obligations, got %d", len(remainingObligations))
	}
	for i, o := range remainingObligations {
		if o.StripeInvoiceID == nil {
			t.Errorf("obligation %d has nil StripeInvoiceID", i+2)
		}
		if o.StripeInvoiceItemID == nil {
			t.Errorf("obligation %d has nil StripeInvoiceItemID", i+2)
		}
		if o.ScheduledFinalizationAt == nil {
			t.Errorf("obligation %d has nil ScheduledFinalizationAt", i+2)
		}
		if o.DueAt == nil {
			t.Errorf("obligation %d has nil DueAt", i+2)
		}
	}

	// Verify atomic idempotent recovery: re-running does not create duplicate invoices
	invoicesBefore := atomic.LoadInt64(&mock.invoicesCreated)
	db.Model(&contract).Update("next_sync_at", nil)
	svc.DrainForTest(t.Context())

	invoicesAfter := atomic.LoadInt64(&mock.invoicesCreated)
	if invoicesAfter != invoicesBefore {
		t.Fatalf("re-run created duplicate invoices: before=%d, after=%d", invoicesBefore, invoicesAfter)
	}
}

func TestRefundsAndFraudSignalsSurfaceWithoutWebhook(t *testing.T) {
	paidAt := time.Date(2026, time.November, 15, 4, 0, 0, 0, time.UTC)
	mock := newWorkerMockServer(paidAt, 60000)
	allModels := append([]any{&students.Student{}}, billing.Models()...)
	db := testdb.Open(t, allModels...)

	tokenHash := make([]byte, 32)
	rand.Read(tokenHash)
	now := time.Now().UTC()
	student := students.Student{
		SourceLinkHash: tokenHash,
		Details: students.Details{
			Name:  "Financial Student",
			Email: "fin@example.com",
		},
		IsActive: true,
	}
	if err := db.Create(&student).Error; err != nil {
		t.Fatalf("create student: %v", err)
	}
	cusID := "cus_worker_test"
	contract := billing.Contract{
		StudentID:        student.ID,
		Reference:        rand.Text(),
		PlanID:           "full",
		PlanName:         "Pago Único",
		InstallmentCount: 1,
		TotalCents:       60000,
		FundedAt:         &now,
		StripeCustomerID: &cusID,
	}
	db.Create(&contract)

	chID := "ch_mock_1"
	payment := billing.Payment{
		ID:                 "pi_worker_test",
		ContractID:         contract.ID,
		StripeChargeID:     &chID,
		GrossReceivedCents: 60000,
		GrossRefundedCents: 0,
		PaymentTime:        now,
	}
	db.Create(&payment)

	invID := "in_initial_paid"
	obligation := billing.Obligation{
		ContractID:            contract.ID,
		Reference:             "ref_full_obl",
		Kind:                  "tuition",
		Number:                1,
		AmountCents:           60000,
		AmountPaidCents:       60000,
		AmountRemainingCents:  0,
		StripeInvoiceID:       &invID,
		FirstSettledAt:        &now,
		CollectionDisposition: "collect",
	}
	db.Create(&obligation)

	svc := billingtest.New(t, db, mock.mux)

	// Verify initial summary: 60000 paid, 0 remaining
	summary, err := svc.ForStudent(context.Background(), student.ID)
	if err != nil || summary == nil {
		t.Fatalf("ForStudent: %v", err)
	}
	if summary.CashPaidCents != 60000 || summary.RemainingTuitionCents != 0 {
		t.Fatalf("unexpected summary before adjustment: %+v", summary)
	}

	// stored reads back the payment and the contract's attention reason
	stored := func() (p billing.Payment, attention string) {
		db.First(&p, "id = ?", payment.ID)
		var c billing.Contract
		db.First(&c, contract.ID)
		if c.AttentionReason != nil {
			attention = *c.AttentionReason
		}
		return p, attention
	}

	// 1. Stripe reports a partial refund; the refund.updated event makes the worker re-read the payment
	mock.mu.Lock()
	mock.refundedCents = 10000
	mock.mu.Unlock()
	db.Create(&billing.WebhookEvent{
		ID:         "evt_refund_1",
		EventType:  "refund.updated",
		ResourceID: "re_test_1",
	})
	svc.DrainForTest(t.Context())

	p, attention := stored()
	if p.GrossRefundedCents != 10000 {
		t.Fatalf("expected GrossRefundedCents 10000 after refund, got %d", p.GrossRefundedCents)
	}
	if attention != "payment_review" {
		t.Fatalf("expected attention reason payment_review after refund, got %q", attention)
	}
	// CashPaid is now 50000 (gross received - refunded)
	summary, err = svc.ForStudent(t.Context(), student.ID)
	if err != nil || summary == nil {
		t.Fatalf("ForStudent: %v", err)
	}
	if summary.CashPaidCents != 50000 {
		t.Fatalf("expected CashPaidCents 50000 after refund, got %d", summary.CashPaidCents)
	}

	// 2. Daily audit discovers early fraud warning without webhook
	mock.mu.Lock()
	mock.earlyWarningsReturned = []map[string]any{
		{
			"id":     "issfr_mock_1",
			"object": "radar.early_fraud_warning",
			"charge": "ch_mock_1",
			"payment_intent": map[string]any{
				"id": "pi_worker_test",
			},
		},
	}
	mock.mu.Unlock()
	db.Model(&contract).Update("next_sync_at", nil)
	svc.DrainForTest(t.Context())

	p, attention = stored()
	if p.DisputeOrFraudReviewState == nil || *p.DisputeOrFraudReviewState != "fraud_warning" {
		t.Fatal("expected DisputeOrFraudReviewState fraud_warning to be recorded by audit")
	}
	if attention != "payment_review" {
		t.Fatalf("expected attention reason to stay payment_review, got %q", attention)
	}
}

func TestSuspensionAndReinstatement(t *testing.T) {
	paidAt := time.Date(2026, time.November, 15, 4, 0, 0, 0, time.UTC)
	mock := newWorkerMockServer(paidAt, 20834)

	allModels := append([]any{&students.Student{}}, billing.Models()...)
	db := testdb.Open(t, allModels...)

	svc := billingtest.New(t, db, mock.mux)

	now := time.Now().UTC()

	// Helper to create student, contract
	setupStudent := func(name, email string) (students.Student, billing.Contract) {
		tokenHash := make([]byte, 32)
		rand.Read(tokenHash)
		student := students.Student{
			SourceLinkHash: tokenHash,
			Details: students.Details{
				Name:  name,
				Email: email,
			},
			IsActive: true,
		}
		if err := db.Create(&student).Error; err != nil {
			t.Fatal(err)
		}
		cusID := "cus_worker_" + rand.Text()[:8]
		contract := billing.Contract{
			StudentID:        student.ID,
			Reference:        rand.Text(),
			PlanID:           "three",
			PlanName:         "Plan de 3 Pagos Mensuales",
			InstallmentCount: 3,
			TotalCents:       60000,
			FundedAt:         &now,
			StripeCustomerID: &cusID,
		}
		if err := db.Create(&contract).Error; err != nil {
			t.Fatal(err)
		}
		return student, contract
	}

	registerInvoice := func(id string, amount int64, status string, cusID string) {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		paid := int64(0)
		remaining := amount
		if status == "paid" {
			paid = amount
			remaining = 0
		}
		mock.invoicesByID[id] = map[string]any{
			"id":               id,
			"object":           "invoice",
			"status":           status,
			"currency":         "usd",
			"total":            amount,
			"amount_due":       amount,
			"amount_paid":      paid,
			"amount_remaining": remaining,
			"customer":         cusID,
			"lines": map[string]any{
				"object": "list",
				"data": []map[string]any{
					{
						"id":       "il_" + id,
						"amount":   amount,
						"currency": "usd",
					},
				},
			},
		}
	}

	// 1. Boundary: unpaid obligation 13 days 23 hours past due -> NOT suspended
	student1, contract1 := setupStudent("Near Boundary", "near@example.com")
	dueNear := now.Add(-13*24*time.Hour - 23*time.Hour)
	invID1 := "in_boundary_1"
	registerInvoice(invID1, 20833, "open", *contract1.StripeCustomerID)
	obNear := billing.Obligation{
		ContractID:            contract1.ID,
		Reference:             rand.Text(),
		Kind:                  "tuition",
		Number:                2,
		AmountCents:           20833,
		AmountRemainingCents:  20833,
		StripePriceID:         "pr_three_2",
		StripeInvoiceID:       &invID1,
		DueAt:                 &dueNear,
		CollectionDisposition: "collect",
	}
	db.Create(&obNear)

	// Run worker sweep for contract 1
	svc.DrainForTest(t.Context())

	var checkStudent1 students.Student
	db.First(&checkStudent1, student1.ID)
	if !checkStudent1.IsActive {
		t.Fatal("expected student to remain active when 14-day deadline has not passed")
	}
	var suspCount1 int64
	db.Model(&billing.Suspension{}).Where("contract_id = ?", contract1.ID).Count(&suspCount1)
	if suspCount1 != 0 {
		t.Fatalf("expected 0 suspensions, got %d", suspCount1)
	}

	// 2. Exact 14-day boundary: unpaid obligation 14 days 1 hour past due -> SUSPENDED
	student2, contract2 := setupStudent("Past Boundary", "past@example.com")
	duePast := now.Add(-14*24*time.Hour - time.Hour)
	invID2 := "in_boundary_2"
	registerInvoice(invID2, 20833, "open", *contract2.StripeCustomerID)
	obPast := billing.Obligation{
		ContractID:            contract2.ID,
		Reference:             rand.Text(),
		Kind:                  "tuition",
		Number:                2,
		AmountCents:           20833,
		AmountRemainingCents:  20833,
		StripePriceID:         "pr_three_2",
		StripeInvoiceID:       &invID2,
		DueAt:                 &duePast,
		CollectionDisposition: "collect",
	}
	db.Create(&obPast)

	// Also add installment 3 (future invoice) to verify future invoices remain scheduled/armed
	dueFuture := now.Add(15 * 24 * time.Hour)
	invID3 := "in_future_3"
	registerInvoice(invID3, 20833, "draft", *contract2.StripeCustomerID)
	obFuture := billing.Obligation{
		ContractID:            contract2.ID,
		Kind:                  "tuition",
		Number:                3,
		AmountCents:           20833,
		AmountRemainingCents:  20833,
		StripePriceID:         "pr_three_2",
		StripeInvoiceID:       &invID3,
		DueAt:                 &dueFuture,
		CollectionDisposition: "collect",
	}
	db.Create(&obFuture)

	svc.DrainForTest(t.Context())

	var checkStudent2 students.Student
	db.First(&checkStudent2, student2.ID)
	if checkStudent2.IsActive {
		t.Fatal("expected student to be suspended (is_active = false) after 14-day deadline passed")
	}
	var openSusp billing.Suspension
	if err := db.Where("contract_id = ? AND closed_at IS NULL", contract2.ID).First(&openSusp).Error; err != nil {
		t.Fatalf("expected open suspension for contract 2: %v", err)
	}
	if openSusp.TriggeringTuitionInstallmentNumber != 2 {
		t.Fatalf("expected triggering installment 2, got %d", openSusp.TriggeringTuitionInstallmentNumber)
	}

	// Verify obligation has DeadlineEpisodeID attributed
	var updatedObPast billing.Obligation
	db.First(&updatedObPast, obPast.ID)
	if updatedObPast.DeadlineEpisodeID == nil || *updatedObPast.DeadlineEpisodeID != openSusp.ID {
		t.Fatalf("expected DeadlineEpisodeID to match suspension ID %d", openSusp.ID)
	}

	// 3. No fee before staff request
	var feeCount int64
	db.Model(&billing.Obligation{}).Where("contract_id = ? AND kind = 'reinstatement'", contract2.ID).Count(&feeCount)
	if feeCount != 0 {
		t.Fatalf("expected 0 fee obligations before staff request, got %d", feeCount)
	}

	// 4. Request reinstatement by staff
	summary, err := svc.RequestReinstatement(context.Background(), student2.ID, "director_carmen")
	if err != nil {
		t.Fatalf("RequestReinstatement failed: %v", err)
	}
	if summary == nil {
		t.Fatal("expected non-nil summary after RequestReinstatement")
	}

	// Verify fee obligation was created and invoiced
	var feeOb billing.Obligation
	if err := db.Where("contract_id = ? AND kind = 'reinstatement'", contract2.ID).First(&feeOb).Error; err != nil {
		t.Fatalf("expected fee obligation created: %v", err)
	}
	if feeOb.AmountCents != 2500 || feeOb.AmountRemainingCents != 2500 {
		t.Fatalf("expected fee amount 2500 cents, got %d", feeOb.AmountCents)
	}
	if feeOb.StripeInvoiceID == nil {
		t.Fatal("expected fee obligation to have StripeInvoiceID")
	}

	// Repeated request reuses the episode and single fee obligation (idempotent)
	summary2, err := svc.RequestReinstatement(context.Background(), student2.ID, "director_carmen")
	if err != nil {
		t.Fatalf("repeated RequestReinstatement failed: %v", err)
	}
	if summary2 == nil {
		t.Fatal("expected summary from repeated request")
	}
	db.Model(&billing.Obligation{}).Where("contract_id = ? AND kind = 'reinstatement'", contract2.ID).Count(&feeCount)
	if feeCount != 1 {
		t.Fatalf("expected exactly 1 fee obligation after repeated request, got %d", feeCount)
	}

	// 5. Fee paid alone cannot reactivate if overdue tuition remains unpaid
	mock.mu.Lock()
	if inv := mock.invoicesByID[*feeOb.StripeInvoiceID]; inv != nil {
		inv["status"] = "paid"
		inv["amount_paid"] = int64(2500)
		inv["amount_remaining"] = int64(0)
		inv["status_transitions"] = map[string]any{"paid_at": now.Unix()}
	}
	mock.mu.Unlock()
	db.Model(&contract2).Update("next_sync_at", nil)
	svc.DrainForTest(t.Context())

	db.First(&checkStudent2, student2.ID)
	if checkStudent2.IsActive {
		t.Fatal("student should NOT reactivate when overdue tuition is still unpaid, even if fee is paid")
	}

	// 6. Full required settlement (overdue tuition + fee) auto-reactivates student
	mock.mu.Lock()
	if inv := mock.invoicesByID[invID2]; inv != nil {
		inv["status"] = "paid"
		inv["amount_paid"] = int64(20833)
		inv["amount_remaining"] = int64(0)
		inv["status_transitions"] = map[string]any{"paid_at": now.Unix()}
	}
	mock.mu.Unlock()
	db.Model(&contract2).Update("next_sync_at", nil)
	svc.DrainForTest(t.Context())

	db.First(&checkStudent2, student2.ID)
	if !checkStudent2.IsActive {
		t.Fatal("expected student to be reactivated (is_active = true) after all overdue tuition and fee are paid")
	}
	var closedSusp billing.Suspension
	db.First(&closedSusp, openSusp.ID)
	if closedSusp.ClosedAt == nil || closedSusp.CloseReason == nil || *closedSusp.CloseReason != "reinstated" {
		t.Fatalf("expected suspension closed with reason 'reinstated', got %+v", closedSusp)
	}

	// Verify future obligation remains scheduled
	var checkFuture billing.Obligation
	db.First(&checkFuture, obFuture.ID)
	if checkFuture.AmountRemainingCents != 20833 {
		t.Fatal("future installment should remain scheduled and owing")
	}

	// 7. On-time payment with late webhook does not penalize
	student3, contract3 := setupStudent("On-time late webhook", "ontime@example.com")
	dueOld := now.Add(-30 * 24 * time.Hour)
	onTimeSettle := dueOld.Add(5 * 24 * time.Hour) // settled 5 days after due, well before 14-day deadline
	invIDOnTime := "in_ontime"
	registerInvoice(invIDOnTime, 20833, "paid", *contract3.StripeCustomerID)
	obOnTime := billing.Obligation{
		ContractID:            contract3.ID,
		Kind:                  "tuition",
		Number:                2,
		AmountCents:           20833,
		AmountPaidCents:       20833,
		AmountRemainingCents:  0,
		FirstSettledAt:        &onTimeSettle,
		StripePriceID:         "pr_three_2",
		StripeInvoiceID:       &invIDOnTime,
		DueAt:                 &dueOld,
		CollectionDisposition: "collect",
	}
	db.Create(&obOnTime)

	svc.DrainForTest(t.Context())

	var checkStudent3 students.Student
	db.First(&checkStudent3, student3.ID)
	if !checkStudent3.IsActive {
		t.Fatal("on-time payment settled before 14-day deadline should NOT suspend student")
	}
}

func TestWorkerSyncsCustomerContact(t *testing.T) {
	mock := newMockStripeServer()
	db, svc, tokenHash, studentID := setupTestEnvironment(t, mock)
	ctx := context.Background()

	// Start checkout creates the customer in Stripe and stamps synced columns
	res, err := svc.StartCheckout(ctx, tokenHash, "three")
	if err != nil {
		t.Fatalf("StartCheckout: %v", err)
	}
	if res.URL == "" {
		t.Fatalf("expected checkout URL, got %+v", res)
	}

	var c billing.Contract
	if err := db.Where("student_id = ?", studentID).Take(&c).Error; err != nil {
		t.Fatalf("take contract: %v", err)
	}
	if c.StripeCustomerID == nil || *c.StripeCustomerID == "" {
		t.Fatal("expected StripeCustomerID to be set on contract")
	}
	if c.SyncedCustomerName != "Maria Santos" || c.SyncedCustomerEmail != "maria@example.com" {
		t.Fatalf("initial stamps wrong: %s, %s", c.SyncedCustomerName, c.SyncedCustomerEmail)
	}

	// 1. Unchanged student => drain produces 0 customer updates
	svc.TouchContractForStudent(ctx, studentID)
	svc.DrainForTest(ctx)
	mock.with(func() {
		if len(mock.customerUpdates) != 0 {
			t.Fatalf("expected 0 customer updates, got %d", len(mock.customerUpdates))
		}
	})

	// 2. Staff changes student email through students.Store.UpdateField
	// Contract is unfunded, but customer syncs durable to Stripe
	roster := students.New(db, nil)
	if _, err := roster.UpdateField(ctx, studentID, "email", "maria.updated@example.com"); err != nil {
		t.Fatalf("UpdateField: %v", err)
	}
	svc.TouchContractForStudent(ctx, studentID)

	// One worker drain records exactly one customer update with new email
	svc.DrainForTest(ctx)
	var updates []customerUpdate
	mock.with(func() {
		updates = append([]customerUpdate(nil), mock.customerUpdates...)
	})
	if len(updates) != 1 {
		t.Fatalf("expected 1 customer update, got %d", len(updates))
	}
	wantUpdate := customerUpdate{ID: *c.StripeCustomerID, Name: "Maria Santos", Email: "maria.updated@example.com"}
	if updates[0] != wantUpdate {
		t.Fatalf("customer update = %+v, want %+v", updates[0], wantUpdate)
	}

	// DB contract stamp equals student
	if err := db.Where("id = ?", c.ID).Take(&c).Error; err != nil {
		t.Fatal(err)
	}
	if c.SyncedCustomerEmail != "maria.updated@example.com" {
		t.Fatalf("stamp not updated in DB: %s", c.SyncedCustomerEmail)
	}

	// 3. Mock fails update => contract stamp stays old and next drain succeeds
	if _, err := roster.UpdateField(ctx, studentID, "name", "Maria Rivera"); err != nil {
		t.Fatalf("UpdateField name: %v", err)
	}
	svc.TouchContractForStudent(ctx, studentID)
	mock.with(func() {
		mock.customerUpdateError = true
	})
	svc.DrainForTest(ctx)

	// Stamp in DB still has old name because update failed
	if err := db.Where("id = ?", c.ID).Take(&c).Error; err != nil {
		t.Fatal(err)
	}
	if c.SyncedCustomerName != "Maria Santos" {
		t.Fatalf("stamp should not be updated on failure: %s", c.SyncedCustomerName)
	}

	// Reset mock error and bump next_sync_at to now for next pass
	mock.with(func() {
		mock.customerUpdateError = false
	})
	svc.TouchContractForStudent(ctx, studentID)
	svc.DrainForTest(ctx)

	// Next drain succeeded
	mock.with(func() {
		updates = append([]customerUpdate(nil), mock.customerUpdates...)
	})
	if len(updates) != 2 {
		t.Fatalf("expected 2 total customer updates, got %d", len(updates))
	}
	wantSecond := customerUpdate{ID: *c.StripeCustomerID, Name: "Maria Rivera", Email: "maria.updated@example.com"}
	if updates[1] != wantSecond {
		t.Fatalf("second update = %+v, want %+v", updates[1], wantSecond)
	}
	if err := db.Where("id = ?", c.ID).Take(&c).Error; err != nil {
		t.Fatal(err)
	}
	if c.SyncedCustomerName != "Maria Rivera" {
		t.Fatalf("stamp not updated after retry: %s", c.SyncedCustomerName)
	}
}
