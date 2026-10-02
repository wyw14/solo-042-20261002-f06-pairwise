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
	guard        func(circuitID string) error
}

func NewManager(circuitIDs ...string) *Manager {
	m := &Manager{
		reservations: make(map[string]Reservation),
		generations:  make(map[string]uint64),
	}
	for _, id := range circuitIDs {
		if id != "" {
			m.generations[id] = 0
		}
	}
	return m
}

// SetReservationGuard installs a check that runs inside the reservation
// critical section, so a circuit cannot be reserved once the guard starts
// rejecting it (e.g. after a maintenance deactivation).
func (m *Manager) SetReservationGuard(guard func(circuitID string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.guard = guard
}

func (m *Manager) Reserve(circuitID, vesselID string, sessionID uuid.UUID, now time.Time) (Reservation, error) {
	if circuitID == "" || vesselID == "" || sessionID == uuid.Nil {
		return Reservation{}, errors.New("invalid circuit reservation")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.guard != nil {
		if err := m.guard(circuitID); err != nil {
			return Reservation{}, err
		}
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
