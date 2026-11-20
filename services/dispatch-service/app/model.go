package app

import (
	"fmt"
	"math"
	"time"
)

// Статусы курьера (enum из dispatch-service.yaml).
const (
	CourierOnline  = "ONLINE"
	CourierBusy    = "BUSY"
	CourierOffline = "OFFLINE"
)

const (
	ActionStartShift = "START_SHIFT"
	ActionEndShift   = "END_SHIFT"

	ReasonSearchTimeout = "COURIER_NOT_FOUND_TIMEOUT"
	ReasonOrderGone     = "ORDER_NOT_ACTIVE"
)

// Courier — курьер. В лабе 3 геопозиции хранились в Redis с TTL;
// здесь то же поведение: координаты старше LocationTTL считаются неактуальными.
type Courier struct {
	ID             string    `json:"courierId"`
	Status         string    `json:"status"`
	Lat            float64   `json:"lat"`
	Lon            float64   `json:"lon"`
	LocationAt     time.Time `json:"locationAt"`
	CurrentOrderID string    `json:"currentOrderId,omitempty"`
}

// Assignment — назначение курьера на заказ.
type Assignment struct {
	OrderID    string    `json:"orderId"`
	CourierID  string    `json:"courierId"`
	AssignedAt time.Time `json:"assignedAt"`
}

// OrderInfo — локальная копия нужных dispatch данных заказа.
// Обновляется по событию order.created (владелец данных — order-service).
type OrderInfo struct {
	OrderID       string
	OriginAddress string
	PickupLat     float64
	PickupLon     float64
}

// Search — заказ, для которого идёт поиск курьера.
type Search struct {
	OrderID   string
	StartedAt time.Time
}

// --- DTO ---

type ShiftRequest struct {
	CourierID string `json:"courierId"`
	Action    string `json:"action"`
}

type ShiftResponse struct {
	CourierID string `json:"courierId"`
	Status    string `json:"status"`
}

type LocationRequest struct {
	CourierID string  `json:"courierId"`
	Lat       float64 `json:"lat"`
	Lon       float64 `json:"lon"`
	Status    string  `json:"status"`
}

func (r LocationRequest) Validate() error {
	switch {
	case r.CourierID == "":
		return fmt.Errorf("%w: courierId обязателен", ErrValidation)
	case r.Lat < -90 || r.Lat > 90:
		return fmt.Errorf("%w: широта должна быть в диапазоне от -90 до 90", ErrValidation)
	case r.Lon < -180 || r.Lon > 180:
		return fmt.Errorf("%w: долгота должна быть в диапазоне от -180 до 180", ErrValidation)
	case r.Status != CourierOnline && r.Status != CourierBusy && r.Status != CourierOffline:
		return fmt.Errorf("%w: status должен быть ONLINE, BUSY или OFFLINE", ErrValidation)
	}
	return nil
}

type AssignmentResponse struct {
	OrderID    string    `json:"orderId"`
	AssignedAt time.Time `json:"assignedAt"`
}

// --- payload событий ---

type OrderCreatedPayload struct {
	ID            string `json:"id"`
	OriginAddress string `json:"originAddress"`
}

type PaymentCompletedPayload struct {
	OrderID string `json:"orderId"`
}

type CourierAssignedPayload struct {
	OrderID   string `json:"orderId"`
	CourierID string `json:"courierId"`
}

type SearchFailedPayload struct {
	OrderID string `json:"orderId"`
	Reason  string `json:"reason"`
}

// DistanceKm — расстояние между точками по формуле гаверсинусов
// (то же, что считает Redis GEORADIUS).
func DistanceKm(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371.0
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLon := (lon2 - lon1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * r * math.Asin(math.Sqrt(a))
}
