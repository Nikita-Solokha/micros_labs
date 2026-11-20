package app

import (
	"sort"
	"sync"
)

// Store — собственное хранилище order-service (in-memory; в лабе 3 — PostgreSQL).
type Store struct {
	mu     sync.RWMutex
	orders map[string]*Order
}

func NewStore() *Store { return &Store{orders: map[string]*Order{}} }

func (s *Store) Save(o Order) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := o
	s.orders[o.ID] = &cp
}

func (s *Store) Get(id string) (Order, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.orders[id]
	if !ok {
		return Order{}, false
	}
	return *o, true
}

// Update атомарно меняет заказ. Если fn вернула ошибку — изменения не сохраняются.
func (s *Store) Update(id string, fn func(o *Order) error) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return Order{}, ErrNotFound
	}
	cp := *o
	if err := fn(&cp); err != nil {
		return *o, err
	}
	s.orders[id] = &cp
	return cp, nil
}

// ListByClient — заказы клиента, новые сначала, с пагинацией page/size.
func (s *Store) ListByClient(clientID string, page, size int) ([]Order, int) {
	s.mu.RLock()
	var all []Order
	for _, o := range s.orders {
		if o.ClientID == clientID {
			all = append(all, *o)
		}
	}
	s.mu.RUnlock()

	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })
	total := len(all)
	from := (page - 1) * size
	if from >= total {
		return []Order{}, total
	}
	to := from + size
	if to > total {
		to = total
	}
	return all[from:to], total
}
