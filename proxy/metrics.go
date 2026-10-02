package proxy

import (
	"io"
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	totalRequest = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "goproxy",
		Subsystem: "router",
		Name:      "request_total",
		Help:      "total request in HTTP",
	}, []string{"mode", "status"})
)

func init() {
	prometheus.MustRegister(totalRequest)
}

type metricsResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

// NewMetricsResponseWriter creates custom metrics response writer.
func NewMetricsResponseWriter(w http.ResponseWriter) *metricsResponseWriter {
	// WriteHeader(int) is not called if our response implicitly returns 0, so
	// we default to that status code.
	return &metricsResponseWriter{w, 0}
}

// WriteHeader implements http.ResponseWriter.
func (mw *metricsResponseWriter) WriteHeader(code int) {
	mw.statusCode = code
	mw.ResponseWriter.WriteHeader(code)
}

// status returns the status code label.
func (mw *metricsResponseWriter) status() string {
	return strconv.Itoa(mw.statusCode)
}

// MetricsMiddleware counts every request handled by next under the given
// mode label, for handler chains that do not count internally (proxy mode).
func MetricsMiddleware(mode string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mw := NewMetricsResponseWriter(w)
		next.ServeHTTP(mw, r)
		totalRequest.WithLabelValues(mode, mw.status()).Inc()
	})
}

// ReadFrom forwards to the underlying writer to keep the sendfile fast path.
func (mw *metricsResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	if rf, ok := mw.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(mw.ResponseWriter, r)
}

// Flush implements http.Flusher to keep reverse proxy streaming.
func (mw *metricsResponseWriter) Flush() {
	if f, ok := mw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying ResponseWriter to net/http.
func (mw *metricsResponseWriter) Unwrap() http.ResponseWriter {
	return mw.ResponseWriter
}
