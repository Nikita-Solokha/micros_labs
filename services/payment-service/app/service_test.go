package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Nikita-Solokha/micros_labs/pkg/events"
	"github.com/Nikita-Solokha/micros_labs/pkg/idempotency"
	"github.com/Nikita-Solokha/micros_labs/pkg/webhook"
)

const secret = "test-secret"

// fakeGateway — in-process мок банка для unit-тестов.
type fakeGateway struct {
	failCreate, failRefund bool
	refunds                int
}

func (g *fakeGateway) CreateSession(_ context.Context, id string, _ float64, _ string) (string, error) {
	if g.failCreate {
		return "", errors.New("timeout")
	}
	return "http://bank/pay/" + id, nil
}

func (g *fakeGateway) Refund(_ context.Context, _ string, _ float64) (string, error) {
	if g.failRefund {
		return "", errors.New("timeout")
	}
	g.refunds++
	return "refund-1", nil
}

// recBus запоминает опубликованные события.
type recBus struct{ published []events.Envelope }

func (b *recBus) Publish(_ context.Context, _, _ string, e events.Envelope) error {
	b.published = append(b.published, e)
	return nil
}

func newTestService() (*Service, *fakeGateway, *recBus) {
	gw := &fakeGateway{}
	bus := &recBus{}
	return &Service{
		Store:         NewStore(),
		Gateway:       gw,
		Bus:           bus,
		Processed:     idempotency.NewProcessed(),
		WebhookSecret: secret,
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, gw, bus
}

func paid(t *testing.T, s *Service) Payment {
	t.Helper()
	p, _, err := s.CreatePayment(context.Background(), CreatePaymentRequest{OrderID: "order-1", Amount: 450})
	if err != nil {
		t.Fatal(err)
	}
	err = s.HandleWebhook(context.Background(), WebhookRequest{
		PaymentID: p.ID, Status: "SUCCESS", Signature: webhook.Sign(secret, p.ID, "SUCCESS"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// ---------- unit-тесты бизнес-логики ----------

func TestCreatePayment_Validation(t *testing.T) {
	s, _, _ := newTestService()
	_, _, err := s.CreatePayment(context.Background(), CreatePaymentRequest{OrderID: "o", Amount: -1})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("ожидалась ошибка валидации, получили %v", err)
	}
}

func TestCreatePayment_SameOrderReturnsExisting(t *testing.T) {
	s, _, _ := newTestService()
	p1, created1, _ := s.CreatePayment(context.Background(), CreatePaymentRequest{OrderID: "o", Amount: 100})
	p2, created2, _ := s.CreatePayment(context.Background(), CreatePaymentRequest{OrderID: "o", Amount: 100})
	if !created1 || created2 || p1.ID != p2.ID {
		t.Fatalf("второй вызов должен вернуть тот же платёж")
	}
}

func TestCreatePayment_GatewayDown(t *testing.T) {
	s, gw, _ := newTestService()
	gw.failCreate = true
	_, _, err := s.CreatePayment(context.Background(), CreatePaymentRequest{OrderID: "o", Amount: 100})
	if !errors.Is(err, ErrGatewayUnavailable) {
		t.Fatalf("ожидалась ErrGatewayUnavailable, получили %v", err)
	}
}

func TestWebhook_BadSignature(t *testing.T) {
	s, _, bus := newTestService()
	p, _, _ := s.CreatePayment(context.Background(), CreatePaymentRequest{OrderID: "o", Amount: 100})
	err := s.HandleWebhook(context.Background(), WebhookRequest{PaymentID: p.ID, Status: "SUCCESS", Signature: "fake"})
	if !errors.Is(err, ErrBadSignature) || len(bus.published) != 0 {
		t.Fatalf("поддельный вебхук должен отклоняться без событий")
	}
}

func TestWebhook_SuccessPublishesCompletedOnce(t *testing.T) {
	s, _, bus := newTestService()
	p := paid(t, s)
	// повторный вебхук от банка
	_ = s.HandleWebhook(context.Background(), WebhookRequest{
		PaymentID: p.ID, Status: "SUCCESS", Signature: webhook.Sign(secret, p.ID, "SUCCESS"),
	})
	if len(bus.published) != 1 || bus.published[0].EventType != events.PaymentCompleted {
		t.Fatalf("ожидалось ровно одно событие payment.completed, получили %d", len(bus.published))
	}
	if bus.published[0].CorrelationID != "order-1" {
		t.Fatalf("correlationId должен быть равен orderId")
	}
}

func TestWebhook_FailedPublishesFailed(t *testing.T) {
	s, _, bus := newTestService()
	p, _, _ := s.CreatePayment(context.Background(), CreatePaymentRequest{OrderID: "o", Amount: 100})
	_ = s.HandleWebhook(context.Background(), WebhookRequest{
		PaymentID: p.ID, Status: "FAILED", Signature: webhook.Sign(secret, p.ID, "FAILED"),
	})
	got, _ := s.Store.Get(p.ID)
	if got.Status != StatusFailed || bus.published[0].EventType != events.PaymentFailed {
		t.Fatalf("ожидался статус FAILED и событие payment.failed")
	}
}

func TestRefund_OnlyCompleted(t *testing.T) {
	s, _, _ := newTestService()
	p, _, _ := s.CreatePayment(context.Background(), CreatePaymentRequest{OrderID: "o", Amount: 100})
	if _, err := s.Refund(context.Background(), p.ID, "X"); !errors.Is(err, ErrWrongState) {
		t.Fatalf("неоплаченный платёж нельзя вернуть, получили %v", err)
	}
}

func TestOnSearchFailed_RefundsAndIgnoresDuplicate(t *testing.T) {
	s, gw, bus := newTestService()
	paid(t, s)
	e, _ := events.New(events.CourierSearchFail, "order-1", SearchFailedPayload{OrderID: "order-1", Reason: "TIMEOUT"})

	if err := s.OnSearchFailed(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if err := s.OnSearchFailed(context.Background(), e); err != nil { // повторная доставка
		t.Fatal(err)
	}
	if gw.refunds != 1 {
		t.Fatalf("возврат должен быть ровно один, было %d", gw.refunds)
	}
	last := bus.published[len(bus.published)-1]
	if last.EventType != events.PaymentRefunded {
		t.Fatalf("ожидалось событие payment.refunded, получили %s", last.EventType)
	}
}

func TestOnSearchFailed_GatewayDownReturnsErrorForRetry(t *testing.T) {
	s, gw, _ := newTestService()
	paid(t, s)
	gw.failRefund = true
	e, _ := events.New(events.CourierSearchFail, "order-1", SearchFailedPayload{OrderID: "order-1"})
	if err := s.OnSearchFailed(context.Background(), e); err == nil {
		t.Fatal("при недоступном банке обработчик должен вернуть ошибку, чтобы брокер повторил доставку")
	}
	if s.Processed.Seen(e.EventID) {
		t.Fatal("необработанное событие не должно помечаться как обработанное")
	}
}

// ---------- интеграционные тесты HTTP-эндпоинтов ----------

func doJSON(t *testing.T, h http.Handler, method, path string, body any, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHTTP_CreatePayment_IdempotencyKey(t *testing.T) {
	s, _, _ := newTestService()
	h := Routes(s, idempotency.NewStore())
	body := CreatePaymentRequest{OrderID: "o", Amount: 450}

	// без ключа — 400 в формате Problem
	rec := doJSON(t, h, "POST", "/internal/v1/payments", body, nil)
	if rec.Code != 400 || rec.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("без Idempotency-Key ожидался 400 problem+json, получили %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}

	key := map[string]string{"Idempotency-Key": "k1"}
	r1 := doJSON(t, h, "POST", "/internal/v1/payments", body, key)
	r2 := doJSON(t, h, "POST", "/internal/v1/payments", body, key)
	if r1.Code != 201 || r2.Code != 201 || r1.Body.String() != r2.Body.String() {
		t.Fatalf("повтор с тем же ключом должен вернуть тот же ответ: %d %d", r1.Code, r2.Code)
	}
	if r2.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatal("повторный ответ должен быть помечен Idempotent-Replayed")
	}

	// тот же ключ, другое тело — 409
	r3 := doJSON(t, h, "POST", "/internal/v1/payments", CreatePaymentRequest{OrderID: "o", Amount: 999}, key)
	if r3.Code != 409 {
		t.Fatalf("ожидался 409, получили %d", r3.Code)
	}
}

func TestHTTP_WebhookAndRefund(t *testing.T) {
	s, _, _ := newTestService()
	h := Routes(s, idempotency.NewStore())

	rec := doJSON(t, h, "POST", "/internal/v1/payments", CreatePaymentRequest{OrderID: "o", Amount: 450},
		map[string]string{"Idempotency-Key": "k"})
	var pr PaymentResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &pr)

	bad := doJSON(t, h, "POST", "/api/v1/payments/webhook",
		WebhookRequest{PaymentID: pr.PaymentID, Status: "SUCCESS", Signature: "x"}, nil)
	if bad.Code != 400 {
		t.Fatalf("неверная подпись: ожидался 400, получили %d", bad.Code)
	}

	ok := doJSON(t, h, "POST", "/api/v1/payments/webhook",
		WebhookRequest{PaymentID: pr.PaymentID, Status: "SUCCESS", Signature: webhook.Sign(secret, pr.PaymentID, "SUCCESS")}, nil)
	if ok.Code != 200 {
		t.Fatalf("вебхук: ожидался 200, получили %d", ok.Code)
	}

	ref := doJSON(t, h, "POST", "/internal/v1/payments/"+pr.PaymentID+"/refund", RefundRequest{Reason: "TEST"}, nil)
	var rr RefundResponse
	_ = json.Unmarshal(ref.Body.Bytes(), &rr)
	if ref.Code != 200 || rr.Status != StatusRefunded {
		t.Fatalf("возврат: ожидался 200 REFUNDED, получили %d %s", ref.Code, rr.Status)
	}

	nf := doJSON(t, h, "POST", "/internal/v1/payments/nope/refund", RefundRequest{Reason: "TEST"}, nil)
	if nf.Code != 404 {
		t.Fatalf("несуществующий платёж: ожидался 404, получили %d", nf.Code)
	}
}
