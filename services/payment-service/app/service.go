package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Nikita-Solokha/micros_labs/pkg/events"
	"github.com/Nikita-Solokha/micros_labs/pkg/httpx"
	"github.com/Nikita-Solokha/micros_labs/pkg/idempotency"
	"github.com/Nikita-Solokha/micros_labs/pkg/webhook"
)

var (
	ErrNotFound           = errors.New("платёж не найден")
	ErrBadSignature       = errors.New("неверная подпись вебхука")
	ErrValidation         = errors.New("ошибка валидации")
	ErrWrongState         = errors.New("недопустимый статус платежа")
	ErrGatewayUnavailable = errors.New("платёжный шлюз недоступен")
)

// Service — бизнес-логика платежей. Не знает про HTTP, поэтому легко тестируется.
type Service struct {
	Store         *Store
	Gateway       Gateway
	Bus           events.Publisher
	Processed     *idempotency.Processed
	WebhookSecret string
	CallbackURL   string // куда банк присылает вебхук
	Log           *slog.Logger
}

// CreatePayment — шаг саги: order-service просит создать платёж.
// Если для заказа уже есть живой платёж — возвращаем его (идемпотентность по orderId).
func (s *Service) CreatePayment(ctx context.Context, req CreatePaymentRequest) (Payment, bool, error) {
	if req.OrderID == "" {
		return Payment{}, false, fmt.Errorf("%w: orderId обязателен", ErrValidation)
	}
	if req.Amount <= 0 {
		return Payment{}, false, fmt.Errorf("%w: amount должен быть больше 0", ErrValidation)
	}
	if p, ok := s.Store.GetByOrder(req.OrderID); ok && p.Status != StatusFailed {
		return p, false, nil
	}

	now := time.Now().UTC()
	p := Payment{
		ID:        httpx.NewID(),
		OrderID:   req.OrderID,
		Amount:    req.Amount,
		Status:    StatusPending,
		CreatedAt: now,
		UpdatedAt: now,
	}
	url, err := s.Gateway.CreateSession(ctx, p.ID, p.Amount, s.CallbackURL)
	if err != nil {
		s.Log.Error("gateway create session failed", "paymentId", p.ID, "err", err)
		return Payment{}, false, fmt.Errorf("%w: %v", ErrGatewayUnavailable, err)
	}
	p.PaymentURL = url
	s.Store.Save(p)
	s.Log.Info("payment created", "paymentId", p.ID, "orderId", p.OrderID, "amount", p.Amount)
	return p, true, nil
}

// HandleWebhook — банк сообщил результат оплаты. Публикует payment.completed
// или payment.failed. Повторный вебхук для уже завершённого платежа игнорируется.
func (s *Service) HandleWebhook(ctx context.Context, req WebhookRequest) error {
	if req.Status != "SUCCESS" && req.Status != "FAILED" {
		return fmt.Errorf("%w: status должен быть SUCCESS или FAILED", ErrValidation)
	}
	if !webhook.Verify(s.WebhookSecret, req.PaymentID, req.Status, req.Signature) {
		return ErrBadSignature
	}

	changed := false
	p, err := s.Store.Update(req.PaymentID, func(p *Payment) error {
		if p.Status != StatusPending {
			return nil // уже обработан — повтор вебхука
		}
		changed = true
		if req.Status == "SUCCESS" {
			p.Status = StatusCompleted
		} else {
			p.Status = StatusFailed
		}
		p.UpdatedAt = time.Now().UTC()
		return nil
	})
	if err != nil {
		return err
	}
	if !changed {
		s.Log.Info("duplicate webhook ignored", "paymentId", p.ID, "status", p.Status)
		return nil
	}

	if p.Status == StatusCompleted {
		return s.publish(ctx, events.PaymentCompleted, p.OrderID, PaymentCompletedPayload{
			PaymentID: p.ID, OrderID: p.OrderID, Amount: p.Amount,
		})
	}
	return s.publish(ctx, events.PaymentFailed, p.OrderID, PaymentFailedPayload{
		PaymentID: p.ID, OrderID: p.OrderID, Reason: "DECLINED_BY_BANK",
	})
}

// Refund — компенсирующее действие саги. Повторный вызов для уже
// возвращённого платежа отдаёт тот же результат.
func (s *Service) Refund(ctx context.Context, paymentID, reason string) (Payment, error) {
	if reason == "" {
		return Payment{}, fmt.Errorf("%w: reason обязателен", ErrValidation)
	}
	p, ok := s.Store.Get(paymentID)
	if !ok {
		return Payment{}, ErrNotFound
	}
	switch p.Status {
	case StatusRefunded:
		return p, nil
	case StatusCompleted:
	default:
		return p, fmt.Errorf("%w: вернуть можно только оплаченный платёж, текущий статус %s", ErrWrongState, p.Status)
	}

	refundID, err := s.Gateway.Refund(ctx, p.ID, p.Amount)
	if err != nil {
		s.Log.Error("gateway refund failed", "paymentId", p.ID, "err", err)
		return p, fmt.Errorf("%w: %v", ErrGatewayUnavailable, err)
	}
	p, err = s.Store.Update(p.ID, func(p *Payment) error {
		p.Status = StatusRefunded
		p.RefundID = refundID
		p.UpdatedAt = time.Now().UTC()
		return nil
	})
	if err != nil {
		return p, err
	}
	s.Log.Info("payment refunded", "paymentId", p.ID, "orderId", p.OrderID, "reason", reason)
	return p, s.publish(ctx, events.PaymentRefunded, p.OrderID, PaymentRefundedPayload{
		PaymentID: p.ID, OrderID: p.OrderID, Amount: p.Amount, Reason: reason,
	})
}

// OnSearchFailed — обработчик события dispatch.search_failed:
// курьер не найден → возвращаем деньги (компенсация).
func (s *Service) OnSearchFailed(ctx context.Context, e events.Envelope) error {
	if s.Processed.Seen(e.EventID) {
		s.Log.Info("duplicate event ignored", "eventId", e.EventID)
		return nil
	}
	var pl SearchFailedPayload
	if err := e.Decode(&pl); err != nil {
		s.Log.Error("bad event payload", "eventId", e.EventID, "err", err)
		s.Processed.Mark(e.EventID) // битое сообщение повторять бесполезно
		return nil
	}
	p, ok := s.Store.GetByOrder(pl.OrderID)
	if !ok {
		s.Log.Warn("no payment for order, nothing to refund", "orderId", pl.OrderID)
		s.Processed.Mark(e.EventID)
		return nil
	}
	reason := pl.Reason
	if reason == "" {
		reason = "COURIER_NOT_FOUND_TIMEOUT"
	}
	if _, err := s.Refund(ctx, p.ID, reason); err != nil {
		if errors.Is(err, ErrWrongState) {
			s.Processed.Mark(e.EventID)
			return nil
		}
		return err // шлюз недоступен — брокер доставит событие повторно
	}
	s.Processed.Mark(e.EventID)
	return nil
}

func (s *Service) publish(ctx context.Context, eventType, orderID string, payload any) error {
	e, err := events.New(eventType, orderID, payload)
	if err != nil {
		return err
	}
	return s.Bus.Publish(ctx, events.TopicPayments, orderID, e)
}
