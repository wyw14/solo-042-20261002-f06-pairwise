package circuit

import (
	"errors"
	"sort"
	"sync"
	"time"
)

var ErrCircuitNotFound = errors.New("circuit not found")

type Circuit struct {
	ID                 string       `json:"id"`
	Zone               string       `json:"zone"`
	ValveID            string       `json:"valve_id"`
	Enabled            bool         `json:"enabled"`
	Pressure           float64      `json:"pressure_kpa"`
	PressureRecordedAt *time.Time   `json:"pressure_recorded_at,omitempty"`
	Maintenance        *Maintenance `json:"maintenance,omitempty"`
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
	return copyCircuit(circuit), ok
}

func (r *Registry) List() []Circuit {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Circuit, 0, len(r.circuits))
	for _, circuit := range r.circuits {
		result = append(result, copyCircuit(circuit))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (r *Registry) RecordPressure(id string, pressure float64, observedAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	circuit, ok := r.circuits[id]
	if !ok {
		return ErrCircuitNotFound
	}
	recorded := observedAt.UTC()
	circuit.Pressure = pressure
	circuit.PressureRecordedAt = &recorded
	r.circuits[id] = circuit
	return nil
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

func (r *Registry) setMaintenance(id string, maintenance Maintenance) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	circuit, ok := r.circuits[id]
	if !ok {
		return ErrCircuitNotFound
	}
	circuit.Maintenance = &maintenance
	r.circuits[id] = circuit
	return nil
}

func (r *Registry) clearMaintenance(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	circuit, ok := r.circuits[id]
	if !ok {
		return ErrCircuitNotFound
	}
	circuit.Maintenance = nil
	r.circuits[id] = circuit
	return nil
}

func (r *Registry) recordRejection(id string, rejection Rejection) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	circuit, ok := r.circuits[id]
	if !ok {
		return ErrCircuitNotFound
	}
	if circuit.Maintenance == nil {
		return ErrNotUnderMaintenance
	}
	maintenance := *circuit.Maintenance
	maintenance.LastRejection = &rejection
	circuit.Maintenance = &maintenance
	r.circuits[id] = circuit
	return nil
}

func copyCircuit(circuit Circuit) Circuit {
	if circuit.Maintenance != nil {
		maintenance := *circuit.Maintenance
		if maintenance.LastRejection != nil {
			rejection := *maintenance.LastRejection
			maintenance.LastRejection = &rejection
		}
		circuit.Maintenance = &maintenance
	}
	if circuit.PressureRecordedAt != nil {
		recorded := *circuit.PressureRecordedAt
		circuit.PressureRecordedAt = &recorded
	}
	return circuit
}
