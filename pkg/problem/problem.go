// Package problem реализует единую модель ошибок для всех сервисов
// по RFC 7807 (Problem Details for HTTP APIs).
package problem

import (
	"encoding/json"
	"net/http"
)

// Базовый адрес для поля type — совпадает с примерами в OpenAPI-спеках.
const typeBase = "https://api.delivery.com/errors/"

// Problem — тело ответа об ошибке (Content-Type: application/problem+json).
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
}

// Error позволяет возвращать Problem как обычную Go-ошибку.
func (p *Problem) Error() string {
	if p.Detail != "" {
		return p.Title + ": " + p.Detail
	}
	return p.Title
}

// New создаёт Problem. code — короткий код ошибки, попадает в конец поля type.
func New(status int, code, title, detail string) *Problem {
	return &Problem{
		Type:   typeBase + code,
		Title:  title,
		Status: status,
		Detail: detail,
	}
}

// Готовые конструкторы для частых случаев.

func BadRequest(detail string) *Problem {
	return New(http.StatusBadRequest, "bad-request", "Неверные параметры запроса", detail)
}

func NotFound(detail string) *Problem {
	return New(http.StatusNotFound, "not-found", "Ресурс не найден", detail)
}

func Conflict(detail string) *Problem {
	return New(http.StatusConflict, "conflict", "Конфликт состояния", detail)
}

func Internal(detail string) *Problem {
	return New(http.StatusInternalServerError, "internal", "Внутренняя ошибка сервиса", detail)
}

func Unavailable(detail string) *Problem {
	return New(http.StatusServiceUnavailable, "unavailable", "Зависимый сервис недоступен", detail)
}

// Write отправляет Problem клиенту. В поле instance подставляется путь запроса.
func Write(w http.ResponseWriter, r *http.Request, p *Problem) {
	if p.Instance == "" && r != nil {
		p.Instance = r.URL.Path
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}
