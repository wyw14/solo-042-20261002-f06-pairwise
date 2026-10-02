package circuit

import "time"

type Health struct {
	CircuitID string `json:"circuit_id"`
	// State describes operational status: available, disabled,
	// maintenance, maintenance-pending-isolation, reserved or
	// fault-retained.
	State       string       `json:"state"`
	Available   bool         `json:"available"`
	Reason      string       `json:"reason,omitempty"`
	Maintenance *Maintenance `json:"maintenance,omitempty"`
	// IsolationPending is true for a deactivated circuit that still has an
	// active refill or an open valve, so it must not be treated as isolated.
	IsolationPending bool `json:"isolation_pending,omitempty"`
	// RestoreRejected carries the most recent restore refusal reason.
	RestoreRejected string    `json:"restore_rejected,omitempty"`
	PressureKPa     float64   `json:"pressure_kpa"`
	CheckedAt       time.Time `json:"checked_at"`
}

type HealthService struct {
	registry    *Registry
	manager     *Manager
	maintenance *MaintenanceService
	now         func() time.Time
}

// NewHealthService builds the health service. The maintenance service may be
// nil for setups that wire it later; maintenance checks are then skipped.
func NewHealthService(registry *Registry, manager *Manager, maintenance *MaintenanceService, now func() time.Time) *HealthService {
	if now == nil {
		now = time.Now
	}
	return &HealthService{registry: registry, manager: manager, maintenance: maintenance, now: now}
}

func (s *HealthService) Check(id string) Health {
	result := Health{CircuitID: id, CheckedAt: s.now().UTC()}
	circuit, ok := s.registry.Get(id)
	if !ok {
		result.State = "unconfigured"
		result.Reason = "circuit is not configured"
		return result
	}
	result.PressureKPa = circuit.Pressure

	if s.maintenance != nil {
		if record, deactivated := s.maintenance.Maintenance(id); deactivated {
			result.Maintenance = &record
			result.RestoreRejected = s.maintenance.LastRestoreRejection(id)
			if s.valveStillOpen(id) {
				result.State = "maintenance-pending-isolation"
				result.IsolationPending = true
				result.Reason = "circuit is deactivated for maintenance but its valve is still open"
			} else if s.maintenance.isolationPending(id) {
				result.State = "maintenance-pending-isolation"
				result.IsolationPending = true
				result.Reason = "circuit is deactivated for maintenance but an active refill is not yet isolated"
			} else {
				result.State = "maintenance"
				result.Reason = "circuit is deactivated for maintenance"
			}
			if result.RestoreRejected != "" {
				result.Reason = result.RestoreRejected
			}
			return result
		}
	}

	if !circuit.Enabled {
		result.State = "disabled"
		result.Reason = "circuit is disabled"
		return result
	}
	if reservation, occupied := s.manager.Get(id); occupied {
		if reservation.Faulted {
			result.State = "fault-retained"
			result.Reason = "circuit is retained after an isolation failure"
		} else {
			result.State = "reserved"
			result.Reason = "circuit is reserved"
		}
		return result
	}
	result.State = "available"
	result.Available = true
	return result
}

// valveStillOpen reports whether the circuit's current reservation still has
// an open valve route.
func (s *HealthService) valveStillOpen(id string) bool {
	reservation, occupied := s.manager.Get(id)
	if !occupied || s.maintenance == nil || s.maintenance.valves == nil {
		return false
	}
	return s.maintenance.valves.IsOpen(reservation.SessionID.String())
}

func (s *HealthService) All() []Health {
	circuits := s.registry.List()
	result := make([]Health, 0, len(circuits))
	for _, circuit := range circuits {
		result = append(result, s.Check(circuit.ID))
	}
	return result
}
