package events

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// MemoryBus — брокер в памяти процесса. Используется в тестах и для запуска
// без Kafka. Повторяет главные свойства Kafka, важные для лабы:
//   - каждая consumer group получает своё сообщение;
//   - при ошибке обработчика — повторная доставка (retry);
//   - после исчерпания попыток сообщение уходит в dead letter queue.
type MemoryBus struct {
	mu      sync.RWMutex
	subs    map[string]map[string]Handler // topic -> group -> handler
	dlq     []Envelope
	log     *slog.Logger
	Retries int
	Backoff time.Duration
	wg      sync.WaitGroup
}

func NewMemoryBus(log *slog.Logger) *MemoryBus {
	return &MemoryBus{
		subs:    make(map[string]map[string]Handler),
		log:     log,
		Retries: 3,
		Backoff: 50 * time.Millisecond,
	}
}

func (b *MemoryBus) Subscribe(topic, group string, h Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs[topic] == nil {
		b.subs[topic] = make(map[string]Handler)
	}
	b.subs[topic][group] = h
}

// Publish доставляет событие всем группам асинхронно — как настоящий брокер,
// издатель не ждёт, пока подписчики его обработают.
func (b *MemoryBus) Publish(_ context.Context, topic, _ string, e Envelope) error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	b.log.Info("event published", "topic", topic, "type", e.EventType, "eventId", e.EventID, "correlationId", e.CorrelationID)
	for group, h := range b.subs[topic] {
		b.wg.Add(1)
		go b.deliver(topic, group, h, e)
	}
	return nil
}

func (b *MemoryBus) deliver(topic, group string, h Handler, e Envelope) {
	defer b.wg.Done()
	for attempt := 1; attempt <= b.Retries; attempt++ {
		err := h(context.Background(), e)
		if err == nil {
			return
		}
		b.log.Warn("event handler failed", "topic", topic, "group", group, "eventId", e.EventID, "attempt", attempt, "err", err)
		time.Sleep(b.Backoff * time.Duration(attempt))
	}
	b.mu.Lock()
	b.dlq = append(b.dlq, e)
	b.mu.Unlock()
	b.log.Error("event moved to DLQ", "topic", topic, "group", group, "eventId", e.EventID)
}

// Start и Close нужны для интерфейса Bus; в памяти запускать нечего.
func (b *MemoryBus) Start(context.Context) {}
func (b *MemoryBus) Close() error          { b.wg.Wait(); return nil }

// Wait ждёт, пока все отправленные события будут обработаны (нужно в тестах).
func (b *MemoryBus) Wait() { b.wg.Wait() }

// DLQ возвращает копию сообщений, которые не удалось обработать.
func (b *MemoryBus) DLQ() []Envelope {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]Envelope(nil), b.dlq...)
}
