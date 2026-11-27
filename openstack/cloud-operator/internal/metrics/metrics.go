// Package metrics registers Prometheus metrics on controller-runtime's registry.
package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	ReconcileTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "cloud_operator",
			Name:      "reconcile_total",
			Help:      "Total number of reconciliations partitioned by controller and outcome.",
		},
		[]string{"controller", "result"},
	)

	ReconcileDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "cloud_operator",
			Name:      "reconcile_duration_seconds",
			Help:      "Duration of controller reconciliations in seconds.",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"controller"},
	)

	DriftHealsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "cloud_operator",
			Name:      "drift_heals_total",
			Help:      "Total number of out-of-band cloud configuration drifts automatically healed.",
		},
		[]string{"controller", "action"},
	)

	LiveMigrationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "cloud_operator",
			Name:      "live_migrations_total",
			Help:      "Total number of zero-copy Nova live migrations triggered over shared Ceph RBD.",
		},
		[]string{"result"},
	)

	ManagedResources = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "cloud_operator",
			Name:      "managed_resources",
			Help:      "Current readiness (1=ready, 0=not ready) of managed IaaS resources.",
		},
		[]string{"kind", "namespace", "name"},
	)
)

var registerOnce = sync.OnceFunc(func() {
	ctrlmetrics.Registry.MustRegister(
		ReconcileTotal,
		ReconcileDurationSeconds,
		DriftHealsTotal,
		LiveMigrationsTotal,
		ManagedResources,
	)
})

// Register registers all operator Prometheus collectors idempotently.
func Register() {
	registerOnce()
}
