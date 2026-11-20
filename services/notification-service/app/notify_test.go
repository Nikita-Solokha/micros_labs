package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Nikita-Solokha/micros_labs/pkg/events"
	"github.com/Nikita-Solokha/micros_labs/pkg/idempotency"
)

func newTestService() (*Service, *MockSMS) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sms := &MockSMS{Log: log}
	return &Service{Store: &Store{}, SMS: sms, Processed: idempotency.NewProcessed(), Log: log}, sms
}

func ev(t *testing.T, typ, orderID string) events.Envelope {
	t.Helper()
	e, err := events.New(typ, orderID, map[string]string{"orderId": orderID, "courierId": "c1"})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestMessageFor(t *testing.T) {
	cases := map[string]string{
		events.CourierAssigned:   "курьер назначен",
		events.CourierSearchFail: "не найден",
		events.OrderDelivered:    "доставлен",
	}
	for typ, want := range cases {
		_, text := MessageFor(ev(t, typ, "order-123456789"))
		if !strings.Contains(text, want) || !strings.Contains(text, "order-12") {
			t.Errorf("%s: неожиданный текст %q", typ, text)
		}
	}
	if _, text := MessageFor(ev(t, events.OrderCreated, "o")); text != "" {
		t.Error("на order.created SMS не отправляется")
	}
}

func TestClientPhone_Format(t *testing.T) {
	p := ClientPhone("abc")
	if len(p) != 12 || !strings.HasPrefix(p, "+7999") || p != ClientPhone("abc") {
		t.Fatalf("номер должен быть +7999XXXXXXX и детерминированным: %s", p)
	}
}

func TestHandleEvent_SendsAndLogs(t *testing.T) {
	s, _ := newTestService()
	_ = s.HandleEvent(context.Background(), ev(t, events.CourierAssigned, "o1"))
	_ = s.HandleEvent(context.Background(), ev(t, events.OrderDelivered, "o1"))
	_ = s.HandleEvent(context.Background(), ev(t, events.OrderCreated, "o1")) // без SMS
	resp, _ := s.Logs("o1", 1, 50)
	if resp.Total != 2 || resp.Items[0].Channel != "SMS" {
		t.Fatalf("ожидалось 2 SMS, получили %+v", resp)
	}
}

func TestHandleEvent_DuplicateIgnored(t *testing.T) {
	s, _ := newTestService()
	e := ev(t, events.CourierAssigned, "o1")
	_ = s.HandleEvent(context.Background(), e)
	_ = s.HandleEvent(context.Background(), e)
	if resp, _ := s.Logs("", 1, 50); resp.Total != 1 {
		t.Fatalf("повтор события не должен слать вторую SMS, отправлено %d", resp.Total)
	}
}

func TestHandleEvent_ProviderDown_RetryLater(t *testing.T) {
	s, sms := newTestService()
	sms.SetFail(true)
	e := ev(t, events.CourierAssigned, "o1")
	if err := s.HandleEvent(context.Background(), e); err == nil {
		t.Fatal("при сбое провайдера обработчик возвращает ошибку — брокер повторит")
	}
	if s.Processed.Seen(e.EventID) {
		t.Fatal("неотправленное уведомление не должно помечаться обработанным")
	}
	sms.SetFail(false)
	if err := s.HandleEvent(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if resp, _ := s.Logs("", 1, 50); resp.Total != 1 {
		t.Fatal("после восстановления провайдера SMS должна уйти")
	}
}

func TestHTTP_Logs(t *testing.T) {
	s, _ := newTestService()
	for _, id := range []string{"o1", "o2", "o3"} {
		_ = s.HandleEvent(context.Background(), ev(t, events.CourierAssigned, id))
	}
	h := Routes(s)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/internal/v1/notifications/logs?page=1&size=2", nil))
	var resp LogResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != 200 || resp.Total != 3 || len(resp.Items) != 2 {
		t.Fatalf("пагинация: %d %+v", rec.Code, resp)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/internal/v1/notifications/logs?orderId=o2", nil))
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Total != 1 || resp.Items[0].OrderID != "o2" {
		t.Fatalf("фильтр по заказу: %+v", resp)
	}

	for _, q := range []string{"page=0", "size=0", "size=101", "page=x"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/internal/v1/notifications/logs?"+q, nil))
		if rec.Code != 400 || rec.Header().Get("Content-Type") != "application/problem+json" {
			t.Errorf("%s: ожидался 400 problem+json, получили %d", q, rec.Code)
		}
	}
}
