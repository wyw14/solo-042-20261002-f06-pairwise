package circuit

import (
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	ErrCircuitBusy         = errors.New("circuit is already reserved")
	ErrReservationMismatch = errors.New("circuit reservation does not match")
)

type Reservation struct {
	CircuitID  string    `json:"circuit_id"`
	SessionID  uuid.UUID `json:"session_id"`
	VesselID   string    `json:"vessel_id"`
	Generation uint64    `json:"generation"`
	ReservedAt time.Time `json:"reserved_at"`
	Faulted    bool      `json:"faulted"`
}

type Manager struct {
	mu           sync.Mutex
	reservations map[string]Reservation
	generations  map[string]uint64
	blocked      map[string]bool
}

func NewManager(circuitIDs ...string) *Manager {
	m := &Manager{
		reservations: make(map[string]Reservation),
		generations:  make(map[string]uint64),
		blocked:      make(map[string]bool),
	}
	for _, id := range circuitIDs {
		if id != "" {
			m.generations[id] = 0
		}
	}
	return m
}

// Block refuses all new reservations for the circuit until Unblock is called.
// Existing reservations are left in place so an in-progress refill completes
// or fails through the normal isolation path.
func (m *Manager) Block(circuitID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blocked[circuitID] = true
}

// Unblock allows new reservations for the circuit again.
func (m *Manager) Unblock(circuitID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.blocked, circuitID)
}

// IsBlocked reports whether new reservations are refused for the circuit.
func (m *Manager) IsBlocked(circuitID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.blocked[circuitID]
}

func (m *Manager) Reserve(circuitID, vesselID string, sessionID uuid.UUID, now time.Time) (Reservation, error) {
	if circuitID == "" || vesselID == "" || sessionID == uuid.Nil {
		return Reservation{}, errors.New("invalid circuit reservation")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.blocked[circuitID] {
		return Reservation{}, ErrMaintenanceDeactivated
	}
	if _, occupied := m.reservations[circuitID]; occupied {
		return Reservation{}, ErrCircuitBusy
	}
	m.generations[circuitID]++
	reservation := Reservation{
		CircuitID:  circuitID,
		SessionID:  sessionID,
		VesselID:   vesselID,
		Generation: m.generations[circuitID],
		ReservedAt: now.UTC(),
	}
	m.reservations[circuitID] = reservation
	return reservation, nil
}

func (m *Manager) Release(circuitID string, sessionID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.reservations[circuitID]
	if !ok {
		return nil
	}
	if current.SessionID != sessionID {
		return ErrReservationMismatch
	}
	if current.Faulted {
		return errors.New("faulted circuit requires explicit recovery")
	}
	delete(m.reservations, circuitID)
	return nil
}

func (m *Manager) RetainFault(circuitID string, sessionID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.reservations[circuitID]
	if !ok || current.SessionID != sessionID {
		return ErrReservationMismatch
	}
	current.Faulted = true
	m.reservations[circuitID] = current
	return nil
}

func (m *Manager) Recover(circuitID string, sessionID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.reservations[circuitID]
	if !ok || current.SessionID != sessionID {
		return ErrReservationMismatch
	}
	delete(m.reservations, circuitID)
	return nil
}

func (m *Manager) Get(circuitID string) (Reservation, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	reservation, ok := m.reservations[circuitID]
	return reservation, ok
}
