package observability

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ismoilovdevml/hangar/internal/audit"
	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/core"
)

// authMethods and authReasons are the label values of
// hangar_auth_failures_total; anything else is "other".
var (
	authMethods = map[string]bool{auth.MethodLogin: true, auth.MethodBasic: true, auth.MethodToken: true}
	authReasons = map[string]bool{
		"malformed": true, "unknown_user": true, "no_local_password": true, "disabled": true,
		"locked": true, "bad_password": true, "unknown_expired_or_revoked": true,
	}
)

// ObserveAuth wraps the audit log given to the auth service: every failed
// authentication it records (auth.login.failed, auth.token.failed) is
// counted in hangar_auth_failures_total, and attempts that locked an
// account in hangar_auth_lockouts_total. Records pass through unchanged.
func ObserveAuth(next core.AuditLog, m *Metrics) core.AuditLog {
	if m == nil {
		return next
	}
	return authObserver{next: next, m: m}
}

type authObserver struct {
	next core.AuditLog
	m    *Metrics
}

func (o authObserver) Record(ctx context.Context, e core.AuditEvent) error {
	if e.Action == auth.AuditLoginFailed || e.Action == auth.AuditTokenAuthFailed {
		method, _ := e.Details["method"].(string)
		reason, _ := e.Details["reason"].(string)
		if !authMethods[method] {
			method = labelOther
		}
		if !authReasons[reason] {
			reason = labelOther
		}
		o.m.AuthFailures.WithLabelValues(method, reason).Inc()
		if _, locked := e.Details["locked_until"]; locked && reason == "bad_password" {
			o.m.AuthLockouts.Inc()
		}
	}
	return o.next.Record(ctx, e)
}

// RegisterAudit adds the audit writer's queue length, capacity and drop
// counters.
func (m *Metrics) RegisterAudit(l *audit.Log) error {
	if m == nil || l == nil {
		return nil
	}
	return m.Registry.Register(auditCollector{l})
}

var (
	descAuditQueued = prometheus.NewDesc("hangar_audit_queue_length",
		"Audit events waiting to be written to PostgreSQL.", nil, nil)
	descAuditCapacity = prometheus.NewDesc("hangar_audit_queue_capacity",
		"Size of the in-memory audit queue.", nil, nil)
	descAuditRejected = prometheus.NewDesc("hangar_audit_events_rejected_total",
		"Audit events refused because the queue stayed full (the mutation failed with backpressure).", nil, nil)
	descAuditLost = prometheus.NewDesc("hangar_audit_events_lost_total",
		"Audit events that could not be stored after every retry (written to the log instead).", nil, nil)
)

type auditCollector struct{ l *audit.Log }

func (auditCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descAuditQueued
	ch <- descAuditCapacity
	ch <- descAuditRejected
	ch <- descAuditLost
}

func (c auditCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.l.Stats()
	ch <- prometheus.MustNewConstMetric(descAuditQueued, prometheus.GaugeValue, float64(s.Queued))
	ch <- prometheus.MustNewConstMetric(descAuditCapacity, prometheus.GaugeValue, float64(s.Capacity))
	ch <- prometheus.MustNewConstMetric(descAuditRejected, prometheus.CounterValue, float64(s.Rejected))
	ch <- prometheus.MustNewConstMetric(descAuditLost, prometheus.CounterValue, float64(s.Lost))
}
