package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-98/internal/circuit"
)

type apiFillStub struct{ active map[string]bool }

func (s *apiFillStub) IsSessionActive(sessionID string) bool { return s.active[sessionID] }

type apiValveStub struct{ open map[string]bool }

func (s *apiValveStub) IsOpen(sessionID string) bool { return s.open[sessionID] }

func circuitAPIFixture(t *testing.T) (*Server, *circuit.Manager, *apiValveStub) {
	t.Helper()
	now := func() time.Time { return time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC) }
	registry := circuit.NewRegistry(circuit.Circuit{ID: "C-A", Zone: "north", ValveID: "VALVE-A", Enabled: true})
	manager := circuit.NewManager("C-A")
	valves := &apiValveStub{open: map[string]bool{}}
	maintenance, err := circuit.NewMaintenanceService(
		registry, manager, now,
		circuit.WithFillInspector(&apiFillStub{active: map[string]bool{}}),
		circuit.WithOpenValveChecker(valves),
	)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(Dependencies{
		Circuits:    registry,
		Maintenance: maintenance,
		Now:         now,
	})
	return server, manager, valves
}

func doRequest(t *testing.T, handler http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	} else {
		reader = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, path, reader)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var decoded map[string]any
	if response.Body.Len() > 0 {
		if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("invalid json response %q: %v", response.Body.String(), err)
		}
	}
	return response.Code, decoded
}

func TestCircuitPressureLifecycleEndpoints(t *testing.T) {
	server, _, _ := circuitAPIFixture(t)
	handler := server.Handler()

	status, body := doRequest(t, handler, http.MethodGet, "/api/circuits", "")
	if status != http.StatusOK {
		t.Fatalf("list status = %d", status)
	}
	items, ok := body["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("expected one circuit, got %v", body["items"])
	}

	status, body = doRequest(t, handler, http.MethodPost, "/api/circuits/C-A/deactivate", `{"reason":"seal work"}`)
	if status != http.StatusAccepted {
		t.Fatalf("deactivate status = %d body=%v", status, body)
	}
	if body["isolation_pending"] != nil {
		t.Fatalf("idle circuit should not be pending isolation: %v", body)
	}

	// Idempotent repeat reports the same state.
	if status, _ = doRequest(t, handler, http.MethodPost, "/api/circuits/C-A/deactivate", `{"reason":"seal work"}`); status != http.StatusOK {
		t.Fatalf("repeat deactivate status = %d", status)
	}

	// Restore without a pressure reading is refused and the reason is given.
	status, body = doRequest(t, handler, http.MethodPost, "/api/circuits/C-A/restore", "")
	if status != http.StatusConflict {
		t.Fatalf("restore status = %d", status)
	}
	if message, _ := body["error"].(string); !strings.Contains(message, "no pressure reading") {
		t.Fatalf("expected missing-reading reason, got %v", body)
	}

	if status, _ = doRequest(t, handler, http.MethodPost, "/api/circuits/C-A/pressure", `{"pressure_kpa":-2}`); status != http.StatusBadRequest {
		t.Fatalf("negative pressure status = %d", status)
	}
	if status, _ = doRequest(t, handler, http.MethodPost, "/api/circuits/C-A/pressure", `{"pressure_kpa":0}`); status != http.StatusOK {
		t.Fatalf("pressure status = %d", status)
	}
	if status, _ = doRequest(t, handler, http.MethodPost, "/api/circuits/C-A/restore", ""); status != http.StatusOK {
		t.Fatalf("restore after bled-down pressure status = %d", status)
	}
}

func TestCircuitDeactivateValidationEndpoints(t *testing.T) {
	server, _, _ := circuitAPIFixture(t)
	handler := server.Handler()

	if status, _ := doRequest(t, handler, http.MethodPost, "/api/circuits/C-UNKNOWN/deactivate", `{"reason":"x"}`); status != http.StatusNotFound {
		t.Fatalf("unknown circuit status = %d", status)
	}
	if status, _ := doRequest(t, handler, http.MethodPost, "/api/circuits/C-A/deactivate", `{"reason":""}`); status != http.StatusBadRequest {
		t.Fatalf("missing reason status = %d", status)
	}
	if status, _ := doRequest(t, handler, http.MethodPost, "/api/circuits/C-A/restore", ""); status != http.StatusConflict {
		t.Fatalf("restore without maintenance status = %d", status)
	}
}

func TestOpenValveRefusesIsolationEndpoint(t *testing.T) {
	server, manager, valves := circuitAPIFixture(t)
	handler := server.Handler()

	reservation, err := manager.Reserve("C-A", "V-01", uuid.New(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	valves.open[reservation.SessionID.String()] = true

	status, body := doRequest(t, handler, http.MethodPost, "/api/circuits/C-A/deactivate", `{"reason":"emergency"}`)
	if status != http.StatusAccepted {
		t.Fatalf("deactivate status = %d body=%v", status, body)
	}
	if body["isolation_pending"] != true {
		t.Fatalf("open valve should report isolation pending: %v", body)
	}
	if message, _ := body["message"].(string); !strings.Contains(message, "not yet isolated") {
		t.Fatalf("expected pending-isolation message, got %v", body)
	}

	status, body = doRequest(t, handler, http.MethodPost, "/api/circuits/C-A/restore", "")
	if status != http.StatusConflict {
		t.Fatalf("restore status = %d", status)
	}
	if message, _ := body["error"].(string); !strings.Contains(message, "valve is still open") {
		t.Fatalf("expected open-valve refusal, got %v", body)
	}
}
