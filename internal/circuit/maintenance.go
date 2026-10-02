package circuit

import (
	"errors"
	"fmt"
	"math"
	"time"
)

var (
	ErrCircuitMaintenance   = errors.New("circuit is under maintenance")
	ErrAlreadyInMaintenance = errors.New("circuit is already under maintenance")
	ErrNotUnderMaintenance  = errors.New("circuit is not under maintenance")
	ErrReasonRequired       = errors.New("maintenance reason is required")
	ErrInvalidReading       = errors.New("invalid pressure reading")
)

// Rejection records a refused maintenance transition so the refusal reason
// stays visible in health reports until the operation finally succeeds.
type Rejection struct {
	Operation string    `json:"operation"`
	Reason    string    `json:"reason"`
	At        time.Time `json:"at"`
}

// Maintenance is the per-circuit registration of a maintenance outage.
type Maintenance struct {
	Reason        string     `json:"reason"`
	Since         time.Time  `json:"since"`
	LastRejection *Rejection `json:"last_rejection,omitempty"`
}

// RestorePolicy bounds the conditions under which a circuit may return to
// service after maintenance.
type RestorePolicy struct {
	MaxPressureKPa float64 `json:"max_pressure_kpa"`
}

func DefaultRestorePolicy() RestorePolicy {
	return RestorePolicy{MaxPressureKPa: 150}
}

func (p RestorePolicy) Validate() error {
	if p.MaxPressureKPa <= 0 {
		return errors.New("restore policy requires a positive maximum pressure")
	}
	return nil
}

// RouteInspector reports whether a circuit still has an open valve route.
type RouteInspector interface {
	CircuitRouteOpen(circuitID string) bool
}

// Status is the live maintenance view of a circuit: the registration plus
// whether the circuit is actually isolated right now.
type Status struct {
	CircuitID        string       `json:"circuit_id"`
	Maintenance      *Maintenance `json:"maintenance,omitempty"`
	Isolated         bool         `json:"isolated"`
	IsolationBlocker string       `json:"isolation_blocker,omitempty"`
}

type MaintenanceService struct {
	registry *Registry
	manager  *Manager
	routes   RouteInspector
	policy   RestorePolicy
	now      func() time.Time
}

func NewMaintenanceService(registry *Registry, manager *Manager, routes RouteInspector, policy RestorePolicy, now func() time.Time) (*MaintenanceService, error) {
	if registry == nil || manager == nil || routes == nil {
		return nil, errors.New("maintenance service requires a registry, reservation manager, and route inspector")
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &MaintenanceService{registry: registry, manager: manager, routes: routes, policy: policy, now: now}, nil
}

// Guard blocks new reservations on circuits under maintenance. It runs
// inside the reservation critical section so deactivation and reservation
// cannot interleave.
func (s *MaintenanceService) Guard(circuitID string) error {
	circuit, ok := s.registry.Get(circuitID)
	if !ok {
		return nil
	}
	if circuit.Maintenance != nil {
		return ErrCircuitMaintenance
	}
	return nil
}

// Deactivate registers a maintenance outage. In-flight fills are not
// interrupted, but the circuit is reported as not isolated until they
// finish and their valves close.
func (s *MaintenanceService) Deactivate(id, reason string) (Status, error) {
	if reason == "" {
		return Status{}, ErrReasonRequired
	}
	circuit, ok := s.registry.Get(id)
	if !ok {
		return Status{}, ErrCircuitNotFound
	}
	if circuit.Maintenance != nil {
		return Status{}, ErrAlreadyInMaintenance
	}
	maintenance := Maintenance{Reason: reason, Since: s.now().UTC()}
	if err := s.registry.setMaintenance(id, maintenance); err != nil {
		return Status{}, err
	}
	return s.Status(id)
}

// Restore returns a circuit to service once it is isolated, carries no
// fault-retained reservation, and has a fresh in-limit pressure reading.
// A refused attempt is recorded on the maintenance registration so health
// reports can explain why the circuit is still out of service.
func (s *MaintenanceService) Restore(id string) (Status, error) {
	circuit, ok := s.registry.Get(id)
	if !ok {
		return Status{}, ErrCircuitNotFound
	}
	if circuit.Maintenance == nil {
		return Status{}, ErrNotUnderMaintenance
	}
	if err := s.checkRestoreRules(circuit); err != nil {
		rejection := Rejection{Operation: "restore", Reason: err.Error(), At: s.now().UTC()}
		_ = s.registry.recordRejection(id, rejection)
		return Status{}, err
	}
	if err := s.registry.clearMaintenance(id); err != nil {
		return Status{}, err
	}
	return s.Status(id)
}

func (s *MaintenanceService) checkRestoreRules(circuit Circuit) error {
	reservation, occupied := s.manager.Get(circuit.ID)
	if occupied && reservation.Faulted {
		return errors.New("circuit has a fault-retained reservation; recover the isolation failure first")
	}
	if occupied && s.routes.CircuitRouteOpen(circuit.ID) {
		return errors.New("circuit has an active fill with the valve still open")
	}
	if circuit.PressureRecordedAt == nil {
		return errors.New("no pressure reading recorded for the circuit")
	}
	if circuit.PressureRecordedAt.Before(circuit.Maintenance.Since) {
		return errors.New("pressure reading predates the maintenance period; record a fresh reading")
	}
	if circuit.Pressure > s.policy.MaxPressureKPa {
		return fmt.Errorf("pressure %.1f kPa exceeds the restore limit of %.1f kPa", circuit.Pressure, s.policy.MaxPressureKPa)
	}
	return nil
}

// RecordPressure registers a pressure reading for a circuit.
func (s *MaintenanceService) RecordPressure(id string, pressure float64, observedAt time.Time) error {
	if math.IsNaN(pressure) || math.IsInf(pressure, 0) {
		return fmt.Errorf("%w: must be a finite number", ErrInvalidReading)
	}
	if pressure < 0 {
		return fmt.Errorf("%w: cannot be negative", ErrInvalidReading)
	}
	if observedAt.IsZero() {
		observedAt = s.now()
	}
	return s.registry.RecordPressure(id, pressure, observedAt)
}

func (s *MaintenanceService) Status(id string) (Status, error) {
	circuit, ok := s.registry.Get(id)
	if !ok {
		return Status{}, ErrCircuitNotFound
	}
	isolated, blocker := s.Isolation(id)
	return Status{
		CircuitID:        id,
		Maintenance:      circuit.Maintenance,
		Isolated:         isolated,
		IsolationBlocker: blocker,
	}, nil
}

// Isolation reports whether the circuit is safe to work on. An active fill
// whose valve is still open, or a fault-retained reservation, means the
// circuit must not be treated as isolated.
func (s *MaintenanceService) Isolation(id string) (bool, string) {
	reservation, occupied := s.manager.Get(id)
	if !occupied {
		return true, ""
	}
	if reservation.Faulted {
		return false, "reservation is fault-retained"
	}
	if s.routes.CircuitRouteOpen(id) {
		return false, "valve is still open for the active fill"
	}
	return true, ""
}
