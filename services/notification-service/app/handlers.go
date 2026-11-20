package app

import (
	"net/http"
	"strconv"

	"github.com/Nikita-Solokha/micros_labs/pkg/httpx"
	"github.com/Nikita-Solokha/micros_labs/pkg/problem"
)

func Routes(s *Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /internal/v1/notifications/logs", s.handleLogs)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "UP"})
	})
	return mux
}

func (s *Service) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, err1 := intOr(q.Get("page"), 1)
	size, err2 := intOr(q.Get("size"), 50)
	if err1 != nil || err2 != nil {
		problem.Write(w, r, problem.BadRequest("page и size должны быть целыми числами"))
		return
	}
	resp, err := s.Logs(q.Get("orderId"), page, size)
	if err != nil {
		problem.Write(w, r, problem.BadRequest(err.Error()))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func intOr(v string, def int) (int, error) {
	if v == "" {
		return def, nil
	}
	return strconv.Atoi(v)
}
