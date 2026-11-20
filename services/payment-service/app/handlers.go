package app

import (
	"errors"
	"net/http"

	"github.com/Nikita-Solokha/micros_labs/pkg/httpx"
	"github.com/Nikita-Solokha/micros_labs/pkg/idempotency"
	"github.com/Nikita-Solokha/micros_labs/pkg/problem"
)

// Routes регистрирует эндпоинты из payment-service.yaml.
func Routes(s *Service, idem *idempotency.Store) http.Handler {
	mux := http.NewServeMux()

	mux.Handle("POST /internal/v1/payments", idem.Middleware(http.HandlerFunc(s.handleCreate)))
	mux.HandleFunc("POST /api/v1/payments/webhook", s.handleWebhook)
	mux.HandleFunc("POST /internal/v1/payments/{paymentId}/refund", s.handleRefund)
	mux.HandleFunc("GET /internal/v1/payments/{paymentId}", s.handleGet)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "UP"})
	})

	return mux
}

func (s *Service) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req CreatePaymentRequest
	if p := httpx.DecodeJSON(r, &req); p != nil {
		problem.Write(w, r, p)
		return
	}
	pay, created, err := s.CreatePayment(r.Context(), req)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	httpx.WriteJSON(w, status, PaymentResponse{PaymentID: pay.ID, PaymentURL: pay.PaymentURL, Status: pay.Status})
}

func (s *Service) handleWebhook(w http.ResponseWriter, r *http.Request) {
	var req WebhookRequest
	if p := httpx.DecodeJSON(r, &req); p != nil {
		problem.Write(w, r, p)
		return
	}
	if err := s.HandleWebhook(r.Context(), req); err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"received": true})
}

func (s *Service) handleRefund(w http.ResponseWriter, r *http.Request) {
	var req RefundRequest
	if p := httpx.DecodeJSON(r, &req); p != nil {
		problem.Write(w, r, p)
		return
	}
	pay, err := s.Refund(r.Context(), r.PathValue("paymentId"), req.Reason)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, RefundResponse{RefundID: pay.RefundID, Status: pay.Status})
}

// handleGet — текущее состояние платежа (нужно для проверки возврата в саге).
func (s *Service) handleGet(w http.ResponseWriter, r *http.Request) {
	p, ok := s.Store.Get(r.PathValue("paymentId"))
	if !ok {
		problem.Write(w, r, problem.NotFound(ErrNotFound.Error()))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// writeErr переводит ошибки бизнес-логики в HTTP-коды и Problem Details.
func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrValidation):
		problem.Write(w, r, problem.BadRequest(err.Error()))
	case errors.Is(err, ErrBadSignature):
		problem.Write(w, r, problem.New(http.StatusBadRequest, "invalid-signature", "Неверная подпись вебхука", err.Error()))
	case errors.Is(err, ErrNotFound):
		problem.Write(w, r, problem.NotFound(err.Error()))
	case errors.Is(err, ErrWrongState):
		problem.Write(w, r, problem.Conflict(err.Error()))
	case errors.Is(err, ErrGatewayUnavailable):
		problem.Write(w, r, problem.Unavailable(err.Error()))
	default:
		problem.Write(w, r, problem.Internal(err.Error()))
	}
}
