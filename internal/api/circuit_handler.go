package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/wyw14/cry-98/internal/circuit"
)

type recordPressureRequest struct {
	PressureKPa float64 `json:"pressure_kpa"`
}

type deactivateCircuitRequest struct {
	Reason string `json:"reason"`
}

type maintenanceResponse struct {
	CircuitID   string               `json:"circuit_id"`
	Status      string               `json:"status"`
	Maintenance *circuit.Maintenance `json:"maintenance,omitempty"`
	// IsolationPending is true when deactivation was accepted but an active
	// refill keeps the circuit from being treated as isolated.
	IsolationPending bool   `json:"isolation_pending,omitempty"`
	Message          string `json:"message,omitempty"`
}

func (s *Server) listCircuits(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"items": s.deps.Circuits.List()})
}

func (s *Server) recordCircuitPressure(writer http.ResponseWriter, request *http.Request) {
	id := chi.URLParam(request, "id")
	var input recordPressureRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	if input.PressureKPa < 0 {
		writeError(writer, http.StatusBadRequest, "pressure reading cannot be negative")
		return
	}
	updated, err := s.deps.Circuits.RecordPressure(id, input.PressureKPa, s.deps.Now())
	if err != nil {
		writeCircuitError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, updated)
}

func (s *Server) deactivateCircuit(writer http.ResponseWriter, request *http.Request) {
	id := chi.URLParam(request, "id")
	var input deactivateCircuitRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	pending, err := s.deps.Maintenance.Deactivate(id, input.Reason)
	response := maintenanceResponse{
		CircuitID:        id,
		Status:           "deactivated",
		IsolationPending: pending,
	}
	if record, ok := s.deps.Maintenance.Maintenance(id); ok {
		response.Maintenance = &record
	}
	if pending {
		response.Message = "circuit is deactivated for maintenance but an active refill is not yet isolated"
	}
	if err != nil {
		// The circuit is already under maintenance; report its current state.
		if errors.Is(err, circuit.ErrMaintenanceActive) {
			writeJSON(writer, http.StatusOK, response)
			return
		}
		writeCircuitError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, response)
}

func (s *Server) restoreCircuit(writer http.ResponseWriter, request *http.Request) {
	id := chi.URLParam(request, "id")
	if err := s.deps.Maintenance.Restore(id); err != nil {
		writeCircuitError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, maintenanceResponse{CircuitID: id, Status: "restored"})
}

func writeCircuitError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, circuit.ErrCircuitNotFound):
		writeError(writer, http.StatusNotFound, err.Error())
	case errors.Is(err, circuit.ErrInvalidMaintenanceRequest):
		writeError(writer, http.StatusBadRequest, err.Error())
	case errors.Is(err, circuit.ErrNotInMaintenance),
		errors.Is(err, circuit.ErrMaintenanceActive),
		errors.Is(err, circuit.ErrMaintenanceDeactivated):
		writeError(writer, http.StatusConflict, err.Error())
	default:
		// Restore rule refusals arrive as plain descriptive errors so the
		// pressure, fault, open-valve and active-fill reasons stay visible.
		writeError(writer, http.StatusConflict, err.Error())
	}
}
