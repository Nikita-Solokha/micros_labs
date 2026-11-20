package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Nikita-Solokha/micros_labs/pkg/events"
	"github.com/Nikita-Solokha/micros_labs/pkg/idempotency"
)

var (
	ErrValidation      = errors.New("ошибка валидации")
	ErrCourierNotFound = errors.New("курьер не найден (не выходил на смену)")
	ErrConflict        = errors.New("конфликт состояния курьера")
	ErrNoAssignment    = errors.New("активных назначений нет")
	ErrNotReady        = errors.New("данные заказа ещё не получены")
)

type Config struct {
	SearchTimeout  time.Duration // сколько ищем курьера до отмены (по лабе 3 — 15 минут)
	SearchInterval time.Duration // как часто повторяем поиск
	LocationTTL    time.Duration // координаты старше считаются неактуальными
	RadiusKm       float64
}

type Service struct {
	Store     *Store
	Orders    OrderClient
	Geo       Geocoder
	Bus       events.Publisher
	Processed *idempotency.Processed
	Cfg       Config
	Log       *slog.Logger
	Now       func() time.Time // подменяется в тестах
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

// ---------- курьеры ----------

func (s *Service) Shift(req ShiftRequest) (Courier, error) {
	if req.CourierID == "" {
		return Courier{}, fmt.Errorf("%w: courierId обязателен", ErrValidation)
	}
	switch req.Action {
	case ActionStartShift:
		return s.Store.UpdateCourier(req.CourierID, true, func(c *Courier) error {
			if c.Status == CourierOffline {
				c.Status = CourierOnline
			}
			return nil
		})
	case ActionEndShift:
		return s.Store.UpdateCourier(req.CourierID, false, func(c *Courier) error {
			if c.Status == CourierBusy {
				return fmt.Errorf("%w: нельзя закрыть смену с активным заказом", ErrConflict)
			}
			c.Status = CourierOffline
			return nil
		})
	default:
		return Courier{}, fmt.Errorf("%w: action должен быть START_SHIFT или END_SHIFT", ErrValidation)
	}
}

func (s *Service) UpdateLocation(req LocationRequest) (Courier, error) {
	if err := req.Validate(); err != nil {
		return Courier{}, err
	}
	return s.Store.UpdateCourier(req.CourierID, false, func(c *Courier) error {
		if c.Status == CourierOffline {
			return fmt.Errorf("%w: сначала откройте смену", ErrConflict)
		}
		if req.Status == CourierOffline {
			if c.Status == CourierBusy {
				return fmt.Errorf("%w: нельзя уйти офлайн с активным заказом", ErrConflict)
			}
			c.Status = CourierOffline
		}
		// статус BUSY определяет сервер по назначению, а не курьер
		c.Lat, c.Lon, c.LocationAt = req.Lat, req.Lon, s.now()
		return nil
	})
}

func (s *Service) CurrentAssignment(courierID string) (Assignment, error) {
	if _, ok := s.Store.Courier(courierID); !ok {
		return Assignment{}, ErrCourierNotFound
	}
	a, ok := s.Store.AssignmentByCourier(courierID)
	if !ok {
		return Assignment{}, ErrNoAssignment
	}
	return a, nil
}

// CompleteDelivery — курьер отметил «доставлен». Синхронно сообщаем order-service;
// только если он подтвердил — освобождаем курьера.
func (s *Service) CompleteDelivery(ctx context.Context, courierID string) (Assignment, error) {
	a, err := s.CurrentAssignment(courierID)
	if err != nil {
		return Assignment{}, err
	}
	if err := s.Orders.MarkDelivered(ctx, a.OrderID, courierID); err != nil {
		s.Log.Error("mark delivered failed", "orderId", a.OrderID, "err", err)
		return a, err
	}
	s.Store.Unassign(a.OrderID)
	s.Log.Info("delivery completed", "orderId", a.OrderID, "courierId", courierID)
	return a, nil
}

// ---------- события ----------

func (s *Service) HandleEvent(ctx context.Context, e events.Envelope) error {
	if s.Processed.Seen(e.EventID) {
		s.Log.Info("duplicate event ignored", "eventId", e.EventID, "type", e.EventType)
		return nil
	}
	var err error
	switch e.EventType {
	case events.OrderCreated:
		err = s.onOrderCreated(e)
	case events.PaymentCompleted:
		err = s.onPaymentCompleted(e)
	default:
		return nil
	}
	if err != nil {
		return err // ErrNotReady — брокер доставит повторно
	}
	s.Processed.Mark(e.EventID)
	return nil
}

// order.created — сохраняем копию адреса забора и сразу геокодируем.
func (s *Service) onOrderCreated(e events.Envelope) error {
	var p OrderCreatedPayload
	if err := e.Decode(&p); err != nil || p.ID == "" {
		s.Log.Error("bad order.created payload", "eventId", e.EventID)
		return nil
	}
	lat, lon := s.Geo.Geocode(p.OriginAddress)
	s.Store.SaveOrder(OrderInfo{OrderID: p.ID, OriginAddress: p.OriginAddress, PickupLat: lat, PickupLon: lon})
	return nil
}

// payment.completed — заказ оплачен, начинаем поиск курьера.
func (s *Service) onPaymentCompleted(e events.Envelope) error {
	var p PaymentCompletedPayload
	if err := e.Decode(&p); err != nil || p.OrderID == "" {
		s.Log.Error("bad payment.completed payload", "eventId", e.EventID)
		return nil
	}
	if _, ok := s.Store.Order(p.OrderID); !ok {
		return ErrNotReady
	}
	if s.Store.AddSearch(p.OrderID, s.now()) {
		s.Log.Info("courier search started", "orderId", p.OrderID)
	}
	s.TryAssign(context.Background(), p.OrderID)
	return nil
}

// ---------- подбор курьера ----------

// TryAssign пытается назначить курьера на заказ. Перед назначением синхронно
// проверяет у order-service, что заказ ещё активен.
func (s *Service) TryAssign(ctx context.Context, orderID string) bool {
	o, ok := s.Store.Order(orderID)
	if !ok {
		return false
	}
	status, err := s.Orders.GetStatus(ctx, orderID)
	switch {
	case errors.Is(err, ErrOrderNotFound) || status == "CANCELLED" || status == "DELIVERED":
		s.Log.Warn("order is not active, search stopped", "orderId", orderID, "status", status)
		s.Store.RemoveSearch(orderID)
		return false
	case err != nil:
		s.Log.Warn("order-service unavailable, will retry", "orderId", orderID, "err", err)
		return false
	}

	fresh := s.now().Add(-s.Cfg.LocationTTL)
	a, ok := s.Store.AssignNearest(orderID, o.PickupLat, o.PickupLon, s.Cfg.RadiusKm, fresh, s.now())
	if !ok {
		return false
	}
	s.Log.Info("courier assigned", "orderId", orderID, "courierId", a.CourierID)
	if err := s.publish(ctx, events.CourierAssigned, orderID, CourierAssignedPayload{OrderID: orderID, CourierID: a.CourierID}); err != nil {
		s.Log.Error("publish courier_assigned failed", "orderId", orderID, "err", err)
	}
	return true
}

// Tick — один проход фонового поиска: пробуем назначить, а по истечении
// SearchTimeout публикуем dispatch.search_failed (запуск компенсаций саги).
func (s *Service) Tick(ctx context.Context) {
	for _, sr := range s.Store.Searches() {
		if s.now().Sub(sr.StartedAt) >= s.Cfg.SearchTimeout {
			s.Store.RemoveSearch(sr.OrderID)
			s.Log.Warn("courier search timeout", "orderId", sr.OrderID, "after", s.Cfg.SearchTimeout.String())
			if err := s.publish(ctx, events.CourierSearchFail, sr.OrderID,
				SearchFailedPayload{OrderID: sr.OrderID, Reason: ReasonSearchTimeout}); err != nil {
				s.Log.Error("publish search_failed failed", "orderId", sr.OrderID, "err", err)
			}
			continue
		}
		s.TryAssign(ctx, sr.OrderID)
	}
}

// Run крутит Tick с интервалом SearchInterval до остановки сервиса.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(s.Cfg.SearchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Tick(ctx)
		}
	}
}

func (s *Service) publish(ctx context.Context, eventType, orderID string, payload any) error {
	e, err := events.New(eventType, orderID, payload)
	if err != nil {
		return err
	}
	return s.Bus.Publish(ctx, events.TopicDispatch, orderID, e)
}
