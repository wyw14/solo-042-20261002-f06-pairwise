package circuit

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type stubFills struct{ active map[string]bool }

func (s *stubFills) IsSessionActive(sessionID string) bool { return s.active[sessionID] }

type stubValves struct{ open map[string]bool }

func (s *stubValves) IsOpen(sessionID string) bool { return s.open[sessionID] }

func fixedClock() func() time.Time {
	base := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	return func() time.Time { return base }
}

func newMaintenanceFixture(t *testing.T, options ...MaintenanceOption) (*Registry, *Manager, *MaintenanceService) {
	t.Helper()
	registry := NewRegistry(Circuit{ID: "C-A", Zone: "north", ValveID: "VALVE-A", Enabled: true})
	manager := NewManager("C-A")
	service, err := NewMaintenanceService(registry, manager, fixedClock(), options...)
	if err != nil {
		t.Fatal(err)
	}
	return registry, manager, service
}

func reserveCircuit(t *testing.T, manager *Manager) Reservation {
	t.Helper()
	reservation, err := manager.Reserve("C-A", "V-01", uuid.New(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return reservation
}

func TestDeactivateBlocksNewReservations(t *testing.T) {
	registry, manager, service := newMaintenanceFixture(t)

	pending, err := service.Deactivate("C-A", "seal replacement")
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("deactivation of an idle circuit should not report pending isolation")
	}
	if !manager.IsBlocked("C-A") {
		t.Fatal("manager did not block the deactivated circuit")
	}
	if _, err := manager.Reserve("C-A", "V-01", uuid.New(), time.Now()); !errors.Is(err, ErrMaintenanceDeactivated) {
		t.Fatalf("expected ErrMaintenanceDeactivated, got %v", err)
	}

	// Restore requires a reported pressure reading first.
	if err := service.Restore("C-A"); err == nil ||
		!strings.Contains(err.Error(), "no pressure reading") {
		t.Fatalf("expected missing-reading refusal, got %v", err)
	}
	if _, err := registry.RecordPressure("C-A", 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := service.Restore("C-A"); err != nil {
		t.Fatal(err)
	}
	if manager.IsBlocked("C-A") {
		t.Fatal("restored circuit remained blocked")
	}
	if _, err := manager.Reserve("C-A", "V-01", uuid.New(), time.Now()); err != nil {
		t.Fatalf("reservation after restore failed: %v", err)
	}
}

func TestOpenValvePreventsIsolationAndRestore(t *testing.T) {
	valves := &stubValves{open: map[string]bool{}}
	_, manager, service := newMaintenanceFixture(t, WithOpenValveChecker(valves))
	reservation := reserveCircuit(t, manager)
	valves.open[reservation.SessionID.String()] = true

	pending, err := service.Deactivate("C-A", "valve overhaul")
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("active refill with open valve must report pending isolation")
	}

	err = service.Restore("C-A")
	if err == nil || !strings.Contains(err.Error(), "valve is still open") {
		t.Fatalf("expected open-valve refusal, got %v", err)
	}
	if got := service.LastRestoreRejection("C-A"); !strings.Contains(got, "valve is still open") {
		t.Fatalf("rejection reason not retained: %q", got)
	}

	health := NewHealthService(service.registry, manager, service, fixedClock()).Check("C-A")
	if health.State != "maintenance-pending-isolation" || !health.IsolationPending {
		t.Fatalf("health did not flag pending isolation: %+v", health)
	}
	if !strings.Contains(health.Reason, "valve is still open") {
		t.Fatalf("health reason should mention the open valve, got %q", health.Reason)
	}
	if health.RestoreRejected == "" {
		t.Fatal("health report should carry the restore rejection")
	}

	// Closing the valve alone is not enough: the fill session is still active.
	valves.open[reservation.SessionID.String()] = false
	fills := &stubFills{active: map[string]bool{reservation.SessionID.String(): true}}
	service.fills = fills
	if err := service.Restore("C-A"); err == nil ||
		!strings.Contains(err.Error(), "active refill is not isolated") {
		t.Fatalf("expected active-refill refusal, got %v", err)
	}
}

func TestActiveFillWithoutOpenValvePending(t *testing.T) {
	_, manager, service := newMaintenanceFixture(t,
		WithFillInspector(&stubFills{active: map[string]bool{}}),
	)
	reservation := reserveCircuit(t, manager)
	service.fills.(*stubFills).active[reservation.SessionID.String()] = true

	pending, err := service.Deactivate("C-A", "inspection")
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("active fill must keep isolation pending even with valve closed")
	}
	if err := service.Restore("C-A"); err == nil ||
		!strings.Contains(err.Error(), "active refill is not isolated") {
		t.Fatalf("expected active-refill refusal, got %v", err)
	}
}

func TestFaultRetentionBlocksRestore(t *testing.T) {
	registry, manager, service := newMaintenanceFixture(t,
		WithFillInspector(&stubFills{active: map[string]bool{}}),
		WithOpenValveChecker(&stubValves{open: map[string]bool{}}),
	)
	reservation := reserveCircuit(t, manager)
	if err := manager.RetainFault("C-A", reservation.SessionID); err != nil {
		t.Fatal(err)
	}

	pending, err := service.Deactivate("C-A", "post-failure inspection")
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("a non-active fault retention should not count as live fill activity")
	}
	if _, err := registry.RecordPressure("C-A", 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := service.Restore("C-A"); err == nil ||
		!strings.Contains(err.Error(), "retained after an isolation failure") {
		t.Fatalf("expected fault-retention refusal, got %v", err)
	}
}

func TestPressureThresholdRefusesRestore(t *testing.T) {
	registry, _, service := newMaintenanceFixture(t)
	if _, err := service.Deactivate("C-A", "leak check"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.RecordPressure("C-A", MaintenanceRestorePressureKPa+0.5, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := service.Restore("C-A"); err == nil ||
		!strings.Contains(err.Error(), "above the isolation threshold") {
		t.Fatalf("expected threshold refusal, got %v", err)
	}
	if _, err := registry.RecordPressure("C-A", MaintenanceRestorePressureKPa, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := service.Restore("C-A"); err != nil {
		t.Fatalf("pressure at the threshold should restore: %v", err)
	}
}

func TestMaintenanceDeactivateValidation(t *testing.T) {
	_, _, service := newMaintenanceFixture(t)
	if _, err := service.Deactivate("C-A", ""); err == nil {
		t.Fatal("deactivation without a reason was accepted")
	}
	if _, err := service.Deactivate("C-UNKNOWN", "x"); !errors.Is(err, ErrCircuitNotFound) {
		t.Fatalf("expected ErrCircuitNotFound, got %v", err)
	}
	if _, err := service.Deactivate("C-A", "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Deactivate("C-A", "second"); !errors.Is(err, ErrMaintenanceActive) {
		t.Fatalf("expected ErrMaintenanceActive, got %v", err)
	}
	if err := service.Restore("C-B"); !errors.Is(err, ErrNotInMaintenance) {
		t.Fatalf("expected ErrNotInMaintenance, got %v", err)
	}
}

func TestHealthReportsSettledMaintenance(t *testing.T) {
	registry, manager, service := newMaintenanceFixture(t)
	if _, err := service.Deactivate("C-A", "planned work"); err != nil {
		t.Fatal(err)
	}
	health := NewHealthService(registry, manager, service, fixedClock()).Check("C-A")
	if health.Available || health.State != "maintenance" {
		t.Fatalf("expected settled maintenance health, got %+v", health)
	}
	if health.Maintenance == nil || health.Maintenance.Reason != "planned work" {
		t.Fatalf("maintenance record missing from health: %+v", health)
	}
}

func TestPressureReadings(t *testing.T) {
	registry, _, _ := newMaintenanceFixture(t)
	if _, err := registry.RecordPressure("C-A", -1, time.Now()); err == nil {
		t.Fatal("negative pressure reading was accepted")
	}
	if _, err := registry.RecordPressure("C-UNKNOWN", 1, time.Now()); !errors.Is(err, ErrCircuitNotFound) {
		t.Fatalf("expected ErrCircuitNotFound, got %v", err)
	}
	updated, err := registry.RecordPressure("C-A", 42, fixedClock()())
	if err != nil {
		t.Fatal(err)
	}
	if updated.Pressure != 42 || updated.PressureRecordedAt.IsZero() {
		t.Fatalf("reading not recorded: %+v", updated)
	}
}

func TestDeactivateIsAtomicAgainstReservations(t *testing.T) {
	_, manager, service := newMaintenanceFixture(t)
	const goroutines = 64

	var group sync.WaitGroup
	group.Add(goroutines + 1)
	reserved := make(chan error, goroutines)
	go func() {
		defer group.Done()
		_, _ = service.Deactivate("C-A", "concurrent")
	}()
	for i := 0; i < goroutines; i++ {
		go func() {
			defer group.Done()
			_, err := manager.Reserve("C-A", "V-01", uuid.New(), time.Now())
			reserved <- err
		}()
	}
	group.Wait()
	close(reserved)

	var reservations int
	for err := range reserved {
		switch {
		case err == nil:
			reservations++
		case errors.Is(err, ErrMaintenanceDeactivated), errors.Is(err, ErrCircuitBusy):
		default:
			t.Fatalf("unexpected reservation error: %v", err)
		}
	}
	// At most one refill can hold the circuit; everything after the
	// deactivation lands is refused.
	if reservations > 1 {
		t.Fatalf("expected at most one reservation, got %d", reservations)
	}
	if !manager.IsBlocked("C-A") {
		t.Fatal("circuit must be blocked after deactivation")
	}
}
