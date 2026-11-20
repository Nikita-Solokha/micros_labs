package app

import "time"

// Статусы платежа (enum из payment-service.yaml).
const (
	StatusPending   = "PENDING"
	StatusCompleted = "COMPLETED"
	StatusFailed    = "FAILED"
	StatusRefunded  = "REFUNDED"
)

// Payment — платёж. Единственный владелец этой сущности — payment-service.
type Payment struct {
	ID         string    `json:"paymentId"`
	OrderID    string    `json:"orderId"`
	Amount     float64   `json:"amount"`
	Status     string    `json:"status"`
	PaymentURL string    `json:"paymentUrl"`
	RefundID   string    `json:"refundId,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// --- DTO запросов и ответов (по контракту) ---

type CreatePaymentRequest struct {
	OrderID string  `json:"orderId"`
	Amount  float64 `json:"amount"`
}

type PaymentResponse struct {
	PaymentID  string `json:"paymentId"`
	PaymentURL string `json:"paymentUrl"`
	Status     string `json:"status"`
}

type WebhookRequest struct {
	PaymentID string `json:"paymentId"`
	Status    string `json:"status"` // SUCCESS | FAILED
	Signature string `json:"signature"`
}

type RefundRequest struct {
	Reason string `json:"reason"`
}

type RefundResponse struct {
	RefundID string `json:"refundId"`
	Status   string `json:"status"`
}

// --- payload публикуемых событий ---

type PaymentCompletedPayload struct {
	PaymentID string  `json:"paymentId"`
	OrderID   string  `json:"orderId"`
	Amount    float64 `json:"amount"`
}

type PaymentFailedPayload struct {
	PaymentID string `json:"paymentId"`
	OrderID   string `json:"orderId"`
	Reason    string `json:"reason"`
}

type PaymentRefundedPayload struct {
	PaymentID string  `json:"paymentId"`
	OrderID   string  `json:"orderId"`
	Amount    float64 `json:"amount"`
	Reason    string  `json:"reason"`
}

// --- payload события, на которое подписан сервис ---

type SearchFailedPayload struct {
	OrderID string `json:"orderId"`
	Reason  string `json:"reason"`
}
