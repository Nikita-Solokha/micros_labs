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
	"sync/atomic"
	"testing"
	"time"

	"github.com/Nikita-Solokha/micros_labs/pkg/events"
	"github.com/Nikita-Solokha/micros_labs/pkg/idempotency"
)

// ---------- моки ----------

type fakePayments struct {
	fail  bool
	calls int
}

func (f *fakePayments) CreatePayment(_ context.Context, orderID string, _ float64) (PaymentInfo, error) {
	f.calls++
	if f.fail {
		return PaymentInfo{}, ErrPaymentUnavailable
	}
	return PaymentInfo{PaymentID: "pay-" + orderID, PaymentURL: "http://bank/pay/" + orderID, Status: "PENDING"}, nil
}

type recBus struct{ published []events.Envelope }

func (b *recBus) Publish(_ context.Context, _, _ string, e events.Envelope) error {
	b.published = append(b.published, e)
	return nil
}

func newTestService() (*Service, *fakePayments, *recBus) {
	pay := &fakePayments{}
	bus := &recBus{}
	return &Service{
		Store:     NewStore(),
		Payments:  pay,
		Bus:       bus,
		Processed: idempotency.NewProcessed(),
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, pay, bus
}

var validReq = CreateOrderRequest{OriginAddress: "пл. Гагарина, 1", DestinationAddress: "ул. Большая Садовая, 105", Weight: 2.5}

func event(t *testing.T, typ string, payload any) events.Envelope {
	t.Helper()
	e, err := events.New(typ, "", payload)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func mustCreate(t *testing.T, s *Service) Order {
	t.Helper()
	o, err := s.CreateOrder(context.Background(), "client-1", validReq)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func status(s *Service, id string) string {
	o, _ := s.Store.Get(id)
	return o.Status
}

// ---------- unit: правила предметной области ----------

func TestCalcPrice(t *testing.T) {
	if got := CalcPrice(2.5); got != 450 {
		t.Fatalf("2.5 кг: ожидалось 450, получили %v", got)
	}
}

func TestValidate(t *testing.T) {
	bad := []CreateOrderRequest{
		{DestinationAddress: "b", Weight: 1},
		{OriginAddress: "a", Weight: 1},
		{OriginAddress: "a", DestinationAddress: "a", Weight: 1},
		{OriginAddress: "a", DestinationAddress: "b", Weight: 0},
		{OriginAddress: "a", DestinationAddress: "b", Weight: 51},
	}
	for i, r := range bad {
		if err := r.Validate(); !errors.Is(err, ErrValidation) {
			t.Errorf("случай %d: ожидалась ошибка валидации", i)
		}
	}
	if err := validReq.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestStateMachine(t *testing.T) {
	allowed := [][2]string{
		{StatusPendingPayment, StatusPaid}, {StatusPendingPayment, StatusCancelled},
		{StatusPaid, StatusAssigned}, {StatusPaid, StatusCancelled},
		{StatusAssigned, StatusDelivered},
	}
	forbidden := [][2]string{
		{StatusPendingPayment, StatusAssigned}, {StatusPendingPayment, StatusDelivered},
		{StatusAssigned, StatusCancelled}, {StatusDelivered, StatusCancelled},
		{StatusCancelled, StatusPaid},
	}
	for _, p := range allowed {
		if !CanTransition(p[0], p[1]) {
			t.Errorf("%s -> %s должен быть разрешён", p[0], p[1])
		}
	}
	for _, p := range forbidden {
		if CanTransition(p[0], p[1]) {
			t.Errorf("%s -> %s должен быть запрещён", p[0], p[1])
		}
	}
}

// ---------- unit: создание заказа ----------

func TestCreateOrder_Success(t *testing.T) {
	s, pay, bus := newTestService()
	o := mustCreate(t, s)
	if o.Status != StatusPendingPayment || o.PaymentURL == "" || o.Price != 450 {
		t.Fatalf("неожиданный заказ: %+v", o)
	}
	if pay.calls != 1 {
		t.Fatalf("payment-service должен вызываться один раз")
	}
	if len(bus.published) != 1 || bus.published[0].EventType != events.OrderCreated {
		t.Fatal("ожидалось событие order.created")
	}
}

func TestCreateOrder_PaymentDown_Compensation(t *testing.T) {
	s, pay, bus := newTestService()
	pay.fail = true
	_, err := s.CreateOrder(context.Background(), "c", validReq)
	if !errors.Is(err, ErrPaymentUnavailable) {
		t.Fatalf("ожидалась ErrPaymentUnavailable, получили %v", err)
	}
	items, _ := s.Store.ListByClient("c", 1, 10)
	if len(items) != 1 || items[0].Status != StatusCancelled || items[0].CancelReason != ReasonPaymentUnavailable {
		t.Fatalf("заказ должен быть отменён с причиной PAYMENT_SERVICE_UNAVAILABLE: %+v", items)
	}
	if len(bus.published) != 0 {
		t.Fatal("order.created не должен публиковаться для отменённого заказа")
	}
}

// ---------- unit: сага через события ----------

func TestSaga_HappyPath(t *testing.T) {
	s, _, bus := newTestService()
	o := mustCreate(t, s)
	ctx := context.Background()

	if err := s.HandleEvent(ctx, event(t, events.PaymentCompleted, PaymentPayload{OrderID: o.ID})); err != nil {
		t.Fatal(err)
	}
	if status(s, o.ID) != StatusPaid {
		t.Fatal("после payment.completed заказ должен быть PAID")
	}
	if err := s.HandleEvent(ctx, event(t, events.CourierAssigned, CourierAssignedPayload{OrderID: o.ID, CourierID: "cour-1"})); err != nil {
		t.Fatal(err)
	}
	if status(s, o.ID) != StatusAssigned {
		t.Fatal("после courier_assigned заказ должен быть ASSIGNED")
	}
	if _, err := s.MarkDelivered(ctx, o.ID, "cour-1"); err != nil {
		t.Fatal(err)
	}
	if status(s, o.ID) != StatusDelivered {
		t.Fatal("заказ должен быть DELIVERED")
	}
	last := bus.published[len(bus.published)-1]
	if last.EventType != events.OrderDelivered {
		t.Fatalf("ожидалось order.delivered, получили %s", last.EventType)
	}
}

func TestSaga_PaymentFailed_Cancels(t *testing.T) {
	s, _, _ := newTestService()
	o := mustCreate(t, s)
	_ = s.HandleEvent(context.Background(), event(t, events.PaymentFailed, PaymentPayload{OrderID: o.ID}))
	got, _ := s.Store.Get(o.ID)
	if got.Status != StatusCancelled || got.CancelReason != ReasonPaymentFailed {
		t.Fatalf("ожидалась отмена PAYMENT_FAILED: %+v", got)
	}
}

func TestSaga_CourierNotFound_Cancels(t *testing.T) {
	s, _, _ := newTestService()
	o := mustCreate(t, s)
	ctx := context.Background()
	_ = s.HandleEvent(ctx, event(t, events.PaymentCompleted, PaymentPayload{OrderID: o.ID}))
	_ = s.HandleEvent(ctx, event(t, events.CourierSearchFail, SearchFailedPayload{OrderID: o.ID, Reason: "TIMEOUT"}))
	got, _ := s.Store.Get(o.ID)
	if got.Status != StatusCancelled || got.CancelReason != ReasonCourierNotFound {
		t.Fatalf("ожидалась отмена COURIER_NOT_FOUND: %+v", got)
	}
}

func TestEvent_DuplicateIgnored(t *testing.T) {
	s, _, _ := newTestService()
	o := mustCreate(t, s)
	e := event(t, events.PaymentCompleted, PaymentPayload{OrderID: o.ID})
	_ = s.HandleEvent(context.Background(), e)
	if !s.Processed.Seen(e.EventID) {
		t.Fatal("событие должно быть помечено обработанным")
	}
	if err := s.HandleEvent(context.Background(), e); err != nil {
		t.Fatal("повтор должен молча игнорироваться")
	}
}

func TestEvent_OutOfOrder_RetriedLater(t *testing.T) {
	s, _, _ := newTestService()
	o := mustCreate(t, s)
	assigned := event(t, events.CourierAssigned, CourierAssignedPayload{OrderID: o.ID, CourierID: "c1"})

	// курьер назначен раньше, чем order-service обработал оплату
	if err := s.HandleEvent(context.Background(), assigned); !errors.Is(err, ErrNotReady) {
		t.Fatalf("ожидалась ErrNotReady (брокер повторит), получили %v", err)
	}
	_ = s.HandleEvent(context.Background(), event(t, events.PaymentCompleted, PaymentPayload{OrderID: o.ID}))
	if err := s.HandleEvent(context.Background(), assigned); err != nil { // повторная доставка
		t.Fatal(err)
	}
	if status(s, o.ID) != StatusAssigned {
		t.Fatal("после повторной доставки заказ должен быть ASSIGNED")
	}
}

func TestEvent_LatePaymentAfterCancel_Skipped(t *testing.T) {
	s, _, _ := newTestService()
	o := mustCreate(t, s)
	_ = s.HandleEvent(context.Background(), event(t, events.PaymentFailed, PaymentPayload{OrderID: o.ID}))
	err := s.HandleEvent(context.Background(), event(t, events.PaymentCompleted, PaymentPayload{OrderID: o.ID}))
	if err != nil || status(s, o.ID) != StatusCancelled {
		t.Fatal("оплата для отменённого заказа не должна его «воскрешать»")
	}
}

func TestMarkDelivered_Rules(t *testing.T) {
	s, _, _ := newTestService()
	o := mustCreate(t, s)
	ctx := context.Background()
	if _, err := s.MarkDelivered(ctx, o.ID, "c1"); !errors.Is(err, ErrWrongState) {
		t.Fatal("нельзя доставить неоплаченный заказ")
	}
	_ = s.HandleEvent(ctx, event(t, events.PaymentCompleted, PaymentPayload{OrderID: o.ID}))
	_ = s.HandleEvent(ctx, event(t, events.CourierAssigned, CourierAssignedPayload{OrderID: o.ID, CourierID: "c1"}))
	if _, err := s.MarkDelivered(ctx, o.ID, "other"); !errors.Is(err, ErrWrongState) {
		t.Fatal("чужой курьер не может отметить доставку")
	}
	if _, err := s.MarkDelivered(ctx, o.ID, "c1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkDelivered(ctx, o.ID, "c1"); err != nil {
		t.Fatal("повторная отметка должна быть идемпотентной")
	}
}

// ---------- HTTP-клиент к payment-service (против мок-сервера) ----------

func TestHTTPPaymentClient_Contract(t *testing.T) {
	var gotKey, gotCT string
	var gotBody map[string]any
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/payments" {
			w.WriteHeader(404)
			return
		}
		gotKey = r.Header.Get("Idempotency-Key")
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"paymentId":"p1","paymentUrl":"http://bank/pay/p1","status":"PENDING"}`))
	}))
	defer mock.Close()

	c := NewHTTPPaymentClient(mock.URL)
	info, err := c.CreatePayment(context.Background(), "order-42", 450)
	if err != nil {
		t.Fatal(err)
	}
	if info.PaymentID != "p1" || info.PaymentURL == "" {
		t.Fatalf("неверно разобран ответ: %+v", info)
	}
	if gotKey != "order-42" || gotCT != "application/json" {
		t.Fatalf("Idempotency-Key должен быть равен orderId, получили %q %q", gotKey, gotCT)
	}
	if gotBody["orderId"] != "order-42" || gotBody["amount"].(float64) != 450 {
		t.Fatalf("тело запроса не по контракту: %v", gotBody)
	}
}

func TestHTTPPaymentClient_RetriesOn5xx(t *testing.T) {
	var calls int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"paymentId":"p1","paymentUrl":"u","status":"PENDING"}`))
	}))
	defer mock.Close()

	c := NewHTTPPaymentClient(mock.URL)
	c.Backoff = time.Millisecond
	if _, err := c.CreatePayment(context.Background(), "o", 1); err != nil {
		t.Fatalf("после двух 503 третья попытка должна пройти: %v", err)
	}
	if calls != 3 {
		t.Fatalf("ожидалось 3 попытки, было %d", calls)
	}
}

func TestHTTPPaymentClient_NoRetryOn4xx(t *testing.T) {
	var calls int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(400)
	}))
	defer mock.Close()
	c := NewHTTPPaymentClient(mock.URL)
	c.Backoff = time.Millisecond
	if _, err := c.CreatePayment(context.Background(), "o", 1); err == nil || calls != 1 {
		t.Fatalf("400 повторять бессмысленно: calls=%d err=%v", calls, err)
	}
}

func TestBreaker_OpensAfterFailures(t *testing.T) {
	var calls int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(500)
	}))
	defer mock.Close()
	c := NewHTTPPaymentClient(mock.URL)
	c.Backoff = time.Millisecond
	c.Retries = 1
	c.Breaker = NewBreaker(2, time.Minute)

	_, _ = c.CreatePayment(context.Background(), "o", 1)
	_, _ = c.CreatePayment(context.Background(), "o", 1)
	before := atomic.LoadInt32(&calls)
	_, err := c.CreatePayment(context.Background(), "o", 1)
	if !errors.Is(err, ErrPaymentUnavailable) || atomic.LoadInt32(&calls) != before {
		t.Fatal("после 2 неудач breaker должен открыться и не звать сервис")
	}
}

// ---------- HTTP-эндпоинты order-service ----------

func do(t *testing.T, h http.Handler, method, path string, body any, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHTTP_CreateAndGet(t *testing.T) {
	s, pay, _ := newTestService()
	h := Routes(s, idempotency.NewStore())
	hdr := map[string]string{"Idempotency-Key": "k1", ClientHeader: "client-1"}

	r1 := do(t, h, "POST", "/api/v1/orders", validReq, hdr)
	r2 := do(t, h, "POST", "/api/v1/orders", validReq, hdr)
	if r1.Code != 201 || r1.Body.String() != r2.Body.String() || pay.calls != 1 {
		t.Fatalf("повтор с тем же ключом не должен создавать второй заказ: %d calls=%d", r1.Code, pay.calls)
	}
	var o Order
	_ = json.Unmarshal(r1.Body.Bytes(), &o)

	if rec := do(t, h, "GET", "/api/v1/orders/"+o.ID, nil, nil); rec.Code != 200 {
		t.Fatalf("GET заказа: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/api/v1/orders/nope", nil, nil); rec.Code != 404 || rec.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("несуществующий заказ: ожидался 404 problem+json, получили %d", rec.Code)
	}
	rec := do(t, h, "GET", "/internal/v1/orders/"+o.ID+"/status", nil, nil)
	var st StatusResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if rec.Code != 200 || st.Status != StatusPendingPayment {
		t.Fatalf("внутренний статус: %d %+v", rec.Code, st)
	}
}

func TestHTTP_CreateValidationAndPaymentDown(t *testing.T) {
	s, pay, _ := newTestService()
	h := Routes(s, idempotency.NewStore())
	if rec := do(t, h, "POST", "/api/v1/orders", CreateOrderRequest{Weight: -1}, map[string]string{"Idempotency-Key": "a"}); rec.Code != 400 {
		t.Fatalf("валидация: ожидался 400, получили %d", rec.Code)
	}
	pay.fail = true
	if rec := do(t, h, "POST", "/api/v1/orders", validReq, map[string]string{"Idempotency-Key": "b"}); rec.Code != 503 {
		t.Fatalf("payment недоступен: ожидался 503, получили %d", rec.Code)
	}
}

func TestHTTP_ListPagination(t *testing.T) {
	s, _, _ := newTestService()
	h := Routes(s, idempotency.NewStore())
	for i := 0; i < 5; i++ {
		_, _ = s.CreateOrder(context.Background(), "me", validReq)
	}
	_, _ = s.CreateOrder(context.Background(), "someone-else", validReq)

	rec := do(t, h, "GET", "/api/v1/orders?page=2&size=2", nil, map[string]string{ClientHeader: "me"})
	var resp OrderListResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != 200 || resp.Total != 5 || len(resp.Items) != 2 || resp.Page != 2 {
		t.Fatalf("пагинация: %d %+v", rec.Code, resp)
	}
	for _, q := range []string{"page=0", "size=0", "size=101", "page=abc"} {
		if rec := do(t, h, "GET", "/api/v1/orders?"+q, nil, nil); rec.Code != 400 {
			t.Errorf("%s: ожидался 400, получили %d", q, rec.Code)
		}
	}
}

func TestHTTP_Delivered(t *testing.T) {
	s, _, _ := newTestService()
	h := Routes(s, idempotency.NewStore())
	o := mustCreate(t, s)
	if rec := do(t, h, "POST", "/internal/v1/orders/"+o.ID+"/delivered", DeliveredRequest{CourierID: "c1"}, nil); rec.Code != 409 {
		t.Fatalf("доставка неоплаченного: ожидался 409, получили %d", rec.Code)
	}
	if rec := do(t, h, "POST", "/internal/v1/orders/nope/delivered", DeliveredRequest{CourierID: "c1"}, nil); rec.Code != 404 {
		t.Fatalf("несуществующий заказ: ожидался 404, получили %d", rec.Code)
	}
}
