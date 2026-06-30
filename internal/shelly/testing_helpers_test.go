package shelly

import (
	"testing"

	"github.com/flunaras/shelly-fritz-proxy/internal/fritz"
	"github.com/flunaras/shelly-fritz-proxy/internal/meter"
)

// injectMeasurement is a tiny test-only shim so server_test.go can preload
// the store without spinning up the polling goroutine.
func injectMeasurement(t *testing.T, store *meter.Store, m *fritz.Measurement) {
	t.Helper()
	store.Apply(m)
}
