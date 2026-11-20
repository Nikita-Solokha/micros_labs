package app

import (
	"fmt"
	"time"
)

// Статусы заказа (enum из order-service.yaml).
const (
	StatusPendingPayment = "PENDING_PAYMENT"
	StatusPaid           = "PAID"
	StatusAssigned       = "ASSIGNED"
	StatusDelivered      = "DELIVERED"
	StatusCancelled      = "CANCELLED"
)

// Причины отмены.
const (
	ReasonPaymentFailed      = "PAYMENT_FAILED"
	ReasonPaymentUnavailable = "PAYMENT_SERVICE_UNAVAILABLE"
	ReasonCourierNotFound    = "COURIER_NOT_FOUND"
)

// transitions — конечный автомат заказа: из какого статуса в какие можно перейти.
var transitions = map[string][]string{
	StatusPendingPayment: {StatusPaid, StatusCancelled},
	StatusPaid:           {StatusAssigned, StatusCancelled},
	StatusAssigned:       {StatusDelivered},
	StatusDelivered:      {},
	StatusCancelled:      {},
}

// CanTransition проверяет, разрешён ли переход между статусами.
func CanTransition(from, to string) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Order — заказ на доставку. Единственный владелец сущности — order-service.
type Order struct {
	ID                 string    `json:"id"`
	ClientID           string    `json:"-"`
	Status             string    `json:"status"`
	OriginAddress      string    `json:"originAddress"`
	DestinationAddress string    `json:"destinationAddress"`
	Weight             float64   `json:"weight"`
	Price              float64   `json:"price"`
	PaymentID          string    `json:"paymentId,omitempty"`
	PaymentURL         string    `json:"paymentUrl,omitempty"`
	CourierID          string    `json:"courierId,omitempty"`
	CancelReason       string    `json:"cancelReason,omitempty"`
	CreatedAt          time.Time `json:"createdAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

// Тарифы доставки.
const (
	BasePrice   = 200.0 // подача курьера
	PricePerKg  = 100.0
	MaxWeightKg = 50.0
)

// CalcPrice — стоимость доставки: подача + за каждый килограмм.
func CalcPrice(weight float64) float64 {
	return BasePrice + PricePerKg*weight
}

// --- DTO ---

type CreateOrderRequest struct {
	OriginAddress      string  `json:"originAddress"`
	DestinationAddress string  `json:"destinationAddress"`
	Weight             float64 `json:"weight"`
}

func (r CreateOrderRequest) Validate() error {
	switch {
	case r.OriginAddress == "":
		return fmt.Errorf("%w: originAddress обязателен", ErrValidation)
	case r.DestinationAddress == "":
		return fmt.Errorf("%w: destinationAddress обязателен", ErrValidation)
	case r.OriginAddress == r.DestinationAddress:
		return fmt.Errorf("%w: адреса отправления и назначения совпадают", ErrValidation)
	case r.Weight <= 0:
		return fmt.Errorf("%w: вес посылки должен быть больше 0", ErrValidation)
	case r.Weight > MaxWeightKg:
		return fmt.Errorf("%w: вес посылки не может превышать %.0f кг", ErrValidation, MaxWeightKg)
	}
	return nil
}

type OrderListResponse struct {
	Total int     `json:"total"`
	Page  int     `json:"page"`
	Size  int     `json:"size"`
	Items []Order `json:"items"`
}

type StatusResponse struct {
	OrderID string `json:"orderId"`
	Status  string `json:"status"`
}

type DeliveredRequest struct {
	CourierID string `json:"courierId"`
}

// --- payload событий ---

type OrderCreatedPayload = Order

type OrderDeliveredPayload struct {
	OrderID     string    `json:"orderId"`
	CourierID   string    `json:"courierId"`
	DeliveredAt time.Time `json:"deliveredAt"`
}

type PaymentPayload struct {
	PaymentID string  `json:"paymentId"`
	OrderID   string  `json:"orderId"`
	Amount    float64 `json:"amount"`
	Reason    string  `json:"reason"`
}

type CourierAssignedPayload struct {
	OrderID   string `json:"orderId"`
	CourierID string `json:"courierId"`
}

type SearchFailedPayload struct {
	OrderID string `json:"orderId"`
	Reason  string `json:"reason"`
}
