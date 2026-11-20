package app

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/Nikita-Solokha/micros_labs/pkg/events"
	"github.com/Nikita-Solokha/micros_labs/pkg/httpx"
	"github.com/Nikita-Solokha/micros_labs/pkg/idempotency"
)

var ErrValidation = errors.New("ошибка валидации")

// LogItem — запись журнала отправок (NotificationLogItem из notification-service.yaml).
type LogItem struct {
	ID        string    `json:"id"`
	Recipient string    `json:"recipient"`
	Message   string    `json:"message"`
	Channel   string    `json:"channel"`
	SentAt    time.Time `json:"sentAt"`
	OrderID   string    `json:"orderId"`
	EventType string    `json:"eventType"`
}

type LogResponse struct {
	Total int       `json:"total"`
	Page  int       `json:"page"`
	Size  int       `json:"size"`
	Items []LogItem `json:"items"`
}

// ---------- SMS-провайдер (внешняя система, замокана) ----------

type SMSProvider interface {
	Send(ctx context.Context, to, text string) error
}

// MockSMS «отправляет» SMS в лог. Fail=true имитирует недоступность провайдера.
type MockSMS struct {
	Log  *slog.Logger
	mu   sync.Mutex
	Fail bool
}

func (m *MockSMS) SetFail(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Fail = v
}

func (m *MockSMS) Send(_ context.Context, to, text string) error {
	m.mu.Lock()
	fail := m.Fail
	m.mu.Unlock()
	if fail {
		return errors.New("SMS-провайдер недоступен")
	}
	m.Log.Info("SMS sent (mock)", "to", to, "text", text)
	return nil
}

// ---------- хранилище журнала ----------

// Store — собственное хранилище (в лабе 3 — PostgreSQL JSONB, здесь in-memory).
type Store struct {
	mu    sync.RWMutex
	items []LogItem
}

func (s *Store) Add(it LogItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, it)
}

// List — новые сначала, необязательный фильтр по заказу, пагинация.
func (s *Store) List(orderID string, page, size int) ([]LogItem, int) {
	s.mu.RLock()
	var all []LogItem
	for _, it := range s.items {
		if orderID == "" || it.OrderID == orderID {
			all = append(all, it)
		}
	}
	s.mu.RUnlock()
	sort.SliceStable(all, func(i, j int) bool { return all[i].SentAt.After(all[j].SentAt) })
	total := len(all)
	from := (page - 1) * size
	if from >= total {
		return []LogItem{}, total
	}
	to := min(from+size, total)
	return all[from:to], total
}

// ---------- сервис ----------

type Service struct {
	Store     *Store
	SMS       SMSProvider
	Processed *idempotency.Processed
	Log       *slog.Logger
}

// ClientPhone — номер клиента. Профиля клиента в системе нет (это отдельный
// сервис, не входящий в лабу), поэтому номер детерминированно выводится из orderId.
func ClientPhone(orderID string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(orderID))
	return fmt.Sprintf("+7999%07d", h.Sum64()%10_000_000)
}

// MessageFor — текст SMS для события; "" — на это событие SMS не шлём.
func MessageFor(e events.Envelope) (orderID, text string) {
	var p struct {
		OrderID   string `json:"orderId"`
		CourierID string `json:"courierId"`
	}
	_ = e.Decode(&p)
	short := p.OrderID
	if len(short) > 8 {
		short = short[:8]
	}
	switch e.EventType {
	case events.CourierAssigned:
		return p.OrderID, fmt.Sprintf("Заказ %s: курьер назначен и уже едет за посылкой.", short)
	case events.CourierSearchFail:
		return p.OrderID, fmt.Sprintf("Заказ %s отменён: свободный курьер не найден. Деньги будут возвращены на карту.", short)
	case events.OrderDelivered:
		return p.OrderID, fmt.Sprintf("Заказ %s доставлен. Спасибо, что пользуетесь нашим сервисом!", short)
	}
	return "", ""
}

// HandleEvent — подписчик на dispatch.events и orders.events.
// Ошибка SMS-провайдера возвращается брокеру (повтор, затем DLQ), но
// на остальные сервисы это никак не влияет — в этом смысл асинхронности.
func (s *Service) HandleEvent(ctx context.Context, e events.Envelope) error {
	if s.Processed.Seen(e.EventID) {
		s.Log.Info("duplicate event ignored", "eventId", e.EventID)
		return nil
	}
	orderID, text := MessageFor(e)
	if text == "" || orderID == "" {
		return nil
	}
	to := ClientPhone(orderID)
	if err := s.SMS.Send(ctx, to, text); err != nil {
		s.Log.Error("SMS send failed", "orderId", orderID, "eventType", e.EventType, "err", err)
		return err
	}
	s.Store.Add(LogItem{
		ID: httpx.NewID(), Recipient: to, Message: text, Channel: "SMS",
		SentAt: time.Now().UTC(), OrderID: orderID, EventType: e.EventType,
	})
	s.Processed.Mark(e.EventID)
	return nil
}

func (s *Service) Logs(orderID string, page, size int) (LogResponse, error) {
	if page < 1 {
		return LogResponse{}, fmt.Errorf("%w: page должен быть >= 1", ErrValidation)
	}
	if size < 1 || size > 100 {
		return LogResponse{}, fmt.Errorf("%w: size должен быть от 1 до 100", ErrValidation)
	}
	items, total := s.Store.List(orderID, page, size)
	return LogResponse{Total: total, Page: page, Size: size, Items: items}, nil
}
