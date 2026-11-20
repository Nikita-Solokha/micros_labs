package app

import (
	"sync"
	"time"
)

// Store — собственное хранилище dispatch-service (in-memory вместо Redis).
// Один мьютекс на всё: назначение курьера должно быть атомарным,
// чтобы один курьер не попал на два заказа одновременно.
type Store struct {
	mu          sync.Mutex
	couriers    map[string]*Courier
	assignments map[string]Assignment // orderId -> назначение
	orders      map[string]OrderInfo
	searches    map[string]Search
}

func NewStore() *Store {
	return &Store{
		couriers:    map[string]*Courier{},
		assignments: map[string]Assignment{},
		orders:      map[string]OrderInfo{},
		searches:    map[string]Search{},
	}
}

func (s *Store) Courier(id string) (Courier, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.couriers[id]
	if !ok {
		return Courier{}, false
	}
	return *c, true
}

// UpdateCourier атомарно меняет курьера; create=true — создать, если нет.
func (s *Store) UpdateCourier(id string, create bool, fn func(c *Courier) error) (Courier, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.couriers[id]
	if !ok {
		if !create {
			return Courier{}, ErrCourierNotFound
		}
		c = &Courier{ID: id, Status: CourierOffline}
	}
	cp := *c
	if err := fn(&cp); err != nil {
		return *c, err
	}
	s.couriers[id] = &cp
	return cp, nil
}

func (s *Store) SaveOrder(o OrderInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orders[o.OrderID] = o
}

func (s *Store) Order(id string) (OrderInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	return o, ok
}

func (s *Store) AddSearch(orderID string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.searches[orderID]; ok {
		return false
	}
	if _, ok := s.assignments[orderID]; ok {
		return false
	}
	s.searches[orderID] = Search{OrderID: orderID, StartedAt: now}
	return true
}

func (s *Store) RemoveSearch(orderID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.searches, orderID)
}

func (s *Store) Searches() []Search {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Search, 0, len(s.searches))
	for _, v := range s.searches {
		out = append(out, v)
	}
	return out
}

// AssignNearest атомарно находит ближайшего свободного курьера со свежими
// координатами в радиусе и закрепляет его за заказом.
func (s *Store) AssignNearest(orderID string, lat, lon, radiusKm float64, fresh time.Time, now time.Time) (Assignment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var best *Courier
	bestDist := radiusKm
	for _, c := range s.couriers {
		if c.Status != CourierOnline || c.LocationAt.Before(fresh) {
			continue
		}
		if d := DistanceKm(lat, lon, c.Lat, c.Lon); d <= bestDist {
			best, bestDist = c, d
		}
	}
	if best == nil {
		return Assignment{}, false
	}
	best.Status = CourierBusy
	best.CurrentOrderID = orderID
	a := Assignment{OrderID: orderID, CourierID: best.ID, AssignedAt: now}
	s.assignments[orderID] = a
	delete(s.searches, orderID)
	return a, true
}

// Unassign отменяет назначение (если заказ оказался отменён) и освобождает курьера.
func (s *Store) Unassign(orderID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.assignments[orderID]
	if !ok {
		return
	}
	delete(s.assignments, orderID)
	if c, ok := s.couriers[a.CourierID]; ok && c.CurrentOrderID == orderID {
		c.Status = CourierOnline
		c.CurrentOrderID = ""
	}
}

func (s *Store) AssignmentByCourier(courierID string) (Assignment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.couriers[courierID]
	if !ok || c.CurrentOrderID == "" {
		return Assignment{}, false
	}
	a, ok := s.assignments[c.CurrentOrderID]
	return a, ok
}
