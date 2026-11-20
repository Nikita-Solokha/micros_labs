package app

import (
	"errors"
	"net/http"

	"github.com/Nikita-Solokha/micros_labs/pkg/httpx"
	"github.com/Nikita-Solokha/micros_labs/pkg/problem"
)

func Routes(s *Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/couriers/shifts", s.handleShift)
	mux.HandleFunc("POST /api/v1/dispatch/location", s.handleLocation)
	mux.HandleFunc("GET /api/v1/couriers/{courierId}/assignment", s.handleAssignment)
	mux.HandleFunc("POST /api/v1/couriers/{courierId}/assignment/complete", s.handleComplete)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "UP"})
	})
	return mux
}

func (s *Service) handleShift(w http.ResponseWriter, r *http.Request) {
	var req ShiftRequest
	if p := httpx.DecodeJSON(r, &req); p != nil {
		problem.Write(w, r, p)
		return
	}
	c, err := s.Shift(req)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ShiftResponse{CourierID: c.ID, Status: c.Status})
}

func (s *Service) handleLocation(w http.ResponseWriter, r *http.Request) {
	var req LocationRequest
	if p := httpx.DecodeJSON(r, &req); p != nil {
		problem.Write(w, r, p)
		return
	}
	if _, err := s.UpdateLocation(req); err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"success": true})
}

func (s *Service) handleAssignment(w http.ResponseWriter, r *http.Request) {
	a, err := s.CurrentAssignment(r.PathValue("courierId"))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, AssignmentResponse{OrderID: a.OrderID, AssignedAt: a.AssignedAt})
}

func (s *Service) handleComplete(w http.ResponseWriter, r *http.Request) {
	a, err := s.CompleteDelivery(r.Context(), r.PathValue("courierId"))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"orderId": a.OrderID, "status": "DELIVERED"})
}

func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrValidation):
		problem.Write(w, r, problem.BadRequest(err.Error()))
	case errors.Is(err, ErrCourierNotFound), errors.Is(err, ErrNoAssignment):
		problem.Write(w, r, problem.NotFound(err.Error()))
	case errors.Is(err, ErrConflict), errors.Is(err, ErrOrderRejected):
		problem.Write(w, r, problem.Conflict(err.Error()))
	case errors.Is(err, ErrOrderUnavailable):
		problem.Write(w, r, problem.Unavailable(err.Error()))
	default:
		problem.Write(w, r, problem.Internal(err.Error()))
	}
}
