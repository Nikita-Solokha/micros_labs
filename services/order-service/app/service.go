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
)

var (
	ErrNotFound   = errors.New("заказ не найден")
	ErrValidation = errors.New("ошибка валидации")
	ErrWrongState = errors.New("недопустимый переход статуса")
	// ErrNotReady — событие пришло раньше, чем заказ к нему готов
	// (например, курьер назначен, а payment.completed ещё не обработан).
	// Обработчик возвращает ошибку, брокер доставит событие повторно.
	ErrNotReady = errors.New("заказ ещё не готов к этому событию")
)

type Service struct {
	Store     *Store
	Payments  PaymentClient
	Bus       events.Publisher
	Processed *idempotency.Processed
	Log       *slog.Logger
}

// CreateOrder — первый шаг саги:
//  1. сохраняем заказ в PENDING_PAYMENT;
//  2. синхронно просим payment-service создать платёж;
//  3. если payment недоступен — компенсация: заказ CANCELLED;
//  4. публикуем order.created.
func (s *Service) CreateOrder(ctx context.Context, clientID string, req CreateOrderRequest) (Order, error) {
	if err := req.Validate(); err != nil {
		return Order{}, err
	}
	now := time.Now().UTC()
	o := Order{
		ID:                 httpx.NewID(),
		ClientID:           clientID,
		Status:             StatusPendingPayment,
		OriginAddress:      req.OriginAddress,
		DestinationAddress: req.DestinationAddress,
		Weight:             req.Weight,
		Price:              CalcPrice(req.Weight),
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	s.Store.Save(o)

	pay, err := s.Payments.CreatePayment(ctx, o.ID, o.Price)
	if err != nil {
		s.Log.Error("payment creation failed, cancelling order", "orderId", o.ID, "err", err)
		_, _ = s.Store.Update(o.ID, func(o *Order) error {
			o.Status = StatusCancelled
			o.CancelReason = ReasonPaymentUnavailable
			o.UpdatedAt = time.Now().UTC()
			return nil
		})
		return Order{}, err
	}

	o, _ = s.Store.Update(o.ID, func(o *Order) error {
		o.PaymentID = pay.PaymentID
		o.PaymentURL = pay.PaymentURL
		return nil
	})
	s.Log.Info("order created", "orderId", o.ID, "price", o.Price, "paymentId", o.PaymentID)

	if err := s.publish(ctx, events.OrderCreated, o.ID, o); err != nil {
		s.Log.Error("publish order.created failed", "orderId", o.ID, "err", err)
	}
	return o, nil
}

func (s *Service) GetOrder(id string) (Order, error) {
	o, ok := s.Store.Get(id)
	if !ok {
		return Order{}, ErrNotFound
	}
	return o, nil
}

func (s *Service) ListOrders(clientID string, page, size int) (OrderListResponse, error) {
	if page < 1 {
		return OrderListResponse{}, fmt.Errorf("%w: page должен быть >= 1", ErrValidation)
	}
	if size < 1 || size > 100 {
		return OrderListResponse{}, fmt.Errorf("%w: size должен быть от 1 до 100", ErrValidation)
	}
	items, total := s.Store.ListByClient(clientID, page, size)
	return OrderListResponse{Total: total, Page: page, Size: size, Items: items}, nil
}

// MarkDelivered — вызывается dispatch-service, когда курьер отметил доставку.
// Повторный вызов для доставленного заказа возвращает успех (идемпотентно).
func (s *Service) MarkDelivered(ctx context.Context, orderID, courierID string) (Order, error) {
	if courierID == "" {
		return Order{}, fmt.Errorf("%w: courierId обязателен", ErrValidation)
	}
	changed := false
	o, err := s.Store.Update(orderID, func(o *Order) error {
		if o.Status == StatusDelivered {
			return nil
		}
		if o.CourierID != "" && o.CourierID != courierID {
			return fmt.Errorf("%w: заказ назначен другому курьеру", ErrWrongState)
		}
		if err := transit(o, StatusDelivered); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil || !changed {
		return o, err
	}
	s.Log.Info("order delivered", "orderId", o.ID, "courierId", courierID)
	err = s.publish(ctx, events.OrderDelivered, o.ID, OrderDeliveredPayload{
		OrderID: o.ID, CourierID: courierID, DeliveredAt: o.UpdatedAt,
	})
	return o, err
}

// ---------- обработчики событий (асинхронные шаги саги) ----------

// HandleEvent — общая точка входа для событий из payments.events и dispatch.events.
func (s *Service) HandleEvent(ctx context.Context, e events.Envelope) error {
	if s.Processed.Seen(e.EventID) {
		s.Log.Info("duplicate event ignored", "eventId", e.EventID, "type", e.EventType)
		return nil
	}
	var err error
	switch e.EventType {
	case events.PaymentCompleted:
		err = s.onPaymentCompleted(e)
	case events.PaymentFailed:
		err = s.onCancel(e, ReasonPaymentFailed)
	case events.CourierAssigned:
		err = s.onCourierAssigned(e)
	case events.CourierSearchFail:
		err = s.onCancel(e, ReasonCourierNotFound)
	default:
		return nil // остальные события order-service не интересуют
	}

	switch {
	case err == nil, errors.Is(err, ErrWrongState), errors.Is(err, ErrNotFound):
		// успех или событие, которое уже неприменимо — повторять бессмысленно
		if err != nil {
			s.Log.Warn("event skipped", "eventId", e.EventID, "type", e.EventType, "err", err)
		}
		s.Processed.Mark(e.EventID)
		return nil
	default:
		return err // ErrNotReady и прочее — пусть брокер повторит
	}
}

func (s *Service) onPaymentCompleted(e events.Envelope) error {
	var p PaymentPayload
	if err := e.Decode(&p); err != nil {
		return fmt.Errorf("%w: битый payload", ErrWrongState)
	}
	o, err := s.Store.Update(p.OrderID, func(o *Order) error {
		if o.Status == StatusPaid {
			return nil
		}
		return transit(o, StatusPaid)
	})
	if err == nil {
		s.Log.Info("order paid", "orderId", o.ID)
	}
	return err
}

func (s *Service) onCourierAssigned(e events.Envelope) error {
	var p CourierAssignedPayload
	if err := e.Decode(&p); err != nil {
		return fmt.Errorf("%w: битый payload", ErrWrongState)
	}
	o, err := s.Store.Update(p.OrderID, func(o *Order) error {
		if o.Status == StatusPendingPayment {
			return ErrNotReady
		}
		if err := transit(o, StatusAssigned); err != nil {
			return err
		}
		o.CourierID = p.CourierID
		return nil
	})
	if err == nil {
		s.Log.Info("courier assigned", "orderId", o.ID, "courierId", o.CourierID)
	}
	return err
}

// onCancel — компенсация: заказ отменяется (оплата отклонена или курьер не найден).
func (s *Service) onCancel(e events.Envelope, reason string) error {
	var p struct {
		OrderID string `json:"orderId"`
	}
	if err := e.Decode(&p); err != nil {
		return fmt.Errorf("%w: битый payload", ErrWrongState)
	}
	o, err := s.Store.Update(p.OrderID, func(o *Order) error {
		if o.Status == StatusCancelled {
			return nil
		}
		if err := transit(o, StatusCancelled); err != nil {
			return err
		}
		o.CancelReason = reason
		return nil
	})
	if err == nil {
		s.Log.Info("order cancelled", "orderId", o.ID, "reason", reason)
	}
	return err
}

// transit меняет статус, проверяя конечный автомат.
func transit(o *Order, to string) error {
	if !CanTransition(o.Status, to) {
		return fmt.Errorf("%w: %s -> %s", ErrWrongState, o.Status, to)
	}
	o.Status = to
	o.UpdatedAt = time.Now().UTC()
	return nil
}

func (s *Service) publish(ctx context.Context, eventType, orderID string, payload any) error {
	e, err := events.New(eventType, orderID, payload)
	if err != nil {
		return err
	}
	return s.Bus.Publish(ctx, events.TopicOrders, orderID, e)
}
