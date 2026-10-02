package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wyw14/cry-98/internal/circuit"
)

type deactivateCircuitRequest struct {
	Reason string `json:"reason"`
}

type recordCircuitPressureRequest struct {
	PressureKPa float64   `json:"pressure_kpa"`
	ObservedAt  time.Time `json:"observed_at"`
}

func (s *Server) listCircuits(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"items": s.deps.Circuits.List()})
}

func (s *Server) getCircuitMaintenance(writer http.ResponseWriter, request *http.Request) {
	status, err := s.deps.CircuitMaintenance.Status(chi.URLParam(request, "id"))
	if err != nil {
		writeCircuitError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, status)
}

func (s *Server) deactivateCircuit(writer http.ResponseWriter, request *http.Request) {
	var input deactivateCircuitRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	status, err := s.deps.CircuitMaintenance.Deactivate(chi.URLParam(request, "id"), input.Reason)
	if err != nil {
		writeCircuitError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, status)
}

func (s *Server) restoreCircuit(writer http.ResponseWriter, request *http.Request) {
	status, err := s.deps.CircuitMaintenance.Restore(chi.URLParam(request, "id"))
	if err != nil {
		writeCircuitError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, status)
}

func (s *Server) recordCircuitPressure(writer http.ResponseWriter, request *http.Request) {
	var input recordCircuitPressureRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	id := chi.URLParam(request, "id")
	if err := s.deps.CircuitMaintenance.RecordPressure(id, input.PressureKPa, input.ObservedAt); err != nil {
		writeCircuitError(writer, err)
		return
	}
	updated, _ := s.deps.Circuits.Get(id)
	writeJSON(writer, http.StatusOK, updated)
}

func writeCircuitError(writer http.ResponseWriter, err error) {
	status := http.StatusConflict
	switch {
	case errors.Is(err, circuit.ErrCircuitNotFound):
		status = http.StatusNotFound
	case errors.Is(err, circuit.ErrReasonRequired), errors.Is(err, circuit.ErrInvalidReading):
		status = http.StatusBadRequest
	}
	writeError(writer, status, err.Error())
}
