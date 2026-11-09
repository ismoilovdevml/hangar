package routing

import "github.com/prometheus/client_golang/prometheus"

// BlockedCounter exposes the exclusion counter to tests.
func BlockedCounter() *prometheus.CounterVec { return blocked }
