// Сквозные (интеграционные) тесты саги: payment-, order- и dispatch-service
// работают в одном процессе, общаются по НАСТОЯЩЕМУ HTTP (httptest-серверы)
// и через общий in-memory брокер. Внешний банк — мок.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Nikita-Solokha/micros_labs/pkg/events"
	"github.com/Nikita-Solokha/micros_labs/pkg/httpx"
	"github.com/Nikita-Solokha/micros_labs/pkg/idempotency"
	"github.com/Nikita-Solokha/micros_labs/pkg/webhook"
	dispatch "github.com/Nikita-Solokha/micros_labs/services/dispatch-service/app"
	order "github.com/Nikita-Solokha/micros_labs/services/order-service/app"
	payment "github.com/Nikita-Solokha/micros_labs/services/payment-service/app"
)

const secret = "e2e-secret"

type bank struct{ refunds int }

func (b *bank) CreateSession(_ context.Context, id string, _ float64, _ string) (string, error) {
	return "http://bank/pay/" + id, nil
}
func (b *bank) Refund(context.Context, string, float64) (string, error) {
	b.refunds++
	return "refund-1", nil
}

type system struct {
	bus      *events.MemoryBus
	bank     *bank
	pay      *payment.Service
	disp     *dispatch.Service
	payURL   string
	orderURL string
	dispURL  string
	close    func()
}

func start(t *testing.T, searchTimeout time.Duration) *system {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := events.NewMemoryBus(log)
	bus.Backoff = 20 * time.Millisecond
	bus.Retries = 10
	b := &bank{}

	pay := &payment.Service{
		Store: payment.NewStore(), Gateway: b, Bus: bus, Processed: idempotency.NewProcessed(),
		WebhookSecret: secret, Log: log,
	}
	paySrv := httptest.NewServer(httpx.Middleware(log, payment.Routes(pay, idempotency.NewStore())))

	ord := &order.Service{
		Store: order.NewStore(), Payments: order.NewHTTPPaymentClient(paySrv.URL), Bus: bus,
		Processed: idempotency.NewProcessed(), Log: log,
	}
	ordSrv := httptest.NewServer(httpx.Middleware(log, order.Routes(ord, idempotency.NewStore())))

	disp := &dispatch.Service{
		Store: dispatch.NewStore(), Orders: dispatch.NewHTTPOrderClient(ordSrv.URL), Geo: dispatch.MockGeocoder{},
		Bus: bus, Processed: idempotency.NewProcessed(), Log: log,
		Cfg: dispatch.Config{SearchTimeout: searchTimeout, SearchInterval: 50 * time.Millisecond, LocationTTL: time.Minute, RadiusKm: 15},
	}
	dispSrv := httptest.NewServer(httpx.Middleware(log, dispatch.Routes(disp)))

	// подписки — как в main.go каждого сервиса
	bus.Subscribe(events.TopicDispatch, "payment-service", func(ctx context.Context, e events.Envelope) error {
		if e.EventType != events.CourierSearchFail {
			return nil
		}
		return pay.OnSearchFailed(ctx, e)
	})
	bus.Subscribe(events.TopicPayments, "order-service", ord.HandleEvent)
	bus.Subscribe(events.TopicDispatch, "order-service", ord.HandleEvent)
	bus.Subscribe(events.TopicOrders, "dispatch-service", disp.HandleEvent)
	bus.Subscribe(events.TopicPayments, "dispatch-service", disp.HandleEvent)

	ctx, cancel := context.WithCancel(context.Background())
	go disp.Run(ctx)

	return &system{
		bus: bus, bank: b, pay: pay, disp: disp,
		payURL: paySrv.URL, orderURL: ordSrv.URL, dispURL: dispSrv.URL,
		close: func() { cancel(); bus.Wait(); dispSrv.Close(); ordSrv.Close(); paySrv.Close() },
	}
}

func call(t *testing.T, method, url string, body any, hdr map[string]string) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, r)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (s *system) createOrder(t *testing.T) (string, string) {
	code, o := call(t, "POST", s.orderURL+"/api/v1/orders",
		map[string]any{"originAddress": "пл. Гагарина, 1", "destinationAddress": "ул. Большая Садовая, 105", "weight": 2.5},
		map[string]string{"Idempotency-Key": httpx.NewID()})
	if code != 201 {
		t.Fatalf("создание заказа: %d %v", code, o)
	}
	return o["id"].(string), o["paymentId"].(string)
}

// bankWebhook — банк сообщает результат оплаты (как делает mocks/bank-mock).
func (s *system) bankWebhook(t *testing.T, paymentID, status string) {
	code, _ := call(t, "POST", s.payURL+"/api/v1/payments/webhook",
		map[string]string{"paymentId": paymentID, "status": status, "signature": webhook.Sign(secret, paymentID, status)}, nil)
	if code != 200 {
		t.Fatalf("вебхук: %d", code)
	}
}

func (s *system) waitOrder(t *testing.T, id, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var o map[string]any
	for time.Now().Before(deadline) {
		_, o = call(t, "GET", s.orderURL+"/api/v1/orders/"+id, nil, nil)
		if o["status"] == want {
			return o
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("заказ %s не перешёл в %s, текущий: %v", id, want, o["status"])
	return nil
}

func courierOnShift(t *testing.T, s *system, id string) {
	if code, _ := call(t, "POST", s.dispURL+"/api/v1/couriers/shifts", map[string]string{"courierId": id, "action": "START_SHIFT"}, nil); code != 200 {
		t.Fatalf("смена: %d", code)
	}
	if code, _ := call(t, "POST", s.dispURL+"/api/v1/dispatch/location",
		map[string]any{"courierId": id, "lat": 47.2357, "lon": 39.7015, "status": "ONLINE"}, nil); code != 200 {
		t.Fatalf("координаты: %d", code)
	}
}

// Успешный сквозной сценарий: заказ -> оплата -> назначение курьера -> доставка.
func TestSaga_HappyPath(t *testing.T) {
	s := start(t, time.Minute)
	defer s.close()

	courierOnShift(t, s, "courier-1")
	orderID, paymentID := s.createOrder(t)
	s.bankWebhook(t, paymentID, "SUCCESS")

	o := s.waitOrder(t, orderID, "ASSIGNED")
	if o["courierId"] != "courier-1" {
		t.Fatalf("назначен не тот курьер: %v", o["courierId"])
	}
	code, a := call(t, "GET", s.dispURL+"/api/v1/couriers/courier-1/assignment", nil, nil)
	if code != 200 || a["orderId"] != orderID {
		t.Fatalf("курьер не видит назначение: %d %v", code, a)
	}
	if code, _ := call(t, "POST", s.dispURL+"/api/v1/couriers/courier-1/assignment/complete", nil, nil); code != 200 {
		t.Fatalf("завершение доставки: %d", code)
	}
	s.waitOrder(t, orderID, "DELIVERED")
	if s.bank.refunds != 0 {
		t.Fatal("в успешном сценарии возвратов быть не должно")
	}
}

// Сбой: курьер не найден за SearchTimeout -> компенсации: заказ отменён, деньги возвращены.
func TestSaga_CourierNotFound_Compensation(t *testing.T) {
	s := start(t, 300*time.Millisecond)
	defer s.close()

	orderID, paymentID := s.createOrder(t)
	s.bankWebhook(t, paymentID, "SUCCESS")
	s.waitOrder(t, orderID, "PAID")

	o := s.waitOrder(t, orderID, "CANCELLED")
	if o["cancelReason"] != order.ReasonCourierNotFound {
		t.Fatalf("неверная причина отмены: %v", o["cancelReason"])
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, p := call(t, "GET", s.payURL+"/internal/v1/payments/"+paymentID, nil, nil); p["status"] == payment.StatusRefunded {
			if s.bank.refunds != 1 {
				t.Fatalf("возврат должен быть ровно один, было %d", s.bank.refunds)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("платёж не был возвращён")
}

// Сбой: банк отклонил оплату -> заказ отменён, поиск курьера не начинается.
func TestSaga_PaymentFailed(t *testing.T) {
	s := start(t, time.Minute)
	defer s.close()

	courierOnShift(t, s, "courier-1")
	orderID, paymentID := s.createOrder(t)
	s.bankWebhook(t, paymentID, "FAILED")

	o := s.waitOrder(t, orderID, "CANCELLED")
	if o["cancelReason"] != order.ReasonPaymentFailed {
		t.Fatalf("неверная причина отмены: %v", o["cancelReason"])
	}
	time.Sleep(200 * time.Millisecond)
	if code, _ := call(t, "GET", s.dispURL+"/api/v1/couriers/courier-1/assignment", nil, nil); code != 404 {
		t.Fatal("на неоплаченный заказ курьер назначаться не должен")
	}
}

// Сбой: payment-service недоступен при создании заказа -> 503 и заказ отменён.
func TestSaga_PaymentServiceDown(t *testing.T) {
	s := start(t, time.Minute)
	defer s.close()

	ordDown := &order.Service{
		Store: order.NewStore(), Payments: order.NewHTTPPaymentClient("http://127.0.0.1:1"),
		Bus: s.bus, Processed: idempotency.NewProcessed(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	srv := httptest.NewServer(order.Routes(ordDown, idempotency.NewStore()))
	defer srv.Close()

	code, p := call(t, "POST", srv.URL+"/api/v1/orders",
		map[string]any{"originAddress": "A", "destinationAddress": "B", "weight": 1},
		map[string]string{"Idempotency-Key": "k"})
	if code != 503 || p["status"].(float64) != 503 {
		t.Fatalf("ожидался 503 Problem, получили %d %v", code, p)
	}
	_, list := call(t, "GET", srv.URL+"/api/v1/orders", nil, nil)
	items := list["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["status"] != "CANCELLED" {
		t.Fatalf("заказ должен быть отменён: %v", list)
	}
}
