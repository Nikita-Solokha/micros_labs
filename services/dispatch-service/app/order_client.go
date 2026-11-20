package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Nikita-Solokha/micros_labs/pkg/httpx"
)

// OrderClient — синхронные вызовы order-service:
//   - GetStatus: GET  /internal/v1/orders/{id}/status    (взаимодействие №2)
//   - MarkDelivered: POST /internal/v1/orders/{id}/delivered (взаимодействие №3)
type OrderClient interface {
	GetStatus(ctx context.Context, orderID string) (string, error)
	MarkDelivered(ctx context.Context, orderID, courierID string) error
}

var (
	ErrOrderNotFound    = errors.New("заказ не найден в order-service")
	ErrOrderRejected    = errors.New("order-service отклонил операцию")
	ErrOrderUnavailable = errors.New("order-service недоступен")
)

type HTTPOrderClient struct {
	BaseURL string
	Client  *http.Client
	Retries int
	Backoff time.Duration
}

func NewHTTPOrderClient(baseURL string) *HTTPOrderClient {
	return &HTTPOrderClient{
		BaseURL: baseURL,
		Client:  &http.Client{Timeout: 2 * time.Second},
		Retries: 3,
		Backoff: 200 * time.Millisecond,
	}
}

func (c *HTTPOrderClient) GetStatus(ctx context.Context, orderID string) (string, error) {
	var out struct {
		Status string `json:"status"`
	}
	err := c.do(ctx, http.MethodGet, "/internal/v1/orders/"+orderID+"/status", nil, &out)
	return out.Status, err
}

func (c *HTTPOrderClient) MarkDelivered(ctx context.Context, orderID, courierID string) error {
	return c.do(ctx, http.MethodPost, "/internal/v1/orders/"+orderID+"/delivered",
		map[string]string{"courierId": courierID}, nil)
}

// do — запрос с таймаутом и повторами при сетевых ошибках и 5xx.
func (c *HTTPOrderClient) do(ctx context.Context, method, path string, in, out any) error {
	var lastErr error
	for attempt := 1; attempt <= c.Retries; attempt++ {
		retry, err := c.once(ctx, method, path, in, out)
		if err == nil || !retry {
			return err
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.Backoff * time.Duration(attempt)):
		}
	}
	return fmt.Errorf("%w: %v", ErrOrderUnavailable, lastErr)
}

func (c *HTTPOrderClient) once(ctx context.Context, method, path string, in, out any) (bool, error) {
	var body *bytes.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	} else {
		body = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return false, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if id := httpx.RequestID(ctx); id != "" {
		req.Header.Set(httpx.RequestIDHeader, id)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return true, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, ErrOrderNotFound
	case resp.StatusCode >= 500:
		return true, fmt.Errorf("order-service ответил %d", resp.StatusCode)
	case resp.StatusCode >= 400:
		return false, fmt.Errorf("%w: код %d", ErrOrderRejected, resp.StatusCode)
	}
	if out != nil {
		return false, json.NewDecoder(resp.Body).Decode(out)
	}
	return false, nil
}
