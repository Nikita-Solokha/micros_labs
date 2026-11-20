// Package events — формат асинхронных сообщений и интерфейс брокера.
// Сервисы работают только с интерфейсами Publisher/Subscriber, поэтому
// in-memory шину (для тестов) можно заменить на Kafka без правки бизнес-логики.
package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Nikita-Solokha/micros_labs/pkg/httpx"
)

// Топики — как в README лабы 3.
const (
	TopicOrders   = "orders.events"
	TopicPayments = "payments.events"
	TopicDispatch = "dispatch.events"
)

// Типы событий.
const (
	OrderCreated      = "order.created"
	OrderDelivered    = "order.delivered"
	PaymentCompleted  = "payment.completed"
	PaymentFailed     = "payment.failed"
	PaymentRefunded   = "payment.refunded"
	CourierAssigned   = "dispatch.courier_assigned"
	CourierSearchFail = "dispatch.search_failed"
)

// Envelope — «конверт» любого события: служебные поля + полезная нагрузка.
type Envelope struct {
	EventID       string          `json:"eventId"`
	EventType     string          `json:"eventType"`
	SchemaVersion int             `json:"schemaVersion"`
	OccurredAt    time.Time       `json:"occurredAt"`
	CorrelationID string          `json:"correlationId"` // = orderId, связывает шаги саги
	Payload       json.RawMessage `json:"payload"`
}

// New собирает событие: генерирует eventId, ставит время и сериализует payload.
func New(eventType, correlationID string, payload any) (Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		EventID:       httpx.NewID(),
		EventType:     eventType,
		SchemaVersion: 1,
		OccurredAt:    time.Now().UTC(),
		CorrelationID: correlationID,
		Payload:       raw,
	}, nil
}

// Decode разбирает payload события в нужную структуру.
func (e Envelope) Decode(dst any) error {
	return json.Unmarshal(e.Payload, dst)
}

// Handler обрабатывает одно событие. Ошибка = событие не обработано,
// брокер доставит его повторно.
type Handler func(ctx context.Context, e Envelope) error

// Publisher публикует событие в топик. key задаёт партицию в Kafka
// (используем orderId, чтобы события одного заказа шли по порядку).
type Publisher interface {
	Publish(ctx context.Context, topic, key string, e Envelope) error
}

// Subscriber подписывает обработчик на топик. group — имя consumer group:
// каждый сервис получает свою копию события.
type Subscriber interface {
	Subscribe(topic, group string, h Handler)
}

// Bus — брокер, который умеет и публиковать, и подписывать.
type Bus interface {
	Publisher
	Subscriber
}
