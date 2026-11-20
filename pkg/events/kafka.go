package events

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"
)

// KafkaBus — брокер на Apache Kafka.
//
//   - Publish пишет событие в топик; ключ сообщения = orderId, поэтому все события
//     одного заказа попадают в одну партицию и читаются по порядку.
//   - Subscribe читает топик в составе consumer group (группа = имя сервиса):
//     каждый сервис получает свою копию события.
//   - Offset коммитится только ПОСЛЕ успешной обработки (at-least-once). Поэтому
//     обработчики должны быть идемпотентными — дубли отсекаются по eventId.
//   - Если обработчик упал Retries раз подряд, сообщение уходит в <topic>.dlq.
type KafkaBus struct {
	brokers []string
	writer  *kafka.Writer
	log     *slog.Logger

	Retries int
	Backoff time.Duration

	mu      sync.Mutex
	subs    []subscription
	readers []*kafka.Reader
	wg      sync.WaitGroup
}

type subscription struct {
	topic, group string
	h            Handler
}

func NewKafkaBus(brokers []string, log *slog.Logger) *KafkaBus {
	return &KafkaBus{
		brokers: brokers,
		log:     log,
		Retries: 5,
		Backoff: 500 * time.Millisecond,
		writer: &kafka.Writer{
			Addr:                   kafka.TCP(brokers...),
			Balancer:               &kafka.Hash{}, // партиция по ключу
			RequiredAcks:           kafka.RequireAll,
			BatchTimeout:           10 * time.Millisecond,
			AllowAutoTopicCreation: true,
		},
	}
}

func (b *KafkaBus) Publish(ctx context.Context, topic, key string, e Envelope) error {
	value, err := json.Marshal(e)
	if err != nil {
		return err
	}
	err = b.writer.WriteMessages(ctx, kafka.Message{
		Topic: topic,
		Key:   []byte(key),
		Value: value,
		Headers: []kafka.Header{
			{Key: "eventType", Value: []byte(e.EventType)},
			{Key: "correlationId", Value: []byte(e.CorrelationID)},
		},
	})
	if err != nil {
		b.log.Error("kafka publish failed", "topic", topic, "type", e.EventType, "err", err)
		return err
	}
	b.log.Info("event published", "topic", topic, "type", e.EventType, "eventId", e.EventID, "correlationId", e.CorrelationID)
	return nil
}

// Subscribe только регистрирует обработчик; чтение начинается в Start.
func (b *KafkaBus) Subscribe(topic, group string, h Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs = append(b.subs, subscription{topic, group, h})
}

// Start запускает по горутине-читателю на каждую подписку.
func (b *KafkaBus) Start(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.subs {
		r := kafka.NewReader(kafka.ReaderConfig{
			Brokers:     b.brokers,
			GroupID:     s.group,
			Topic:       s.topic,
			MinBytes:    1,
			MaxBytes:    10e6,
			MaxWait:     500 * time.Millisecond,
			StartOffset: kafka.FirstOffset,
		})
		b.readers = append(b.readers, r)
		b.wg.Add(1)
		go b.consume(ctx, r, s)
		b.log.Info("subscribed", "topic", s.topic, "group", s.group)
	}
}

func (b *KafkaBus) consume(ctx context.Context, r *kafka.Reader, s subscription) {
	defer b.wg.Done()
	for {
		m, err := r.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return
			}
			b.log.Warn("kafka fetch failed", "topic", s.topic, "err", err)
			time.Sleep(time.Second)
			continue
		}

		var e Envelope
		if err := json.Unmarshal(m.Value, &e); err != nil {
			b.log.Error("bad message, moving to DLQ", "topic", s.topic, "offset", m.Offset, "err", err)
			b.toDLQ(ctx, s, m.Key, m.Value)
		} else if !b.handle(ctx, s, e) {
			b.toDLQ(ctx, s, m.Key, m.Value)
		}

		if err := r.CommitMessages(ctx, m); err != nil && ctx.Err() == nil {
			b.log.Error("kafka commit failed", "topic", s.topic, "offset", m.Offset, "err", err)
		}
	}
}

// handle вызывает обработчик с повторами. false — все попытки неудачны.
func (b *KafkaBus) handle(ctx context.Context, s subscription, e Envelope) bool {
	for attempt := 1; attempt <= b.Retries; attempt++ {
		err := s.h(ctx, e)
		if err == nil {
			return true
		}
		b.log.Warn("event handler failed", "topic", s.topic, "group", s.group, "type", e.EventType,
			"eventId", e.EventID, "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return true // выключаемся: не коммитим в DLQ, сообщение перечитается после рестарта
		case <-time.After(b.Backoff * time.Duration(attempt)):
		}
	}
	return false
}

func (b *KafkaBus) toDLQ(ctx context.Context, s subscription, key, value []byte) {
	dlq := s.topic + ".dlq"
	err := b.writer.WriteMessages(ctx, kafka.Message{
		Topic: dlq, Key: key, Value: value,
		Headers: []kafka.Header{{Key: "failedGroup", Value: []byte(s.group)}},
	})
	if err != nil {
		b.log.Error("DLQ write failed", "topic", dlq, "err", err)
		return
	}
	b.log.Error("event moved to DLQ", "topic", dlq, "group", s.group)
}

// Close дожидается читателей (ctx из Start должен быть уже отменён) и закрывает соединения.
func (b *KafkaBus) Close() error {
	b.mu.Lock()
	readers := b.readers
	b.mu.Unlock()
	for _, r := range readers {
		_ = r.Close()
	}
	b.wg.Wait()
	return b.writer.Close()
}
