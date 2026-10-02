package circuit

import (
	"errors"
	"sort"
	"sync"
	"time"
)

var ErrCircuitNotFound = errors.New("circuit not found")

type Circuit struct {
	ID       string  `json:"id"`
	Zone     string  `json:"zone"`
	ValveID  string  `json:"valve_id"`
	Enabled  bool    `json:"enabled"`
	Pressure float64 `json:"pressure_kpa"`
	// PressureRecordedAt is zero until the first pressure reading is
	// recorded through RecordPressure.
	PressureRecordedAt time.Time `json:"pressure_recorded_at,omitempty"`
}

type Registry struct {
	mu       sync.RWMutex
	circuits map[string]Circuit
}

func NewRegistry(circuits ...Circuit) *Registry {
	r := &Registry{circuits: make(map[string]Circuit)}
	for _, circuit := range circuits {
		if circuit.ID != "" {
			r.circuits[circuit.ID] = circuit
		}
	}
	return r
}

func (r *Registry) Get(id string) (Circuit, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	circuit, ok := r.circuits[id]
	return circuit, ok
}

func (r *Registry) List() []Circuit {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Circuit, 0, len(r.circuits))
	for _, circuit := range r.circuits {
		result = append(result, circuit)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// RecordPressure registers a pressure reading for the circuit. Readings are
// accepted both in and out of service: a deactivated circuit must be able to
// report that pressure has bled down before restore is allowed.
func (r *Registry) RecordPressure(id string, pressure float64, observedAt time.Time) (Circuit, error) {
	if pressure < 0 {
		return Circuit{}, errors.New("pressure reading cannot be negative")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	circuit, ok := r.circuits[id]
	if !ok {
		return Circuit{}, ErrCircuitNotFound
	}
	circuit.Pressure = pressure
	circuit.PressureRecordedAt = observedAt.UTC()
	r.circuits[id] = circuit
	return circuit, nil
}

// UpdatePressure keeps compatibility with callers that only have the reading.
func (r *Registry) UpdatePressure(id string, pressure float64) error {
	_, err := r.RecordPressure(id, pressure, time.Time{})
	return err
}

func (r *Registry) SetEnabled(id string, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	circuit, ok := r.circuits[id]
	if !ok {
		return ErrCircuitNotFound
	}
	circuit.Enabled = enabled
	r.circuits[id] = circuit
	return nil
}
