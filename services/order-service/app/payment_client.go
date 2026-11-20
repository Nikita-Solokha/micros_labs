package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/Nikita-Solokha/micros_labs/pkg/httpx"
	"github.com/Nikita-Solokha/micros_labs/pkg/idempotency"
)

// PaymentClient — синхронный вызов payment-service (взаимодействие №1 из лабы 3).
type PaymentClient interface {
	CreatePayment(ctx context.Context, orderID string, amount float64) (PaymentInfo, error)
}

type PaymentInfo struct {
	PaymentID  string `json:"paymentId"`
	PaymentURL string `json:"paymentUrl"`
	Status     string `json:"status"`
}

var ErrPaymentUnavailable = errors.New("payment-service недоступен")

// HTTPPaymentClient: таймаут на каждый запрос, повторы с паузой и circuit breaker.
// Idempotency-Key = orderId, поэтому повтор запроса никогда не создаст второй платёж.
type HTTPPaymentClient struct {
	BaseURL string
	Client  *http.Client
	Retries int
	Backoff time.Duration
	Breaker *Breaker
}

func NewHTTPPaymentClient(baseURL string) *HTTPPaymentClient {
	return &HTTPPaymentClient{
		BaseURL: baseURL,
		Client:  &http.Client{Timeout: 2 * time.Second},
		Retries: 3,
		Backoff: 200 * time.Millisecond,
		Breaker: NewBreaker(5, 15*time.Second),
	}
}

func (c *HTTPPaymentClient) CreatePayment(ctx context.Context, orderID string, amount float64) (PaymentInfo, error) {
	if !c.Breaker.Allow() {
		return PaymentInfo{}, fmt.Errorf("%w: circuit breaker открыт", ErrPaymentUnavailable)
	}
	body, _ := json.Marshal(map[string]any{"orderId": orderID, "amount": amount})

	var lastErr error
	for attempt := 1; attempt <= c.Retries; attempt++ {
		info, retry, err := c.try(ctx, orderID, body)
		if err == nil {
			c.Breaker.Success()
			return info, nil
		}
		lastErr = err
		if !retry {
			break
		}
		select {
		case <-ctx.Done():
			return PaymentInfo{}, ctx.Err()
		case <-time.After(c.Backoff * time.Duration(attempt)):
		}
	}
	c.Breaker.Failure()
	return PaymentInfo{}, fmt.Errorf("%w: %v", ErrPaymentUnavailable, lastErr)
}

// try делает один запрос. retry=true — ошибку имеет смысл повторить (сеть, 5xx).
func (c *HTTPPaymentClient) try(ctx context.Context, orderID string, body []byte) (PaymentInfo, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/internal/v1/payments", bytes.NewReader(body))
	if err != nil {
		return PaymentInfo{}, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(idempotency.Header, orderID)
	if id := httpx.RequestID(ctx); id != "" {
		req.Header.Set(httpx.RequestIDHeader, id) // сквозной id запроса
	}

	resp, err := c.Client.Do(req)
	if err != nil {
		return PaymentInfo{}, true, err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK:
		var info PaymentInfo
		if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
			return PaymentInfo{}, false, fmt.Errorf("некорректный ответ payment-service: %w", err)
		}
		return info, false, nil
	case resp.StatusCode >= 500:
		return PaymentInfo{}, true, fmt.Errorf("payment-service ответил %d", resp.StatusCode)
	default:
		return PaymentInfo{}, false, fmt.Errorf("payment-service отклонил запрос: %d", resp.StatusCode)
	}
}

// Breaker — простейший circuit breaker: после N неудач подряд перестаёт
// звать сервис на время cooldown и сразу отвечает ошибкой. Так order-service
// не копит зависшие запросы, пока payment-service лежит (защита от каскадного отказа).
type Breaker struct {
	mu        sync.Mutex
	failures  int
	threshold int
	cooldown  time.Duration
	openUntil time.Time
}

func NewBreaker(threshold int, cooldown time.Duration) *Breaker {
	return &Breaker{threshold: threshold, cooldown: cooldown}
}

func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Now().After(b.openUntil)
}

func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
}

func (b *Breaker) Failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	if b.failures >= b.threshold {
		b.openUntil = time.Now().Add(b.cooldown)
		b.failures = 0
	}
}
