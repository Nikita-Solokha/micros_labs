package app

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/Nikita-Solokha/micros_labs/pkg/httpx"
	"github.com/Nikita-Solokha/micros_labs/pkg/idempotency"
	"github.com/Nikita-Solokha/micros_labs/pkg/problem"
)

// ClientHeader — id клиента. В реальной системе его проставляет API Gateway
// после проверки JWT; в лабе его передаёт сам клиент.
const ClientHeader = "X-Client-Id"

func Routes(s *Service, idem *idempotency.Store) http.Handler {
	mux := http.NewServeMux()

	// публичные (через API Gateway)
	mux.Handle("POST /api/v1/orders", idem.Middleware(http.HandlerFunc(s.handleCreate)))
	mux.HandleFunc("GET /api/v1/orders", s.handleList)
	mux.HandleFunc("GET /api/v1/orders/{orderId}", s.handleGet)

	// внутренние (для других сервисов)
	mux.HandleFunc("GET /internal/v1/orders/{orderId}/status", s.handleStatus)
	mux.HandleFunc("POST /internal/v1/orders/{orderId}/delivered", s.handleDelivered)

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "UP"})
	})
	return mux
}

func clientID(r *http.Request) string {
	if id := r.Header.Get(ClientHeader); id != "" {
		return id
	}
	return "anonymous"
}

func (s *Service) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req CreateOrderRequest
	if p := httpx.DecodeJSON(r, &req); p != nil {
		problem.Write(w, r, p)
		return
	}
	o, err := s.CreateOrder(r.Context(), clientID(r), req)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, o)
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	page, err1 := queryInt(r, "page", 1)
	size, err2 := queryInt(r, "size", 20)
	if err1 != nil || err2 != nil {
		problem.Write(w, r, problem.BadRequest("page и size должны быть целыми числами"))
		return
	}
	resp, err := s.ListOrders(clientID(r), page, size)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (s *Service) handleGet(w http.ResponseWriter, r *http.Request) {
	o, err := s.GetOrder(r.PathValue("orderId"))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, o)
}

func (s *Service) handleStatus(w http.ResponseWriter, r *http.Request) {
	o, err := s.GetOrder(r.PathValue("orderId"))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, StatusResponse{OrderID: o.ID, Status: o.Status})
}

func (s *Service) handleDelivered(w http.ResponseWriter, r *http.Request) {
	var req DeliveredRequest
	if p := httpx.DecodeJSON(r, &req); p != nil {
		problem.Write(w, r, p)
		return
	}
	o, err := s.MarkDelivered(r.Context(), r.PathValue("orderId"), req.CourierID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, StatusResponse{OrderID: o.ID, Status: o.Status})
}

func queryInt(r *http.Request, key string, def int) (int, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def, nil
	}
	return strconv.Atoi(v)
}

func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrValidation):
		problem.Write(w, r, problem.BadRequest(err.Error()))
	case errors.Is(err, ErrNotFound):
		problem.Write(w, r, problem.NotFound(err.Error()))
	case errors.Is(err, ErrWrongState):
		problem.Write(w, r, problem.Conflict(err.Error()))
	case errors.Is(err, ErrPaymentUnavailable):
		problem.Write(w, r, problem.Unavailable(err.Error()))
	default:
		problem.Write(w, r, problem.Internal(err.Error()))
	}
}
