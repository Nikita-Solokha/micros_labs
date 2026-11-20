package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Gateway — внешний платёжный шлюз (банк). В лабе он замокан:
// HTTPGateway ходит в mocks/bank-mock, а в unit-тестах используется FakeGateway.
type Gateway interface {
	// CreateSession регистрирует платёж в банке и возвращает ссылку на оплату.
	CreateSession(ctx context.Context, paymentID string, amount float64, callbackURL string) (string, error)
	// Refund возвращает деньги клиенту, возвращает id возврата.
	Refund(ctx context.Context, paymentID string, amount float64) (string, error)
}

// HTTPGateway — клиент к банку по HTTP с таймаутом.
type HTTPGateway struct {
	BaseURL string
	Client  *http.Client
}

func NewHTTPGateway(baseURL string) *HTTPGateway {
	return &HTTPGateway{BaseURL: baseURL, Client: &http.Client{Timeout: 3 * time.Second}}
}

func (g *HTTPGateway) post(ctx context.Context, path string, in, out any) error {
	body, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.Client.Do(req)
	if err != nil {
		return fmt.Errorf("банк недоступен: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("банк ответил %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (g *HTTPGateway) CreateSession(ctx context.Context, paymentID string, amount float64, callbackURL string) (string, error) {
	var out struct {
		PaymentURL string `json:"paymentUrl"`
	}
	err := g.post(ctx, "/sessions", map[string]any{
		"paymentId": paymentID, "amount": amount, "callbackUrl": callbackURL,
	}, &out)
	return out.PaymentURL, err
}

func (g *HTTPGateway) Refund(ctx context.Context, paymentID string, amount float64) (string, error) {
	var out struct {
		RefundID string `json:"refundId"`
	}
	err := g.post(ctx, "/refunds", map[string]any{"paymentId": paymentID, "amount": amount}, &out)
	return out.RefundID, err
}
