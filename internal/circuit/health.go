package circuit

import "time"

type Health struct {
	CircuitID   string       `json:"circuit_id"`
	Available   bool         `json:"available"`
	Reason      string       `json:"reason,omitempty"`
	Maintenance *Maintenance `json:"maintenance,omitempty"`
	CheckedAt   time.Time    `json:"checked_at"`
}

type HealthService struct {
	registry    *Registry
	manager     *Manager
	maintenance *MaintenanceService
	now         func() time.Time
}

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
		result.Reason = "circuit is not configured"
		return result
	}
	if circuit.Maintenance != nil {
		result.Maintenance = circuit.Maintenance
		if isolated, blocker := s.maintenance.Isolation(id); isolated {
			result.Reason = "circuit is under maintenance (isolated)"
		} else {
			result.Reason = "circuit is under maintenance: " + blocker
		}
		if rejection := circuit.Maintenance.LastRejection; rejection != nil {
			result.Reason += "; last " + rejection.Operation + " rejected: " + rejection.Reason
		}
		return result
	}
	if !circuit.Enabled {
		result.Reason = "circuit is disabled"
		return result
	}
	if reservation, occupied := s.manager.Get(id); occupied {
		if reservation.Faulted {
			result.Reason = "circuit is retained after an isolation failure"
		} else {
			result.Reason = "circuit is reserved"
		}
		return result
	}
	result.Available = true
	return result
}

func (s *HealthService) All() []Health {
	circuits := s.registry.List()
	result := make([]Health, 0, len(circuits))
	for _, circuit := range circuits {
		result = append(result, s.Check(circuit.ID))
	}
	return result
}
