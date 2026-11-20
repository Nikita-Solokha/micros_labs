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
	"sync"
	"testing"
	"time"

	"github.com/Nikita-Solokha/micros_labs/pkg/events"
	"github.com/Nikita-Solokha/micros_labs/pkg/idempotency"
)

// ---------- моки ----------

type fakeOrders struct {
	mu        sync.Mutex
	status    map[string]string
	down      bool
	delivered []string
}

func (f *fakeOrders) GetStatus(_ context.Context, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return "", ErrOrderUnavailable
	}
	st, ok := f.status[id]
	if !ok {
		return "", ErrOrderNotFound
	}
	return st, nil
}

func (f *fakeOrders) MarkDelivered(_ context.Context, id, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return ErrOrderUnavailable
	}
	f.delivered = append(f.delivered, id)
	f.status[id] = "DELIVERED"
	return nil
}

// fixedGeo — все адреса в одной точке, чтобы считать расстояния в тестах.
type fixedGeo struct{ lat, lon float64 }

func (g fixedGeo) Geocode(string) (float64, float64) { return g.lat, g.lon }

type recBus struct {
	mu        sync.Mutex
	published []events.Envelope
}

func (b *recBus) Publish(_ context.Context, _, _ string, e events.Envelope) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.published = append(b.published, e)
	return nil
}

func (b *recBus) types() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, e := range b.published {
		out = append(out, e.EventType)
	}
	return out
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

const pickupLat, pickupLon = 47.2357, 39.7015

func newTestService() (*Service, *fakeOrders, *recBus, *clock) {
	orders := &fakeOrders{status: map[string]string{}}
	bus := &recBus{}
	clk := &clock{t: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)}
	return &Service{
		Store:     NewStore(),
		Orders:    orders,
		Geo:       fixedGeo{pickupLat, pickupLon},
		Bus:       bus,
		Processed: idempotency.NewProcessed(),
		Cfg: Config{
			SearchTimeout:  15 * time.Minute,
			SearchInterval: time.Second,
			LocationTTL:    time.Minute,
			RadiusKm:       15,
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: clk.now,
	}, orders, bus, clk
}

func ev(t *testing.T, typ string, payload any) events.Envelope {
	t.Helper()
	e, err := events.New(typ, "", payload)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func courierOnline(t *testing.T, s *Service, id string, lat, lon float64) {
	t.Helper()
	if _, err := s.Shift(ShiftRequest{CourierID: id, Action: ActionStartShift}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateLocation(LocationRequest{CourierID: id, Lat: lat, Lon: lon, Status: CourierOnline}); err != nil {
		t.Fatal(err)
	}
}

// оплаченный заказ: order.created + payment.completed
func paidOrder(t *testing.T, s *Service, orders *fakeOrders, id string) {
	t.Helper()
	orders.status[id] = "PAID"
	ctx := context.Background()
	if err := s.HandleEvent(ctx, ev(t, events.OrderCreated, OrderCreatedPayload{ID: id, OriginAddress: "пл. Гагарина, 1"})); err != nil {
		t.Fatal(err)
	}
	if err := s.HandleEvent(ctx, ev(t, events.PaymentCompleted, PaymentCompletedPayload{OrderID: id})); err != nil {
		t.Fatal(err)
	}
}

// ---------- unit: геометрия и геокодер ----------

func TestDistanceKm(t *testing.T) {
	// Ростов -> Москва ≈ 960 км
	d := DistanceKm(47.2357, 39.7015, 55.7558, 37.6173)
	if d < 940 || d > 980 {
		t.Fatalf("расстояние Ростов-Москва ≈ 960 км, получили %.0f", d)
	}
	if DistanceKm(1, 1, 1, 1) != 0 {
		t.Fatal("расстояние до той же точки должно быть 0")
	}
}

func TestMockGeocoder_Deterministic(t *testing.T) {
	g := MockGeocoder{}
	lat1, lon1 := g.Geocode("ул. Большая Садовая, 105")
	lat2, lon2 := g.Geocode("ул. Большая Садовая, 105")
	if lat1 != lat2 || lon1 != lon2 {
		t.Fatal("один адрес должен давать одни координаты")
	}
	if DistanceKm(lat1, lon1, centerLat, centerLon) > 10 {
		t.Fatal("точка должна быть в пределах города")
	}
}

// ---------- unit: смены и координаты ----------

func TestShiftAndLocationRules(t *testing.T) {
	s, _, _, _ := newTestService()

	if _, err := s.UpdateLocation(LocationRequest{CourierID: "c1", Lat: 47, Lon: 39, Status: CourierOnline}); !errors.Is(err, ErrCourierNotFound) {
		t.Fatalf("координаты без смены: ожидалась ErrCourierNotFound, получили %v", err)
	}
	if _, err := s.Shift(ShiftRequest{CourierID: "c1", Action: "DANCE"}); !errors.Is(err, ErrValidation) {
		t.Fatal("неизвестный action должен отклоняться")
	}
	c, _ := s.Shift(ShiftRequest{CourierID: "c1", Action: ActionStartShift})
	if c.Status != CourierOnline {
		t.Fatal("после START_SHIFT курьер ONLINE")
	}
	for _, bad := range []LocationRequest{
		{CourierID: "c1", Lat: 91, Lon: 0, Status: CourierOnline},
		{CourierID: "c1", Lat: 0, Lon: 181, Status: CourierOnline},
		{CourierID: "c1", Lat: 0, Lon: 0, Status: "SLEEPING"},
	} {
		if _, err := s.UpdateLocation(bad); !errors.Is(err, ErrValidation) {
			t.Errorf("%+v: ожидалась ошибка валидации", bad)
		}
	}
	c, _ = s.Shift(ShiftRequest{CourierID: "c1", Action: ActionEndShift})
	if c.Status != CourierOffline {
		t.Fatal("после END_SHIFT курьер OFFLINE")
	}
	if _, err := s.UpdateLocation(LocationRequest{CourierID: "c1", Lat: 47, Lon: 39, Status: CourierOnline}); !errors.Is(err, ErrConflict) {
		t.Fatal("офлайн-курьер не может слать координаты")
	}
}

// ---------- unit: подбор курьера ----------

func TestAssign_Nearest(t *testing.T) {
	s, orders, bus, _ := newTestService()
	courierOnline(t, s, "far", pickupLat+0.05, pickupLon)   // ~5.5 км
	courierOnline(t, s, "near", pickupLat+0.005, pickupLon) // ~0.5 км
	courierOnline(t, s, "away", pickupLat+1.0, pickupLon)   // ~110 км, вне радиуса

	paidOrder(t, s, orders, "o1")

	a, err := s.CurrentAssignment("near")
	if err != nil || a.OrderID != "o1" {
		t.Fatalf("должен быть назначен ближайший курьер: %v", err)
	}
	c, _ := s.Store.Courier("near")
	if c.Status != CourierBusy {
		t.Fatal("назначенный курьер должен стать BUSY")
	}
	if got := bus.types(); len(got) != 1 || got[0] != events.CourierAssigned {
		t.Fatalf("ожидалось одно событие courier_assigned, получили %v", got)
	}
}

func TestAssign_SkipsBusyAndStaleCouriers(t *testing.T) {
	s, orders, _, clk := newTestService()
	courierOnline(t, s, "stale", pickupLat, pickupLon)
	clk.advance(2 * time.Minute) // координаты «протухли» (TTL 1 мин)
	courierOnline(t, s, "fresh", pickupLat+0.01, pickupLon)

	paidOrder(t, s, orders, "o1")
	if _, err := s.CurrentAssignment("fresh"); err != nil {
		t.Fatal("курьер со старыми координатами не должен назначаться")
	}
	paidOrder(t, s, orders, "o2") // fresh уже занят, stale протух
	if len(s.Store.Searches()) != 1 {
		t.Fatal("для второго заказа курьера нет — заказ должен остаться в поиске")
	}
}

func TestAssign_OneCourierTwoOrders_NoDoubleBooking(t *testing.T) {
	s, orders, _, _ := newTestService()
	courierOnline(t, s, "c1", pickupLat, pickupLon)
	var wg sync.WaitGroup
	for _, id := range []string{"o1", "o2", "o3", "o4"} {
		orders.status[id] = "PAID"
		s.Store.SaveOrder(OrderInfo{OrderID: id, PickupLat: pickupLat, PickupLon: pickupLon})
	}
	results := make(chan bool, 4)
	for _, id := range []string{"o1", "o2", "o3", "o4"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			results <- s.TryAssign(context.Background(), id)
		}(id)
	}
	wg.Wait()
	close(results)
	n := 0
	for ok := range results {
		if ok {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("один курьер может получить только один заказ, назначений: %d", n)
	}
}

func TestAssign_LaterWhenCourierAppears(t *testing.T) {
	s, orders, bus, clk := newTestService()
	paidOrder(t, s, orders, "o1")
	if len(bus.types()) != 0 {
		t.Fatal("курьеров нет — назначения быть не должно")
	}
	clk.advance(time.Minute)
	courierOnline(t, s, "c1", pickupLat, pickupLon)
	s.Tick(context.Background())
	if got := bus.types(); len(got) != 1 || got[0] != events.CourierAssigned {
		t.Fatalf("после появления курьера фоновый поиск должен его назначить: %v", got)
	}
}

// ---------- unit: сага ----------

func TestSaga_SearchTimeout_PublishesSearchFailed(t *testing.T) {
	s, orders, bus, clk := newTestService()
	paidOrder(t, s, orders, "o1")

	clk.advance(14 * time.Minute)
	s.Tick(context.Background())
	if len(bus.types()) != 0 {
		t.Fatal("до 15 минут событий быть не должно")
	}
	clk.advance(time.Minute)
	s.Tick(context.Background())
	got := bus.types()
	if len(got) != 1 || got[0] != events.CourierSearchFail {
		t.Fatalf("через 15 минут ожидалось dispatch.search_failed, получили %v", got)
	}
	var p SearchFailedPayload
	_ = bus.published[0].Decode(&p)
	if p.OrderID != "o1" || p.Reason != ReasonSearchTimeout {
		t.Fatalf("неверный payload: %+v", p)
	}
	s.Tick(context.Background())
	if len(bus.types()) != 1 {
		t.Fatal("search_failed публикуется один раз")
	}
}

func TestSaga_CancelledOrderNotAssigned(t *testing.T) {
	s, orders, bus, _ := newTestService()
	courierOnline(t, s, "c1", pickupLat, pickupLon)
	orders.status["o1"] = "CANCELLED"
	_ = s.HandleEvent(context.Background(), ev(t, events.OrderCreated, OrderCreatedPayload{ID: "o1", OriginAddress: "x"}))
	_ = s.HandleEvent(context.Background(), ev(t, events.PaymentCompleted, PaymentCompletedPayload{OrderID: "o1"}))
	if len(bus.types()) != 0 || len(s.Store.Searches()) != 0 {
		t.Fatal("на отменённый заказ курьер не назначается, поиск прекращается")
	}
}

func TestOrderServiceDown_SearchContinues(t *testing.T) {
	s, orders, bus, _ := newTestService()
	courierOnline(t, s, "c1", pickupLat, pickupLon)
	orders.down = true
	paidOrder(t, s, orders, "o1")
	if len(bus.types()) != 0 || len(s.Store.Searches()) != 1 {
		t.Fatal("пока order-service недоступен, назначение откладывается")
	}
	orders.down = false
	s.Tick(context.Background())
	if len(bus.types()) != 1 {
		t.Fatal("после восстановления order-service курьер должен назначиться")
	}
}

func TestEvents_PaymentBeforeOrderCreated_Retry(t *testing.T) {
	s, _, _, _ := newTestService()
	e := ev(t, events.PaymentCompleted, PaymentCompletedPayload{OrderID: "o1"})
	if err := s.HandleEvent(context.Background(), e); !errors.Is(err, ErrNotReady) {
		t.Fatalf("без order.created ожидалась ErrNotReady (повтор брокером), получили %v", err)
	}
	if s.Processed.Seen(e.EventID) {
		t.Fatal("необработанное событие не должно помечаться")
	}
}

func TestEvents_DuplicatePaymentCompleted(t *testing.T) {
	s, orders, bus, _ := newTestService()
	courierOnline(t, s, "c1", pickupLat, pickupLon)
	courierOnline(t, s, "c2", pickupLat, pickupLon)
	orders.status["o1"] = "PAID"
	_ = s.HandleEvent(context.Background(), ev(t, events.OrderCreated, OrderCreatedPayload{ID: "o1", OriginAddress: "x"}))
	e := ev(t, events.PaymentCompleted, PaymentCompletedPayload{OrderID: "o1"})
	_ = s.HandleEvent(context.Background(), e)
	_ = s.HandleEvent(context.Background(), e)
	if len(bus.types()) != 1 {
		t.Fatalf("повтор события не должен назначать второго курьера: %v", bus.types())
	}
}

// ---------- unit: завершение доставки ----------

func TestCompleteDelivery(t *testing.T) {
	s, orders, _, _ := newTestService()
	courierOnline(t, s, "c1", pickupLat, pickupLon)
	paidOrder(t, s, orders, "o1")

	orders.down = true
	if _, err := s.CompleteDelivery(context.Background(), "c1"); !errors.Is(err, ErrOrderUnavailable) {
		t.Fatal("order-service недоступен — ожидалась ошибка")
	}
	if c, _ := s.Store.Courier("c1"); c.Status != CourierBusy {
		t.Fatal("пока доставка не подтверждена, курьер остаётся BUSY")
	}
	orders.down = false
	if _, err := s.CompleteDelivery(context.Background(), "c1"); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.Store.Courier("c1"); c.Status != CourierOnline || c.CurrentOrderID != "" {
		t.Fatal("после доставки курьер снова ONLINE и свободен")
	}
	if _, err := s.CompleteDelivery(context.Background(), "c1"); !errors.Is(err, ErrNoAssignment) {
		t.Fatal("второй раз завершать нечего")
	}
	if _, err := s.Shift(ShiftRequest{CourierID: "c1", Action: ActionEndShift}); err != nil {
		t.Fatal("свободный курьер может закрыть смену")
	}
}

func TestEndShiftWhileBusy(t *testing.T) {
	s, orders, _, _ := newTestService()
	courierOnline(t, s, "c1", pickupLat, pickupLon)
	paidOrder(t, s, orders, "o1")
	if _, err := s.Shift(ShiftRequest{CourierID: "c1", Action: ActionEndShift}); !errors.Is(err, ErrConflict) {
		t.Fatal("нельзя закрыть смену с активным заказом")
	}
}

// ---------- HTTP-клиент к order-service (против мок-сервера) ----------

func TestHTTPOrderClient_Contract(t *testing.T) {
	var deliveredBody map[string]string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/internal/v1/orders/o1/status":
			_, _ = w.Write([]byte(`{"orderId":"o1","status":"PAID"}`))
		case r.Method == "POST" && r.URL.Path == "/internal/v1/orders/o1/delivered":
			_ = json.NewDecoder(r.Body).Decode(&deliveredBody)
			_, _ = w.Write([]byte(`{"orderId":"o1","status":"DELIVERED"}`))
		case r.URL.Path == "/internal/v1/orders/o2/delivered":
			w.WriteHeader(409)
		default:
			w.WriteHeader(404)
		}
	}))
	defer mock.Close()
	c := NewHTTPOrderClient(mock.URL)
	c.Backoff = time.Millisecond

	if st, err := c.GetStatus(context.Background(), "o1"); err != nil || st != "PAID" {
		t.Fatalf("GetStatus: %q %v", st, err)
	}
	if _, err := c.GetStatus(context.Background(), "nope"); !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("404 -> ErrOrderNotFound, получили %v", err)
	}
	if err := c.MarkDelivered(context.Background(), "o1", "c1"); err != nil || deliveredBody["courierId"] != "c1" {
		t.Fatalf("MarkDelivered: %v %v", err, deliveredBody)
	}
	if err := c.MarkDelivered(context.Background(), "o2", "c1"); !errors.Is(err, ErrOrderRejected) {
		t.Fatalf("409 -> ErrOrderRejected, получили %v", err)
	}
}

func TestHTTPOrderClient_Down(t *testing.T) {
	c := NewHTTPOrderClient("http://127.0.0.1:1")
	c.Backoff = time.Millisecond
	if _, err := c.GetStatus(context.Background(), "o1"); !errors.Is(err, ErrOrderUnavailable) {
		t.Fatalf("ожидалась ErrOrderUnavailable, получили %v", err)
	}
}

// ---------- HTTP-эндпоинты ----------

func do(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, r))
	return rec
}

func TestHTTP_CourierFlow(t *testing.T) {
	s, orders, _, _ := newTestService()
	h := Routes(s)

	if rec := do(t, h, "POST", "/api/v1/dispatch/location", LocationRequest{CourierID: "c1", Lat: 47, Lon: 39, Status: "ONLINE"}); rec.Code != 404 {
		t.Fatalf("координаты без смены: ожидался 404, получили %d", rec.Code)
	}
	rec := do(t, h, "POST", "/api/v1/couriers/shifts", ShiftRequest{CourierID: "c1", Action: "START_SHIFT"})
	var sr ShiftResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &sr)
	if rec.Code != 200 || sr.Status != "ONLINE" {
		t.Fatalf("START_SHIFT: %d %+v", rec.Code, sr)
	}
	if rec := do(t, h, "POST", "/api/v1/dispatch/location", LocationRequest{CourierID: "c1", Lat: 95, Lon: 39, Status: "ONLINE"}); rec.Code != 400 || rec.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("неверная широта: ожидался 400 problem+json, получили %d", rec.Code)
	}
	if rec := do(t, h, "POST", "/api/v1/dispatch/location", LocationRequest{CourierID: "c1", Lat: pickupLat, Lon: pickupLon, Status: "ONLINE"}); rec.Code != 200 {
		t.Fatalf("координаты: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/api/v1/couriers/c1/assignment", nil); rec.Code != 404 {
		t.Fatalf("назначений ещё нет: ожидался 404, получили %d", rec.Code)
	}

	paidOrder(t, s, orders, "o1")

	rec = do(t, h, "GET", "/api/v1/couriers/c1/assignment", nil)
	var ar AssignmentResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &ar)
	if rec.Code != 200 || ar.OrderID != "o1" {
		t.Fatalf("назначение: %d %+v", rec.Code, ar)
	}
	if rec := do(t, h, "POST", "/api/v1/couriers/shifts", ShiftRequest{CourierID: "c1", Action: "END_SHIFT"}); rec.Code != 409 {
		t.Fatalf("закрыть смену с заказом: ожидался 409, получили %d", rec.Code)
	}
	if rec := do(t, h, "POST", "/api/v1/couriers/c1/assignment/complete", nil); rec.Code != 200 {
		t.Fatalf("завершение доставки: %d %s", rec.Code, rec.Body.String())
	}
	if len(orders.delivered) != 1 || orders.delivered[0] != "o1" {
		t.Fatal("dispatch должен синхронно сообщить order-service о доставке")
	}
}
