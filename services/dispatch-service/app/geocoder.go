package app

import "hash/fnv"

// Geocoder превращает адрес в координаты. В реальной системе это внешний
// сервис (Яндекс/2ГИС Геокодер); в лабе — мок.
type Geocoder interface {
	Geocode(address string) (lat, lon float64)
}

// MockGeocoder детерминированно «размещает» адрес внутри прямоугольника
// вокруг центра Ростова-на-Дону: один и тот же адрес всегда даёт одну точку.
type MockGeocoder struct{}

const (
	centerLat = 47.2357
	centerLon = 39.7015
	spreadDeg = 0.05 // ~5 км
)

func (MockGeocoder) Geocode(address string) (float64, float64) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(address))
	v := h.Sum64()
	fx := float64(v&0xffff)/0xffff*2 - 1       // [-1; 1]
	fy := float64((v>>16)&0xffff)/0xffff*2 - 1 // [-1; 1]
	return centerLat + fx*spreadDeg, centerLon + fy*spreadDeg
}
