package main

import "github.com/prometheus/client_golang/prometheus"

type metrics struct {
	registry          *prometheus.Registry
	timeshiftsActive  prometheus.Gauge
	injectTotal       *prometheus.CounterVec
	setTimeTotal      *prometheus.CounterVec
	sweepExpiredTotal prometheus.Counter
	apiRequestsTotal  *prometheus.CounterVec
}

func newMetrics() *metrics {
	reg := prometheus.NewRegistry()
	m := &metrics{
		registry: reg,
		timeshiftsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "epochd_timeshifts_active",
			Help: "Number of timeshifts currently in the registry.",
		}),
		injectTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "epochd_inject_total",
			Help: "Container injections attempted, by result.",
		}, []string{"result"}),
		setTimeTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "epochd_settime_total",
			Help: "SetTime RPC calls made to agents, by result.",
		}, []string{"result"}),
		sweepExpiredTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "epochd_sweep_expired_total",
			Help: "Timeshifts removed by the TTL sweeper.",
		}),
		apiRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "epochd_api_requests_total",
			Help: "HTTP requests handled by the controller API.",
		}, []string{"method", "path", "status"}),
	}
	reg.MustRegister(
		m.timeshiftsActive,
		m.injectTotal,
		m.setTimeTotal,
		m.sweepExpiredTotal,
		m.apiRequestsTotal,
	)
	return m
}
