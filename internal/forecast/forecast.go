// Package forecast implements "forecast mode": a compensation layer
// that counteracts a control-loop escalation that occurs when a
// consumer (e.g. Solakon ONE) polls this proxy's Shelly emulation much
// faster than the upstream FRITZ!Box actually refreshes its cached
// measurement.
//
// The problem (see the worked example in the project's design notes):
// a naive consumer reads "50 W import", increases its own export by
// 50 W, reads the *same stale* 50 W again (because FRITZ hasn't
// refreshed yet), and increases by another 50 W, escalating without
// bound until FRITZ finally refreshes.
//
// The fix: track, alongside every genuinely-new FRITZ reading, the
// sum of the currently configured solar-battery devices' own power
// output at that moment. As long as the FRITZ reading stays byte-equal
// to what was last seen (i.e. is almost certainly a stale cache, not a
// coincidentally-unchanged household load), report a value that
// backs out however much the consumer's own output has moved since
// then:
//
//	adjusted = remembered_fritz - current_battery_sum + remembered_battery_sum
//
// This is exactly it "correcting itself": if the battery sum has
// grown by X since the last genuine FRITZ reading, the reported value
// shrinks by X, so a consumer that increases output in proportion to
// what it reads converges instead of escalating.
package forecast

import "sync"

// Engine holds the running state needed to compute the adjustment. It
// is safe for concurrent use, though this proxy only ever drives it
// from the single poller goroutine.
type Engine struct {
	mu          sync.Mutex
	initialized bool
	// lastRawFritz is the last-seen raw (byte-comparable) FRITZ power
	// reading, used purely to detect "the FRITZ cache has not
	// refreshed since last time" -- comparing the *raw* integer
	// (mirroring the safeguard's own stuck-cache detector) rather than
	// the float watts value avoids any risk of float round-tripping
	// masking a genuinely-unchanged reading as "changed" or vice versa.
	lastRawFritz int64
	// lastFritzWatts is the FRITZ power value (in watts) as of the
	// last genuinely-new reading; this is what gets "remembered" and
	// corrected against on subsequent stale cycles.
	lastFritzWatts float64
	// lastBatteryWatts is the sum of all configured battery devices'
	// power output as of that same last genuinely-new reading.
	lastBatteryWatts float64
}

// New builds an empty Engine. The first call to Adjust always returns
// fritzWatts unchanged (there is nothing yet to compare against).
func New() *Engine {
	return &Engine{}
}

// Adjust returns the power value (in watts) that should actually be
// reported to consumers this cycle, given the latest raw FRITZ sample.
//
// rawFritz is a byte-comparable representation of the same underlying
// FRITZ reading as fritzWatts (typically fritz.Measurement.RawPower);
// it is used only to decide whether the FRITZ!Box has produced a
// genuinely new sample since the last call. fritzWatts is that
// reading converted to watts. batterySum is the sum of
// battery.Reader.ReadPowerWatts across all configured devices, sampled
// as close as possible to the same instant as the FRITZ reading.
//
// Call Adjust once per accepted FRITZ sample (i.e. from the same place
// the safeguard's Accept verdict is handled), never for
// rejected/stale-per-safeguard samples -- those already have their own
// handling via the configured stale-policy and should not also flow
// through the forecast corrector.
func (e *Engine) Adjust(rawFritz int64, fritzWatts, batterySum float64) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.initialized || rawFritz != e.lastRawFritz {
		// A genuinely new FRITZ sample: trust it outright, and
		// remember both readings as the new baseline for future
		// stale cycles.
		e.initialized = true
		e.lastRawFritz = rawFritz
		e.lastFritzWatts = fritzWatts
		e.lastBatteryWatts = batterySum
		return fritzWatts
	}

	// The FRITZ cache has not refreshed since the last call: back out
	// however much the battery's own output has moved since then.
	return e.lastFritzWatts - batterySum + e.lastBatteryWatts
}

// Reset clears the remembered baseline, forcing the next Adjust call
// to trust its input outright. Useful after a poll failure or a
// safeguard-rejected sample, where the remembered baseline may no
// longer correspond to reality.
func (e *Engine) Reset() {
	e.mu.Lock()
	e.initialized = false
	e.mu.Unlock()
}
