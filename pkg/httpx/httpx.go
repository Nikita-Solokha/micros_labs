// Package httpx — общие HTTP-помощники: JSON-ответы, разбор тела запроса
// и middleware (request id, логирование, перехват паник).
package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/Nikita-Solokha/micros_labs/pkg/problem"
)

const maxBodySize = 1 << 20 // 1 МБ

// WriteJSON отправляет ответ в формате JSON с указанным статусом.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// DecodeJSON читает тело запроса в dst. Неизвестные поля и мусор после JSON
// считаются ошибкой. Возвращает готовый Problem (400), который можно сразу отдать клиенту.
func DecodeJSON(r *http.Request, dst any) *problem.Problem {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBodySize))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return problem.BadRequest("пустое тело запроса")
		}
		return problem.BadRequest(fmt.Sprintf("некорректный JSON: %v", err))
	}
	if dec.More() {
		return problem.BadRequest("в теле запроса должен быть один JSON-объект")
	}
	return nil
}

// ---------- request id ----------

type ctxKey struct{}

// RequestIDHeader — заголовок, по которому id запроса проходит через все сервисы.
const RequestIDHeader = "X-Request-Id"

// RequestID достаёт id текущего запроса из контекста.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// NewID генерирует случайный UUID v4 без внешних библиотек.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // версия 4
	b[8] = (b[8] & 0x3f) | 0x80 // вариант RFC 4122
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// ---------- middleware ----------

// statusRecorder запоминает код ответа, чтобы записать его в лог.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Middleware оборачивает обработчик: проставляет X-Request-Id, логирует
// каждый запрос и превращает панику в ответ 500 в формате Problem.
func Middleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		id := r.Header.Get(RequestIDHeader)
		if id == "" {
			id = NewID()
		}
		w.Header().Set(RequestIDHeader, id)
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, id))

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		defer func() {
			if err := recover(); err != nil {
				log.Error("panic", "err", err, "requestId", id)
				problem.Write(rec, r, problem.Internal("непредвиденная ошибка"))
			}
			log.Info("http",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration", time.Since(start).String(),
				"requestId", id,
			)
		}()

		next.ServeHTTP(rec, r)
	})
}
