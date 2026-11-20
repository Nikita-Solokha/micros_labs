// bank-mock — заглушка внешнего платёжного шлюза (банка).
//
//	POST /sessions          — зарегистрировать платёж, вернуть ссылку на оплату
//	GET  /pay/{paymentId}   — «страница оплаты»: ?result=success (по умолчанию) или ?result=fail;
//	                          банк отправляет подписанный вебхук в payment-service
//	POST /refunds           — возврат денег
//
// Переменные окружения: PORT (9090), PUBLIC_URL, WEBHOOK_SECRET, FAIL_REFUNDS=true —
// чтобы показать недоступность банка при возврате.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/Nikita-Solokha/micros_labs/pkg/httpx"
	"github.com/Nikita-Solokha/micros_labs/pkg/problem"
	"github.com/Nikita-Solokha/micros_labs/pkg/webhook"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type session struct {
	PaymentID   string  `json:"paymentId"`
	Amount      float64 `json:"amount"`
	CallbackURL string  `json:"callbackUrl"`
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "bank-mock")
	port := env("PORT", "9090")
	publicURL := env("PUBLIC_URL", "http://localhost:"+port)
	secret := env("WEBHOOK_SECRET", "dev-secret")
	failRefunds := env("FAIL_REFUNDS", "false") == "true"

	var mu sync.Mutex
	sessions := map[string]session{}
	client := &http.Client{Timeout: 3 * time.Second}

	mux := http.NewServeMux()

	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		var s session
		if p := httpx.DecodeJSON(r, &s); p != nil {
			problem.Write(w, r, p)
			return
		}
		mu.Lock()
		sessions[s.PaymentID] = s
		mu.Unlock()
		httpx.WriteJSON(w, http.StatusCreated, map[string]string{
			"paymentUrl": publicURL + "/pay/" + s.PaymentID,
		})
	})

	mux.HandleFunc("GET /pay/{paymentId}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("paymentId")
		mu.Lock()
		s, ok := sessions[id]
		mu.Unlock()
		if !ok {
			problem.Write(w, r, problem.NotFound("платёжная сессия не найдена"))
			return
		}
		status := "SUCCESS"
		if r.URL.Query().Get("result") == "fail" {
			status = "FAILED"
		}
		body, _ := json.Marshal(map[string]string{
			"paymentId": id,
			"status":    status,
			"signature": webhook.Sign(secret, id, status),
		})
		resp, err := client.Post(s.CallbackURL, "application/json", bytes.NewReader(body))
		if err != nil {
			problem.Write(w, r, problem.Unavailable("не удалось отправить вебхук: "+err.Error()))
			return
		}
		resp.Body.Close()
		log.Info("webhook sent", "paymentId", id, "status", status, "callbackStatus", resp.StatusCode)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "Банк-заглушка: платёж %s, результат %s (вебхук: %d)\n", id, status, resp.StatusCode)
	})

	mux.HandleFunc("POST /refunds", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			PaymentID string  `json:"paymentId"`
			Amount    float64 `json:"amount"`
		}
		if p := httpx.DecodeJSON(r, &req); p != nil {
			problem.Write(w, r, p)
			return
		}
		if failRefunds {
			problem.Write(w, r, problem.Unavailable("банк временно недоступен (FAIL_REFUNDS=true)"))
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"refundId": httpx.NewID()})
	})

	log.Info("listening", "port", port)
	if err := http.ListenAndServe(":"+port, httpx.Middleware(log, mux)); err != nil {
		log.Error("server failed", "err", err)
		os.Exit(1)
	}
}
