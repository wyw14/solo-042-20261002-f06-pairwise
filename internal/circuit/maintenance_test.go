package circuit

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fakeRouteInspector struct {
	open map[string]bool
}

func (f *fakeRouteInspector) CircuitRouteOpen(circuitID string) bool {
	return f.open[circuitID]
}

func newMaintenanceFixture(t *testing.T) (*Registry, *Manager, *fakeRouteInspector, *MaintenanceService) {
	t.Helper()
	registry := NewRegistry(Circuit{ID: "C-A", Zone: "north", ValveID: "VALVE-A", Enabled: true})
	manager := NewManager("C-A")
	routes := &fakeRouteInspector{open: map[string]bool{}}
	service, err := NewMaintenanceService(registry, manager, routes, DefaultRestorePolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	manager.SetReservationGuard(service.Guard)
	return registry, manager, routes, service
}

func TestDeactivateBlocksNewReservations(t *testing.T) {
	_, manager, _, service := newMaintenanceFixture(t)
	if _, err := service.Deactivate("C-A", "valve replacement"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Reserve("C-A", "V-01", uuid.New(), time.Now()); !errors.Is(err, ErrCircuitMaintenance) {
		t.Fatalf("expected maintenance error, got %v", err)
	}
	if _, err := service.Restore("C-A"); err == nil {
		t.Fatal("restore without any pressure reading should fail")
	}
	if err := service.RecordPressure("C-A", 120, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Restore("C-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Reserve("C-A", "V-01", uuid.New(), time.Now()); err != nil {
		t.Fatalf("reservation after restore failed: %v", err)
	}
}

func TestActiveFillWithOpenValveIsNotIsolated(t *testing.T) {
	_, manager, routes, service := newMaintenanceFixture(t)
	sessionID := uuid.New()
	if _, err := manager.Reserve("C-A", "V-01", sessionID, time.Now()); err != nil {
		t.Fatal(err)
	}
	routes.open["C-A"] = true
	status, err := service.Deactivate("C-A", "gasket inspection")
	if err != nil {
		t.Fatal(err)
	}
	if status.Isolated {
		t.Fatal("circuit with an open valve was reported isolated")
	}
	if !strings.Contains(status.IsolationBlocker, "valve") {
		t.Fatalf("isolation blocker should mention the open valve, got %q", status.IsolationBlocker)
	}
	if _, err := service.Restore("C-A"); err == nil || !strings.Contains(err.Error(), "valve still open") {
		t.Fatalf("restore should refuse while the valve is open, got %v", err)
	}
	routes.open["C-A"] = false
	if err := manager.Release("C-A", sessionID); err != nil {
		t.Fatal(err)
	}
	status, err = service.Status("C-A")
	if err != nil {
		t.Fatal(err)
	}
	if !status.Isolated {
		t.Fatalf("circuit should be isolated after release, blocker %q", status.IsolationBlocker)
	}
}

func TestRestoreRequiresFaultRecovery(t *testing.T) {
	registry, manager, _, service := newMaintenanceFixture(t)
	sessionID := uuid.New()
	if _, err := manager.Reserve("C-A", "V-01", sessionID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := manager.RetainFault("C-A", sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Deactivate("C-A", "post-fault inspection"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Restore("C-A"); err == nil || !strings.Contains(err.Error(), "fault-retained") {
		t.Fatalf("restore should refuse a fault-retained circuit, got %v", err)
	}
	circuit, _ := registry.Get("C-A")
	if circuit.Maintenance == nil || circuit.Maintenance.LastRejection == nil {
		t.Fatal("rejected restore was not recorded on the maintenance registration")
	}
	if !strings.Contains(circuit.Maintenance.LastRejection.Reason, "fault-retained") {
		t.Fatalf("rejection reason should explain the fault retention, got %q", circuit.Maintenance.LastRejection.Reason)
	}
	if err := manager.Recover("C-A", sessionID); err != nil {
		t.Fatal(err)
	}
	if err := service.RecordPressure("C-A", 118, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Restore("C-A"); err != nil {
		t.Fatalf("restore after fault recovery failed: %v", err)
	}
	circuit, _ = registry.Get("C-A")
	if circuit.Maintenance != nil {
		t.Fatal("maintenance registration survived a successful restore")
	}
}

func TestRestoreRequiresFreshPressureWithinLimit(t *testing.T) {
	_, _, _, service := newMaintenanceFixture(t)
	stale := time.Now().Add(-time.Hour)
	if err := service.RecordPressure("C-A", 110, stale); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Deactivate("C-A", "routine"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Restore("C-A"); err == nil || !strings.Contains(err.Error(), "predates") {
		t.Fatalf("stale reading should block restore, got %v", err)
	}
	if err := service.RecordPressure("C-A", 500, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Restore("C-A"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("over-limit reading should block restore, got %v", err)
	}
	if err := service.RecordPressure("C-A", 122.5, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Restore("C-A"); err != nil {
		t.Fatalf("fresh in-limit reading should allow restore, got %v", err)
	}
}

func TestRecordPressureValidation(t *testing.T) {
	registry, _, _, service := newMaintenanceFixture(t)
	if err := service.RecordPressure("C-A", -1, time.Now()); !errors.Is(err, ErrInvalidReading) {
		t.Fatalf("negative reading should be rejected, got %v", err)
	}
	if err := service.RecordPressure("missing", 10, time.Now()); !errors.Is(err, ErrCircuitNotFound) {
		t.Fatalf("unknown circuit should be reported, got %v", err)
	}
	if err := service.RecordPressure("C-A", 101.3, time.Time{}); err != nil {
		t.Fatal(err)
	}
	circuit, _ := registry.Get("C-A")
	if circuit.PressureRecordedAt == nil {
		t.Fatal("reading timestamp was not stored")
	}
	if circuit.Pressure != 101.3 {
		t.Fatalf("pressure not updated, got %v", circuit.Pressure)
	}
}

func TestHealthExplainsMaintenanceAndRejection(t *testing.T) {
	registry, manager, routes, service := newMaintenanceFixture(t)
	health := NewHealthService(registry, manager, service, nil)
	sessionID := uuid.New()
	if _, err := manager.Reserve("C-A", "V-01", sessionID, time.Now()); err != nil {
		t.Fatal(err)
	}
	routes.open["C-A"] = true
	if _, err := service.Deactivate("C-A", "valve replacement"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Restore("C-A"); err == nil {
		t.Fatal("restore should be refused while the valve is open")
	}
	report := health.Check("C-A")
	if report.Available {
		t.Fatal("circuit under maintenance was reported available")
	}
	if report.Maintenance == nil {
		t.Fatal("health report does not carry the maintenance registration")
	}
	for _, want := range []string{"under maintenance", "valve", "rejected"} {
		if !strings.Contains(report.Reason, want) {
			t.Fatalf("health reason %q should mention %q", report.Reason, want)
		}
	}
	routes.open["C-A"] = false
	if err := manager.Release("C-A", sessionID); err != nil {
		t.Fatal(err)
	}
	report = health.Check("C-A")
	if !strings.Contains(report.Reason, "isolated") {
		t.Fatalf("health reason should report isolation, got %q", report.Reason)
	}
}
