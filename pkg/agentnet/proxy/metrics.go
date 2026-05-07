package proxy

// ProxyMetrics is the minimal observability interface — the production
// path wires Prometheus counters; tests pass a noop. Keeping it small
// keeps the proxy package free of metric library imports.
type ProxyMetrics interface {
	DialOK(resource string)
	DialError(resource, reason string)
}

// NoopMetrics ignores everything. Default when nil.
type NoopMetrics struct{}

func (NoopMetrics) DialOK(_ string)       {}
func (NoopMetrics) DialError(_, _ string) {}

// metricsOf returns m or a NoopMetrics if m is nil. Lets the proxy
// code call methods unconditionally.
func metricsOf(m ProxyMetrics) ProxyMetrics {
	if m == nil {
		return NoopMetrics{}
	}
	return m
}
