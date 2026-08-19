// Package obs holds process observability: the metrics registry and the
// readiness model that the HTTP health endpoints report on.
package obs

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics owns the registry and the collectors shared across subsystems.
// A dedicated registry (rather than prometheus.DefaultRegisterer) keeps tests
// independent: each one can build its own Metrics without colliding on
// duplicate registration.
type Metrics struct {
	Registry *prometheus.Registry

	BuildInfo *prometheus.GaugeVec
}

// NewMetrics builds a registry preloaded with Go runtime and process collectors.
func NewMetrics(version, commit string) *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m := &Metrics{
		Registry: reg,
		BuildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "imapped_build_info",
			Help: "Build metadata; the value is always 1.",
		}, []string{"version", "commit"}),
	}
	reg.MustRegister(m.BuildInfo)
	m.BuildInfo.WithLabelValues(version, commit).Set(1)
	return m
}

// Health tracks per-subsystem readiness. Liveness answers "is the process
// running"; readiness answers "can it serve traffic", which is the distinction
// that lets a container orchestrator restart a wedged process rather than
// merely routing around it.
type Health struct {
	mu     sync.RWMutex
	checks map[string]error
}

// NewHealth returns a Health with the named subsystems registered as not-yet-ready.
func NewHealth(subsystems ...string) *Health {
	h := &Health{checks: make(map[string]error, len(subsystems))}
	for _, name := range subsystems {
		h.checks[name] = errNotReady
	}
	return h
}

// Set records the current state of a subsystem. A nil error means ready.
func (h *Health) Set(subsystem string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.checks[subsystem] = err
}

// Ready reports whether every registered subsystem is healthy, along with the
// per-subsystem detail so the endpoint can say which one is failing.
func (h *Health) Ready() (bool, map[string]error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	detail := make(map[string]error, len(h.checks))
	ready := true
	for name, err := range h.checks {
		detail[name] = err
		if err != nil {
			ready = false
		}
	}
	return ready, detail
}

type notReadyError struct{}

func (notReadyError) Error() string { return "not ready" }

var errNotReady = notReadyError{}
