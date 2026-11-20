package app

import (
	"sync"
)

// Store — собственное хранилище payment-service. По условию лабы допускается
// in-memory; в лабе 3 здесь PostgreSQL. Доступ только через методы, поэтому
// замена на БД затронет один этот файл.
type Store struct {
	mu      sync.RWMutex
	byID    map[string]*Payment
	byOrder map[string]string // orderId -> paymentId
}

func NewStore() *Store {
	return &Store{byID: map[string]*Payment{}, byOrder: map[string]string{}}
}

func (s *Store) Save(p Payment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := p
	s.byID[p.ID] = &cp
	s.byOrder[p.OrderID] = p.ID
}

func (s *Store) Get(id string) (Payment, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.byID[id]
	if !ok {
		return Payment{}, false
	}
	return *p, true
}

func (s *Store) GetByOrder(orderID string) (Payment, bool) {
	s.mu.RLock()
	id, ok := s.byOrder[orderID]
	s.mu.RUnlock()
	if !ok {
		return Payment{}, false
	}
	return s.Get(id)
}

func (s *Store) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.byID[id]; ok {
		delete(s.byOrder, p.OrderID)
		delete(s.byID, id)
	}
}

// Update атомарно меняет платёж: fn получает текущую версию и может её изменить.
// Если fn вернула ошибку — изменения не сохраняются.
func (s *Store) Update(id string, fn func(p *Payment) error) (Payment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byID[id]
	if !ok {
		return Payment{}, ErrNotFound
	}
	cp := *p
	if err := fn(&cp); err != nil {
		return *p, err
	}
	s.byID[id] = &cp
	return cp, nil
}
