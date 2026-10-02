package circuit

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// MaintenanceRestorePressureKPa is the pressure a circuit must report at or
// below before it can leave maintenance (a zero-pressure line is isolated).
const MaintenanceRestorePressureKPa = 10.0

var (
	// ErrMaintenanceDeactivated is returned by Reserve while a circuit is
	// registered out of service for maintenance.
	ErrMaintenanceDeactivated = errors.New("circuit is deactivated for maintenance")
	// ErrMaintenanceActive is returned when deactivation is requested for a
	// circuit that is already under maintenance.
	ErrMaintenanceActive = errors.New("circuit is already under maintenance")
	// ErrNotInMaintenance is returned when restore is requested for a circuit
	// that is not under maintenance.
	ErrNotInMaintenance = errors.New("circuit is not under maintenance")
	// ErrInvalidMaintenanceRequest is returned for malformed deactivate input.
	ErrInvalidMaintenanceRequest = errors.New("invalid maintenance request")
)

// ActiveFillInspector reports whether a fill session is still active.
// Implemented by the fill coordinator.
type ActiveFillInspector interface {
	IsSessionActive(sessionID string) bool
}

// OpenValveChecker reports whether a session's valve route is still open.
// Implemented by the valve route service.
type OpenValveChecker interface {
	IsOpen(sessionID string) bool
}

// Maintenance records the out-of-service state of one circuit.
type Maintenance struct {
	Reason        string    `json:"reason"`
	DeactivatedAt time.Time `json:"deactivated_at"`
}

// MaintenanceService registers circuits out of service for maintenance and
// restores them once the isolation rules are satisfied.
type MaintenanceService struct {
	registry   *Registry
	manager    *Manager
	fills      ActiveFillInspector
	valves     OpenValveChecker
	now        func() time.Time
	maxRestore float64

	mu            sync.Mutex
	deactivations map[string]Maintenance
	// restoreRejected holds the last refusal explanation per circuit so the
	// health report can state why restore was not allowed.
	restoreRejected map[string]string
}

// MaintenanceOption configures a MaintenanceService.
type MaintenanceOption func(*MaintenanceService)

// WithRestorePressureThreshold overrides the pressure threshold at or below
// which a deactivated circuit may be restored.
func WithRestorePressureThreshold(kpa float64) MaintenanceOption {
	return func(s *MaintenanceService) { s.maxRestore = kpa }
}

// WithFillInspector wires the fill coordinator so the service can tell active
// refills apart from retained fault reservations.
func WithFillInspector(inspector ActiveFillInspector) MaintenanceOption {
	return func(s *MaintenanceService) { s.fills = inspector }
}

// WithOpenValveChecker wires the valve route service so the service can reject
// isolation while a valve is still open.
func WithOpenValveChecker(checker OpenValveChecker) MaintenanceOption {
	return func(s *MaintenanceService) { s.valves = checker }
}

func NewMaintenanceService(registry *Registry, manager *Manager, now func() time.Time, options ...MaintenanceOption) (*MaintenanceService, error) {
	if registry == nil || manager == nil {
		return nil, errors.New("maintenance service requires a registry and a manager")
	}
	if now == nil {
		now = time.Now
	}
	service := &MaintenanceService{
		registry:        registry,
		manager:         manager,
		now:             now,
		maxRestore:      MaintenanceRestorePressureKPa,
		deactivations:   make(map[string]Maintenance),
		restoreRejected: make(map[string]string),
	}
	for _, option := range options {
		option(service)
	}
	return service, nil
}

// Maintenance returns the maintenance record for a circuit, if any.
func (s *MaintenanceService) Maintenance(circuitID string) (Maintenance, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.deactivations[circuitID]
	return record, ok
}

// LastRestoreRejection returns the explanation from the most recent refused
// restore for the circuit.
func (s *MaintenanceService) LastRestoreRejection(circuitID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restoreRejected[circuitID]
}

// Deactivate registers the circuit out of service for maintenance. New
// reservations are blocked atomically. A circuit with an active refill can be
// deactivated, but it is not considered isolated: the refill must first be
// aborted and its valve confirmed closed. The returned bool is true when the
// circuit still has live fill activity (valve open or active session).
func (s *MaintenanceService) Deactivate(circuitID, reason string) (pendingIsolation bool, err error) {
	if circuitID == "" {
		return false, fmt.Errorf("%w: circuit id is required", ErrInvalidMaintenanceRequest)
	}
	if reason == "" {
		return false, fmt.Errorf("%w: maintenance reason is required", ErrInvalidMaintenanceRequest)
	}
	if _, ok := s.registry.Get(circuitID); !ok {
		return false, ErrCircuitNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.deactivations[circuitID]; exists {
		return s.isolationPending(circuitID), ErrMaintenanceActive
	}
	// Blocking in the same critical section that records deactivation makes
	// admission atomic: once this returns, Reserve refuses the circuit and no
	// refill can have slipped in between. The manager, valve and fill locks
	// are independent and never acquired in reverse order.
	s.manager.Block(circuitID)
	s.deactivations[circuitID] = Maintenance{Reason: reason, DeactivatedAt: s.now().UTC()}
	delete(s.restoreRejected, circuitID)
	return s.isolationPending(circuitID), nil
}

// isolationPending reports whether live fill activity keeps the circuit from
// being treated as isolated (open valve takes priority, then active session).
func (s *MaintenanceService) isolationPending(circuitID string) bool {
	reservation, occupied := s.manager.Get(circuitID)
	if !occupied {
		return false
	}
	if s.valves != nil && s.valves.IsOpen(reservation.SessionID.String()) {
		return true
	}
	if s.fills != nil && s.fills.IsSessionActive(reservation.SessionID.String()) {
		return true
	}
	return false
}

// Restore brings a deactivated circuit back into service. Restore is refused
// while the valve may still be open, the fill is still active, pressure has
// not bled down to the isolation threshold (or was never reported), or the
// circuit is retained after an isolation failure.
func (s *MaintenanceService) Restore(circuitID string) error {
	s.mu.Lock()
	if _, ok := s.deactivations[circuitID]; !ok {
		s.mu.Unlock()
		return ErrNotInMaintenance
	}
	s.mu.Unlock()

	if reason := s.restoreBlocker(circuitID); reason != "" {
		s.mu.Lock()
		s.restoreRejected[circuitID] = reason
		s.mu.Unlock()
		return errors.New(reason)
	}

	s.mu.Lock()
	s.manager.Unblock(circuitID)
	delete(s.deactivations, circuitID)
	delete(s.restoreRejected, circuitID)
	s.mu.Unlock()
	return nil
}

// restoreBlocker evaluates the restore rules in safety order and returns the
// first reason restore must be refused, or "" when the circuit may be restored.
func (s *MaintenanceService) restoreBlocker(circuitID string) string {
	if reservation, occupied := s.manager.Get(circuitID); occupied {
		if s.valves != nil && s.valves.IsOpen(reservation.SessionID.String()) {
			return "restore refused: valve is still open"
		}
		if s.fills != nil && s.fills.IsSessionActive(reservation.SessionID.String()) {
			return "restore refused: active refill is not isolated"
		}
		if reservation.Faulted {
			return "restore refused: circuit is retained after an isolation failure"
		}
	}
	circuit, ok := s.registry.Get(circuitID)
	if !ok {
		return "restore refused: circuit is not configured"
	}
	if circuit.PressureRecordedAt.IsZero() {
		return "restore refused: no pressure reading has been reported"
	}
	if circuit.Pressure > s.maxRestore {
		return "restore refused: pressure is above the isolation threshold"
	}
	return ""
}
