// Package meter holds the live shared state that the Shelly emulator
// serves to its callers. It is updated by a background poller that reads
// from a FRITZ!Box, runs every sample through a safeguard, and applies
// the user-selected phase mapping if (and only if) the safeguard accepts
// the sample.
package meter

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/flunaras/shelly-fritz-proxy/internal/battery"
	"github.com/flunaras/shelly-fritz-proxy/internal/forecast"
	"github.com/flunaras/shelly-fritz-proxy/internal/fritz"
	"github.com/flunaras/shelly-fritz-proxy/internal/safeguard"
)

// PhaseMode determines how the single FRITZ!Smart Energy 250 channel is
// mapped onto the three phases that a Shelly Pro 3EM reports.
type PhaseMode int

const (
	// PhaseSplit divides the measured total evenly across phase A/B/C.
	// Mathematically wrong on unbalanced loads but the most compatible
	// option for clients (Solakon ONE included) that only sum the three
	// phases anyway.
	PhaseSplit PhaseMode = iota
	// PhaseA puts everything on phase A and leaves B/C at zero. Most
	// "honest" representation but may break clients that require all
	// three phases to be non-null.
	PhaseA
)

// ParsePhaseMode maps a config string to a PhaseMode.
func ParsePhaseMode(s string) (PhaseMode, error) {
	switch s {
	case "", "split":
		return PhaseSplit, nil
	case "a", "phase-a", "single":
		return PhaseA, nil
	}
	return 0, errors.New("phase mode must be 'split' or 'a'")
}

// Phase is the per-phase view that Shelly Pro 3EM exposes.
type Phase struct {
	Voltage     float64 // V
	Current     float64 // A
	ActivePower float64 // W (signed: + import, - export)
	Frequency   float64 // Hz
}

// State is a snapshot of the meter that Shelly endpoints read on every
// request. It is copied out under lock so callers can encode it lock-free.
type State struct {
	A, B, C       Phase
	TotalCurrent  float64 // A
	TotalPower    float64 // W (sum of phases)
	TotalEnergy   float64 // Wh, cumulative
	EnergyIn      float64 // Wh import
	EnergyOut     float64 // Wh export
	Updated       time.Time
	HaveFrequency bool
	HaveVoltage   bool
}

// HealthSnapshot exposes the safeguard's view to the Shelly server and
// the /healthz endpoint without leaking the guard's internal types.
type HealthSnapshot struct {
	Health             safeguard.Health
	Reason             safeguard.Reason
	LastAcceptedAge    time.Duration
	StuckRunLength     int
	ConsecutiveRejects int
	// True once at least one sample has ever been accepted (whether or
	// not the state is currently degraded/stale).
	HasEverHadData bool
	// Product-name guard satisfied (i.e. the configured unit is on an
	// allowlisted product). False before the first device-info refresh
	// or if the device is unknown/disallowed.
	ProductOk bool
}

// Store holds the latest State, the safeguard's state machine, and the
// background polling loop.
type Store struct {
	client *fritz.Client
	unitID string // FRITZ! Smart Home unit UID (equals AIN for DECT-ULE meters)
	mode   PhaseMode
	logger *slog.Logger
	period time.Duration
	guard  *safeguard.Guard

	mu         sync.RWMutex
	state      State
	lastDevice *fritz.DeviceInfo
	health     HealthSnapshot
	productOk  bool
	hasData    atomic.Bool

	// Forecast mode (see internal/forecast): optional compensation for
	// consumers that poll far faster than FRITZ!Box refreshes its
	// cache. Nil/empty fields mean forecast mode is disabled.
	forecastEngine *forecast.Engine
	batteries      []battery.Reader
	batteryTimeout time.Duration
}

// NewStore prepares a Store that will poll the given unit UID at the
// given interval and run every sample through guard. Call Run from a
// goroutine to start polling.
//
// For an FSE 250 connected via DECT-ULE, the unit UID is the same
// 12-digit number printed on the device sticker (the AIN), with or
// without its internal space -- both forms are normalized to the same
// value (see fritz.NormalizeAIN). The same UID is used to fetch device
// metadata for the product-name allowlist check.
func NewStore(client *fritz.Client, unitUID string, mode PhaseMode, period time.Duration, guard *safeguard.Guard, logger *slog.Logger) *Store {
	if guard == nil {
		guard = safeguard.NewGuard(safeguard.DefaultConfig())
	}
	return &Store{
		client: client,
		unitID: unitUID,
		mode:   mode,
		logger: logger,
		period: period,
		guard:  guard,
	}
}

// EnableForecastMode turns on forecast-mode compensation (see
// internal/forecast) using the given battery.Reader devices. It must be
// called before Run starts polling; it is not safe to call
// concurrently with Run.
//
// batteryTimeout bounds how long each device's ReadPowerWatts call may
// take per poll cycle; if 0, a 5s default is used. A device that times
// out or errors is treated as contributing 0 W for that cycle and is
// logged at Warn level -- forecast mode degrades to "no compensation"
// for that cycle rather than blocking the whole poll or crashing.
func (s *Store) EnableForecastMode(readers []battery.Reader, batteryTimeout time.Duration) {
	if batteryTimeout <= 0 {
		batteryTimeout = 5 * time.Second
	}
	s.forecastEngine = forecast.New()
	s.batteries = readers
	s.batteryTimeout = batteryTimeout
}

// ForecastEnabled reports whether EnableForecastMode has been called.
func (s *Store) ForecastEnabled() bool {
	return s.forecastEngine != nil
}

// SetProductOk is called by the startup product-name check (see
// cmd/shelly-fritz-proxy) to tell the store that the configured unit
// passed the allowlist. The flag flows through into HealthSnapshot.
func (s *Store) SetProductOk(ok bool) {
	s.mu.Lock()
	s.productOk = ok
	s.mu.Unlock()
}

// Run polls FRITZ until ctx is cancelled.
func (s *Store) Run(ctx context.Context) {
	t := time.NewTicker(s.period)
	defer t.Stop()
	// Refresh device metadata once at startup; we use isConnected per
	// poll but the product name and friendly name are stable, so we do
	// not need to refetch them.
	s.refreshDevice(ctx)
	// Fetch once up front so the HTTP API has data immediately.
	s.refresh(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.refresh(ctx)
		}
	}
}

func (s *Store) refreshDevice(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	// GetDevice wants the owning physical device's UID, which differs
	// from s.unitID whenever the configured unit UID carries a "-<n>"
	// suffix (e.g. an FSE 250 whose firmware splits metering into
	// "<AIN>-1"/"<AIN>-2"). Without this translation, GetDevice would
	// permanently fail with "UID_NOT_FOUND" for such devices, leaving
	// lastDevice nil forever and silently defeating the
	// RequireConnected safeguard (it only checks devInfo when non-nil).
	// See fritz.ParentDeviceUID.
	dev, err := s.client.GetDevice(rctx, fritz.ParentDeviceUID(s.unitID))
	if err != nil {
		s.logger.Warn("fritz device metadata fetch failed", "err", err)
		return
	}
	s.mu.Lock()
	s.lastDevice = dev
	s.mu.Unlock()
}

func (s *Store) refresh(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	m, err := s.client.GetMeasurement(rctx, s.unitID)
	now := time.Now()
	if err != nil {
		s.logger.Warn("fritz poll failed", "err", err)
		res := s.guard.NotePollFailure(now)
		if s.forecastEngine != nil {
			s.forecastEngine.Reset()
		}
		s.updateHealthOnly(res)
		return
	}
	// Periodically refresh the device info too so RequireConnected stays
	// accurate. Do it lazily every ~30 polls.
	if s.shouldRefreshDevice() {
		go s.refreshDevice(ctx)
	}
	s.mu.RLock()
	dev := s.lastDevice
	s.mu.RUnlock()

	res := s.guard.Evaluate(now, m, dev)
	switch res.Verdict {
	case safeguard.Accept:
		var batterySum float64
		if s.forecastEngine != nil {
			batterySum = s.readBatterySum(ctx)
		}
		s.apply(m, res, batterySum)
	default:
		// Reject or Stale: keep last good State, but advance health.
		// Also drop the forecast baseline: whatever it last
		// remembered may no longer correspond to reality once real
		// samples resume, so the next accepted sample should be
		// trusted outright rather than compared against a
		// potentially stale/irrelevant baseline.
		if s.forecastEngine != nil {
			s.forecastEngine.Reset()
		}
		s.updateHealthOnly(res)
		s.logger.Debug("fritz sample rejected", "reason", res.Reason, "health", res.Health, "stuck_run", res.StuckRunLength)
	}
}

// readBatterySum queries every configured battery.Reader and returns
// the sum of their current power outputs. A device that errors or
// times out contributes 0 W for this cycle and is logged at Warn --
// forecast mode degrades to "no compensation from that device" rather
// than blocking the whole poll cycle or propagating the error.
func (s *Store) readBatterySum(ctx context.Context) float64 {
	var sum float64
	for i, r := range s.batteries {
		rctx, cancel := context.WithTimeout(ctx, s.batteryTimeout)
		w, err := r.ReadPowerWatts(rctx)
		cancel()
		if err != nil {
			s.logger.Warn("forecast mode: battery device read failed; treating as 0W this cycle",
				"device_index", i, "err", err)
			continue
		}
		sum += w
	}
	return sum
}

// shouldRefreshDevice returns true ~once every 30 polls so the
// RequireConnected check stays accurate without doing a second HTTP
// round trip on every cycle.
var deviceRefreshCounter atomic.Int64

func (s *Store) shouldRefreshDevice() bool {
	return deviceRefreshCounter.Add(1)%30 == 0
}

func (s *Store) updateHealthOnly(res safeguard.EvaluateResult) {
	s.mu.Lock()
	s.health.Health = res.Health
	s.health.Reason = res.Reason
	s.health.LastAcceptedAge = res.LastAcceptedAge
	s.health.StuckRunLength = res.StuckRunLength
	s.health.ConsecutiveRejects = res.ConsecutiveRejects
	s.health.HasEverHadData = s.hasData.Load()
	s.health.ProductOk = s.productOk
	s.mu.Unlock()
}

func (s *Store) apply(m *fritz.Measurement, res safeguard.EvaluateResult, batterySum float64) {
	st := State{
		Updated: m.Updated,
	}
	voltage := m.Voltage
	if voltage == 0 || math.IsNaN(voltage) {
		voltage = 230.0 // nominal EU mains, only used to derive currents
	} else {
		st.HaveVoltage = true
	}
	freq := m.Frequency
	if freq == 0 {
		freq = 50.0
	} else {
		st.HaveFrequency = true
	}

	// Forecast mode (see internal/forecast): compensate for a FRITZ
	// sample that has not actually refreshed since the last cycle by
	// backing out however much the configured battery devices' own
	// output has moved in the meantime. When disabled, power is used
	// unchanged. This adjustment happens *after* the safeguard has
	// already accepted m as a genuine (non-stale, non-stuck,
	// plausible) sample -- forecast mode is a separate concern from
	// the safeguard's own stuck-cache detection, and operates only on
	// the reported value, never feeding back into the safeguard
	// itself.
	power := m.ActivePower
	if s.forecastEngine != nil {
		power = s.forecastEngine.Adjust(m.RawPower, m.ActivePower, batterySum)
	}

	switch s.mode {
	case PhaseA:
		st.A = phaseFromTotal(power, voltage, freq)
		st.B = Phase{Voltage: voltage, Frequency: freq}
		st.C = Phase{Voltage: voltage, Frequency: freq}
	default: // PhaseSplit
		third := power / 3.0
		st.A = phaseFromTotal(third, voltage, freq)
		st.B = phaseFromTotal(third, voltage, freq)
		st.C = phaseFromTotal(third, voltage, freq)
	}

	st.TotalPower = st.A.ActivePower + st.B.ActivePower + st.C.ActivePower
	st.TotalCurrent = st.A.Current + st.B.Current + st.C.Current
	st.TotalEnergy = m.Energy
	st.EnergyIn = m.EnergyIn
	st.EnergyOut = m.EnergyOut

	s.mu.Lock()
	s.state = st
	s.health.Health = res.Health
	s.health.Reason = res.Reason
	s.health.LastAcceptedAge = res.LastAcceptedAge
	s.health.StuckRunLength = res.StuckRunLength
	s.health.ConsecutiveRejects = res.ConsecutiveRejects
	s.health.HasEverHadData = true
	s.health.ProductOk = s.productOk
	s.mu.Unlock()
	s.hasData.Store(true)
}

func phaseFromTotal(power, voltage, freq float64) Phase {
	p := Phase{
		Voltage:     voltage,
		ActivePower: power,
		Frequency:   freq,
	}
	if voltage > 0 {
		p.Current = math.Abs(power) / voltage
	}
	return p
}

// Snapshot returns a copy of the current State together with the
// safeguard health snapshot. ok reports whether at least one sample
// has ever been accepted.
func (s *Store) Snapshot() (State, HealthSnapshot, bool) {
	if !s.hasData.Load() {
		// Return health even before first acceptance so callers can
		// tell the difference between "still warming up" and
		// "definitely stale".
		s.mu.RLock()
		h := s.health
		s.mu.RUnlock()
		return State{}, h, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state, s.health, true
}
