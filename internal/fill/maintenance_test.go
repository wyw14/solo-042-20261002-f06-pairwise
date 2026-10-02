package fill

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wyw14/cry-98/internal/circuit"
	"github.com/wyw14/cry-98/internal/model"
	"github.com/wyw14/cry-98/internal/valve"
	"github.com/wyw14/cry-98/internal/vessel"
)

func maintenanceCoordinatorFixture(t *testing.T) (*Coordinator, *circuit.Registry, *circuit.Manager, *circuit.MaintenanceService, *vessel.StateService) {
	t.Helper()
	now := func() time.Time { return time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC) }
	circuitRegistry := circuit.NewRegistry(
		circuit.Circuit{ID: "C-A", Zone: "north", ValveID: "VALVE-A", Enabled: true},
	)
	circuitManager := circuit.NewManager("C-A")
	transport := valve.NewMemoryTransport(now)
	controller, err := valve.NewController(transport, now)
	if err != nil {
		t.Fatal(err)
	}
	routes := valve.NewRouteService(valve.NewCommandService(controller, now))

	vesselRegistry := vessel.NewRegistry()
	for _, seed := range []struct{ id, name, zone string }{
		{"V-01", "Vessel 1", "north"},
		{"V-02", "Vessel 2", "north"},
	} {
		v, err := model.NewVessel(seed.id, seed.name, seed.zone, now())
		if err != nil {
			t.Fatal(err)
		}
		if err := vesselRegistry.Register(v); err != nil {
			t.Fatal(err)
		}
	}
	vesselState := vessel.NewStateService(vesselRegistry, now)
	coordinator := NewCoordinator(circuitManager, circuitRegistry, routes, vesselState, now)
	maintenance, err := circuit.NewMaintenanceService(
		circuitRegistry, circuitManager, now,
		circuit.WithFillInspector(coordinator),
		circuit.WithOpenValveChecker(routes),
	)
	if err != nil {
		t.Fatal(err)
	}
	return coordinator, circuitRegistry, circuitManager, maintenance, vesselState
}

func TestDeactivatedCircuitRejectsNewFills(t *testing.T) {
	coordinator, _, manager, maintenance, _ := maintenanceCoordinatorFixture(t)

	pending, err := maintenance.Deactivate("C-A", "scheduled maintenance")
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("idle circuit should deactivate cleanly")
	}
	if _, err := coordinator.StartManual(context.Background(), "V-01", 2.0); !errors.Is(err, circuit.ErrMaintenanceDeactivated) {
		t.Fatalf("expected ErrMaintenanceDeactivated, got %v", err)
	}
	if _, occupied := manager.Get("C-A"); occupied {
		t.Fatal("rejected fill must not hold a reservation")
	}
}

func TestActiveFillKeepsIsolationPendingUntilValveClosed(t *testing.T) {
	coordinator, registry, manager, maintenance, vesselState := maintenanceCoordinatorFixture(t)
	ctx := context.Background()

	session, err := coordinator.StartManual(ctx, "V-01", 2.0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.BeginSoak(session.ID); err != nil {
		t.Fatal(err)
	}

	pending, err := maintenance.Deactivate("C-A", "emergency isolation")
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("circuit with a filling session and open valve must report pending isolation")
	}

	// Restore is refused while the valve route is open.
	if err := maintenance.Restore("C-A"); err == nil ||
		!strings.Contains(err.Error(), "valve is still open") {
		t.Fatalf("expected open-valve refusal, got %v", err)
	}

	// A second fill (on another vessel) cannot start while the circuit is blocked.
	if _, err := coordinator.StartManual(ctx, "V-02", 1.0); !errors.Is(err, circuit.ErrMaintenanceDeactivated) {
		t.Fatalf("expected deactivated circuit to block second fill, got %v", err)
	}

	// Abort closes the valve and releases the (non-faulted) reservation.
	aborts := NewAbortService(coordinator, time.Now)
	if _, err := aborts.Abort(ctx, session.ID, "maintenance"); err != nil {
		t.Fatal(err)
	}
	if _, occupied := manager.Get("C-A"); occupied {
		t.Fatal("aborted fill should have released the circuit")
	}
	if _, ok := vesselState.Registry().Get("V-01"); !ok {
		t.Fatal("vessel V-01 missing after abort")
	}

	// Still under maintenance and blocked until pressure bled down and restore.
	if _, err := coordinator.StartManual(ctx, "V-01", 1.0); !errors.Is(err, circuit.ErrMaintenanceDeactivated) {
		t.Fatalf("circuit should stay blocked before restore, got %v", err)
	}
	if _, err := registry.RecordPressure("C-A", 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := maintenance.Restore("C-A"); err != nil {
		t.Fatalf("restore after isolation should succeed: %v", err)
	}
	if _, err := coordinator.StartManual(ctx, "V-01", 1.0); err != nil {
		t.Fatalf("fill after restore should start: %v", err)
	}
}

func TestFaultedReservationRequiresRecoveryBeforeRestore(t *testing.T) {
	coordinator, registry, _, maintenance, _ := maintenanceCoordinatorFixture(t)
	ctx := context.Background()

	session, err := coordinator.StartManual(ctx, "V-01", 2.0)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the post-incident state: the valve was closed manually, the
	// fill session is terminal, but the reservation is retained after the
	// isolation failure.
	if err := coordinator.routes.Close(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	coordinator.mu.Lock()
	coordinator.sessions[session.ID] = session.Failed("isolation failure", time.Now())
	coordinator.mu.Unlock()
	if err := coordinator.circuits.RetainFault("C-A", session.ID); err != nil {
		t.Fatal(err)
	}

	pending, err := maintenance.Deactivate("C-A", "post-failure work")
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("closed valve plus terminal session should not be pending isolation")
	}
	if _, err := registry.RecordPressure("C-A", 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := maintenance.Restore("C-A"); err == nil ||
		!strings.Contains(err.Error(), "retained after an isolation failure") {
		t.Fatalf("expected fault-retention refusal, got %v", err)
	}

	// Explicit fault recovery clears the retention and restore succeeds.
	if err := coordinator.circuits.Recover("C-A", session.ID); err != nil {
		t.Fatal(err)
	}
	if err := maintenance.Restore("C-A"); err != nil {
		t.Fatalf("restore after fault recovery should succeed: %v", err)
	}
}
