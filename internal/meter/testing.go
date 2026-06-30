package meter

import (
	"context"
	"time"

	"github.com/flunaras/shelly-fritz-proxy/internal/fritz"
	"github.com/flunaras/shelly-fritz-proxy/internal/safeguard"
)

// Apply is exposed only for tests in sibling packages. It feeds the
// given measurement through the guard exactly as the poller would, so
// the store ends up in a realistic state. Do not call from production
// code.
func (s *Store) Apply(m *fritz.Measurement) {
	now := time.Now()
	if m.Updated.IsZero() {
		m.Updated = now
	}
	res := s.guard.Evaluate(now, m, nil)
	if res.Verdict == safeguard.Accept {
		var batterySum float64
		if s.forecastEngine != nil {
			batterySum = s.readBatterySum(context.Background())
		}
		s.apply(m, res, batterySum)
	} else {
		s.updateHealthOnly(res)
	}
}
