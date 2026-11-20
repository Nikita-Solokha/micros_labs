// Package idempotency защищает от повторов:
//   - Middleware — для HTTP-операций с заголовком Idempotency-Key;
//   - Processed — для событий из брокера (дедупликация по eventId).
package idempotency

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"sync"

	"github.com/Nikita-Solokha/micros_labs/pkg/problem"
)

const Header = "Idempotency-Key"

// record — сохранённый ответ на первый запрос с данным ключом.
type record struct {
	requestHash string
	done        bool // false = первый запрос ещё обрабатывается
	status      int
	header      http.Header
	body        []byte
}

// Store хранит ключи идемпотентности в памяти сервиса.
type Store struct {
	mu   sync.Mutex
	recs map[string]*record
}

func NewStore() *Store { return &Store{recs: make(map[string]*record)} }

// Middleware реализует правила Idempotency-Key:
//   - нет ключа → 400;
//   - ключ новый → выполняем запрос и запоминаем ответ;
//   - тот же ключ и то же тело → возвращаем сохранённый ответ, повторно ничего не создаём;
//   - тот же ключ, но другое тело → 409;
//   - первый запрос с этим ключом ещё выполняется → 409.
func (s *Store) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(Header)
		if key == "" {
			problem.Write(w, r, problem.BadRequest("заголовок Idempotency-Key обязателен"))
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			problem.Write(w, r, problem.BadRequest("не удалось прочитать тело запроса"))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		sum := sha256.Sum256(append([]byte(r.Method+" "+r.URL.Path+"\n"), body...))
		hash := hex.EncodeToString(sum[:])

		// Ключ уникален в рамках пути, чтобы один UUID не пересекался между ручками.
		storeKey := r.URL.Path + "|" + key

		s.mu.Lock()
		rec, ok := s.recs[storeKey]
		if ok {
			s.mu.Unlock()
			switch {
			case rec.requestHash != hash:
				problem.Write(w, r, problem.Conflict("Idempotency-Key уже использован с другим телом запроса"))
			case !rec.done:
				problem.Write(w, r, problem.Conflict("запрос с этим Idempotency-Key ещё обрабатывается"))
			default:
				for k, v := range rec.header {
					w.Header()[k] = v
				}
				w.Header().Set("Idempotent-Replayed", "true")
				w.WriteHeader(rec.status)
				_, _ = w.Write(rec.body)
			}
			return
		}
		rec = &record{requestHash: hash}
		s.recs[storeKey] = rec
		s.mu.Unlock()

		cw := &captureWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(cw, r)

		s.mu.Lock()
		defer s.mu.Unlock()
		if cw.status >= 500 {
			// Ошибку сервера не запоминаем: клиент может повторить с тем же ключом.
			delete(s.recs, storeKey)
			return
		}
		rec.done = true
		rec.status = cw.status
		rec.header = w.Header().Clone()
		rec.body = cw.buf.Bytes()
	})
}

// captureWriter пишет ответ клиенту и одновременно копирует его для сохранения.
type captureWriter struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (c *captureWriter) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	c.buf.Write(b)
	return c.ResponseWriter.Write(b)
}

// Processed — множество уже обработанных eventId. Брокер может доставить
// событие повторно, и обработчик должен его проигнорировать.
type Processed struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

func NewProcessed() *Processed { return &Processed{seen: make(map[string]struct{})} }

// Seen возвращает true, если событие уже обрабатывалось.
func (p *Processed) Seen(eventID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.seen[eventID]
	return ok
}

// Mark отмечает событие обработанным. Вызывать после успешной обработки.
func (p *Processed) Mark(eventID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen[eventID] = struct{}{}
}
