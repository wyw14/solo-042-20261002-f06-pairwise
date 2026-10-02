package monitor

import (
	"time"

	"github.com/wyw14/cry-98/internal/circuit"
	"github.com/wyw14/cry-98/internal/telemetry"
)

type HealthReport struct {
	Status      string           `json:"status"`
	CheckedAt   time.Time        `json:"checked_at"`
	Telemetry   telemetry.Health `json:"telemetry"`
	Circuits    []circuit.Health `json:"circuits"`
	ProbeIssues []ProbeHealth    `json:"probe_issues"`
}

type HealthService struct {
	telemetry *telemetry.Ingestor
	circuits  *circuit.HealthService
	probes    *ProbeMonitor
	now       func() time.Time
}

func NewHealthService(ingestor *telemetry.Ingestor, circuits *circuit.HealthService, probes *ProbeMonitor, now func() time.Time) *HealthService {
	if now == nil {
		now = time.Now
	}
	return &HealthService{telemetry: ingestor, circuits: circuits, probes: probes, now: now}
}

func (s *HealthService) Report() HealthReport {
	report := HealthReport{
		Status:      "ok",
		CheckedAt:   s.now().UTC(),
		Telemetry:   s.telemetry.Health(),
		Circuits:    s.circuits.All(),
		ProbeIssues: s.probes.Disconnected(),
	}
	if !report.Telemetry.Healthy || len(report.ProbeIssues) > 0 {
		report.Status = "degraded"
	}
	for _, health := range report.Circuits {
		// A reserved circuit is busy with a normal refill and a circuit in
		// settled maintenance is intentionally out of service; anything else
		// unavailable (fault, pending isolation, disabled) is degradation.
		if !health.Available && health.State != "reserved" && health.State != "maintenance" {
			report.Status = "degraded"
		}
	}
	return report
}
