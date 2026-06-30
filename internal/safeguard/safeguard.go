// Package safeguard implements the runtime guards that prevent a Solakon ONE
// (or any other consumer of the emulated Shelly Pro 3 EM) from acting on
// stale, frozen, or implausible measurements coming from a FRITZ!Smart
// Energy 250.
//
// The FRITZ!Smart Energy 250 is a DECT-ULE bidirectional energy meter.
// In practice it exhibits several known failure modes that, left
// unmitigated, would cause downstream controllers to mis-regulate inverter
// output:
//
//   1. *Stuck cache*. The FRITZ! Smart Home REST API periodically returns
//      byte-for-byte identical readings for many polls in a row because
//      the FRITZ!Box's internal cache has not refreshed (the
//      `solakon-one-fritz-powerregulator` project documents this as a
//      regularly-observed real-world failure mode and addresses it with
//      its --fritz-stuck-cycles guard). On signed power this is the most
//      reliable signal that the meter is frozen.
//   2. *Transmission stall*. The DECT-ULE radio link drops out; the
//      FRITZ!Box keeps serving the last known measurement without
//      flipping `isConnected` for tens of seconds.
//   3. *Implausible glitch*. A single sample reports an out-of-range
//      voltage / current / power. Acting on a 32 kW spike to "correct"
//      it is exactly the kind of failure the safeguards exist to
//      prevent.
//   4. *Network / auth failure*. The REST call returned an error or the
//      payload was malformed. The poller already logs and skips these,
//      but each one bumps a contiguous-error counter so we know when to
//      degrade.
//
// The safeguard works at sample-application time: every incoming
// fritz.Measurement is evaluated by Evaluate; the result classifies the
// sample as Accept (apply normally), Reject (discard this sample but
// keep last good), or Stale (declare the state unsafe and let the
// consumer apply its configured fail-safe policy). The classification
// is sticky across cycles via a small state machine and a recovery
// hold-down counter so a single good sample does not immediately clear
// a prolonged stall.
package safeguard

import (
	"fmt"
	"strings"
	"time"

	"github.com/flunaras/shelly-fritz-proxy/internal/fritz"
)

// Config describes the safeguard policy. All fields are independently
// optional: leaving a value at zero disables that guard.
type Config struct {
	// MaxAge declares the maximum wall-clock distance between now and
	// the last accepted sample's Updated stamp before the state is
	// considered stale. Default 30s.
	MaxAge time.Duration

	// StuckCycles is the number of consecutive byte-equal samples (on
	// RawPower in particular) that triggers a "stuck data" alarm. The
	// FSE 250's DECT-ULE cache stall produces exactly this signature.
	// Default 5; set to 0 to disable.
	StuckCycles int

	// StuckMatchVoltage also requires byte-equal voltage to declare
	// the sample stuck. Without this, a constant DC load on a stable
	// grid will flap the guard. Recommended on. Default true.
	StuckMatchVoltage bool

	// MinVoltage / MaxVoltage clamp the plausible single-phase voltage
	// in volts. Samples outside this range are rejected. Defaults
	// 180 / 280 V (matches the Shelly Pro 3EM hardware safe range).
	MinVoltage float64
	MaxVoltage float64

	// MaxAbsPower is the largest believable instantaneous power in
	// watts. Defaults 30000 (30 kW; well above any residential FSE 250
	// install). Set to 0 to disable.
	MaxAbsPower float64

	// MaxStep is the largest plausible change in absolute active power
	// between two consecutive samples in watts. A single 32 kW glitch
	// next to a stable 500 W reading will trip this. Default 15000.
	// Set to 0 to disable.
	MaxStep float64

	// RecoverySamples requires N consecutive Accept-classified samples
	// after a Stale period before the state machine clears back to
	// Healthy. Default 3.
	RecoverySamples int

	// RequireConnected refuses to ever accept samples while the
	// FRITZ!Box reports the device as offline. Optional; default true.
	RequireConnected bool

	// AcceptedStates lists multimeterInterface.state values that are
	// treated as trustworthy. Defaults to {"valid"}. Anything not in
	// this set causes a Reject.
	AcceptedStates []string
}

// DefaultConfig returns the conservative defaults applied when no
// overrides are configured. Every value was chosen to fail safely: when
// in doubt, the proxy will rather report stale data than act on a bad
// sample.
func DefaultConfig() Config {
	return Config{
		MaxAge:            30 * time.Second,
		StuckCycles:       5,
		StuckMatchVoltage: true,
		MinVoltage:        180,
		MaxVoltage:        280,
		MaxAbsPower:       30000,
		MaxStep:           15000,
		RecoverySamples:   3,
		RequireConnected:  true,
		AcceptedStates:    []string{"valid"},
	}
}

// Verdict describes what the Guard decided about a single sample.
type Verdict int

const (
	// Accept the sample as the new last-known-good measurement.
	Accept Verdict = iota
	// Reject this single sample (likely a glitch); keep last-known-good
	// but do not advance freshness. After enough rejections in a row
	// the state will transition to Stale.
	Reject
	// Stale: the underlying data is no longer trustworthy at all.
	// Consumers should apply their configured stale policy.
	Stale
)

func (v Verdict) String() string {
	switch v {
	case Accept:
		return "accept"
	case Reject:
		return "reject"
	case Stale:
		return "stale"
	}
	return "?"
}

// Health summarises the overall state machine for inspection by the
// /healthz endpoint and structured logging.
type Health int

const (
	// HealthInit means no successful sample has been processed yet.
	HealthInit Health = iota
	// HealthOk means the most recent sample was accepted and we are
	// within the freshness budget. This is the only state that grants
	// a "200 OK" on /healthz.
	HealthOk
	// HealthDegraded means at least one recent sample was rejected
	// but we are still within the freshness budget on the last
	// accepted sample. Consumers should still trust the last value.
	HealthDegraded
	// HealthStale means we have not had a fresh trustworthy sample in
	// MaxAge. Consumers should apply their stale policy.
	HealthStale
)

func (h Health) String() string {
	switch h {
	case HealthInit:
		return "init"
	case HealthOk:
		return "ok"
	case HealthDegraded:
		return "degraded"
	case HealthStale:
		return "stale"
	}
	return "?"
}

// Reason names the most specific cause that produced a non-Accept
// verdict or a non-Ok health. Stable strings so they can flow through
// to logs, metrics, and the Shelly errors[] array unchanged.
type Reason string

const (
	ReasonNone               Reason = ""
	ReasonNoSamples          Reason = "no_samples_yet"
	ReasonPollFailure        Reason = "poll_failure"
	ReasonStateNotValid      Reason = "interface_state_invalid"
	ReasonDeviceOffline      Reason = "device_offline"
	ReasonOutOfRangeVoltage  Reason = "out_of_range_voltage"
	ReasonOutOfRangePower    Reason = "out_of_range_power"
	ReasonStepTooLarge       Reason = "step_too_large"
	ReasonStuckData          Reason = "stuck_data"
	ReasonFreshnessTimeout   Reason = "freshness_timeout"
)

// EvaluateResult describes the outcome of a single Evaluate call.
type EvaluateResult struct {
	Verdict Verdict
	Reason  Reason
	Health  Health
	// LastAcceptedAge is wall-clock duration since the most recent
	// Accepted sample's Updated timestamp. Zero if no sample has ever
	// been accepted.
	LastAcceptedAge time.Duration
	// StuckRunLength is the current run-length of byte-equal samples.
	StuckRunLength int
	// ConsecutiveRejects is the run-length of Reject verdicts since
	// the last Accept.
	ConsecutiveRejects int
}

// Guard owns the rolling state for the safeguard policy.
type Guard struct {
	cfg Config

	// Most recent sample that was Accepted; nil until first acceptance.
	lastAccepted *fritz.Measurement
	// State machine progression.
	health Health
	// Run-length of byte-equal samples by RawPower (and RawVoltage if
	// StuckMatchVoltage is on). Counts the *latest* run, including the
	// current sample.
	stuckRun int
	// Last seen raw values, regardless of verdict (used to detect stuck
	// data even on Reject samples).
	lastRawPower   int64
	lastRawVoltage int64
	haveLastRaw    bool
	// Consecutive Rejects since the last Accept.
	rejectRun int
	// Consecutive Accepts since the last Stale. Counter towards
	// RecoverySamples before HealthDegraded/HealthStale clears.
	recoveryRun int
	// Last reported poll-failure or evaluate timestamp; used to backfill
	// freshness even when Evaluate is never called.
	lastEvaluatedAt time.Time
	// First time Evaluate or NotePollFailure was called - used to drive
	// the Init->Stale transition when we have never had a successful
	// poll but MaxAge has elapsed since the very first attempt.
	firstEvaluatedAt time.Time
}

// NewGuard returns a Guard configured with cfg. Numeric zero-valued
// fields fall back to DefaultConfig values; bool fields default to
// false unless explicitly set (so callers always know exactly what
// they are getting). Use DefaultConfig() as a starting point if you
// want the conservative defaults for bools too:
//
//	cfg := safeguard.DefaultConfig()
//	cfg.StuckCycles = 10
//	g := safeguard.NewGuard(cfg)
func NewGuard(cfg Config) *Guard {
	d := DefaultConfig()
	if cfg.MaxAge == 0 {
		cfg.MaxAge = d.MaxAge
	}
	if cfg.StuckCycles == 0 {
		cfg.StuckCycles = d.StuckCycles
	}
	if cfg.MinVoltage == 0 {
		cfg.MinVoltage = d.MinVoltage
	}
	if cfg.MaxVoltage == 0 {
		cfg.MaxVoltage = d.MaxVoltage
	}
	if cfg.MaxAbsPower == 0 {
		cfg.MaxAbsPower = d.MaxAbsPower
	}
	if cfg.MaxStep == 0 {
		cfg.MaxStep = d.MaxStep
	}
	if cfg.RecoverySamples == 0 {
		cfg.RecoverySamples = d.RecoverySamples
	}
	if len(cfg.AcceptedStates) == 0 {
		cfg.AcceptedStates = d.AcceptedStates
	}
	return &Guard{cfg: cfg, health: HealthInit}
}

// Config returns the effective configuration for inspection.
func (g *Guard) Config() Config { return g.cfg }

// LastAccepted returns the most recent Measurement that was Accepted,
// or nil if no Accept has occurred yet.
func (g *Guard) LastAccepted() *fritz.Measurement { return g.lastAccepted }

// Health returns the current state-machine health.
func (g *Guard) Health() Health { return g.health }

// NotePollFailure tells the guard that the underlying poll attempt
// failed entirely (no Measurement was produced). The state machine is
// not advanced past a transient failure but the freshness clock keeps
// ticking, so enough back-to-back failures will eventually transition
// to HealthStale.
func (g *Guard) NotePollFailure(now time.Time) EvaluateResult {
	g.markEvaluated(now)
	g.advanceHealthByAge(now)
	return EvaluateResult{
		Verdict:         Reject,
		Reason:          ReasonPollFailure,
		Health:          g.health,
		LastAcceptedAge: g.ageOfLastAccepted(now),
	}
}

// Evaluate classifies a single sample. Caller must pass the
// fritz.Measurement and (optionally) the most recent DeviceInfo for the
// configured unit. devInfo may be nil if RequireConnected is false or
// the caller does not have one.
func (g *Guard) Evaluate(now time.Time, m *fritz.Measurement, devInfo *fritz.DeviceInfo) EvaluateResult {
	g.markEvaluated(now)

	// Connection check (cheap and binary - do it first).
	if g.cfg.RequireConnected && devInfo != nil && !devInfo.IsConnected {
		g.rejectRun++
		g.advanceHealthByAge(now)
		return g.result(Reject, ReasonDeviceOffline, now)
	}

	// multimeterInterface.state must be one of the accepted values.
	if !g.stateAccepted(m.State) {
		g.rejectRun++
		g.advanceHealthByAge(now)
		return g.result(Reject, ReasonStateNotValid, now)
	}

	// Plausibility bounds (voltage / power).
	if m.Voltage != 0 && (m.Voltage < g.cfg.MinVoltage || m.Voltage > g.cfg.MaxVoltage) {
		g.rejectRun++
		g.advanceHealthByAge(now)
		return g.result(Reject, ReasonOutOfRangeVoltage, now)
	}
	if g.cfg.MaxAbsPower > 0 && absf(m.ActivePower) > g.cfg.MaxAbsPower {
		g.rejectRun++
		g.advanceHealthByAge(now)
		return g.result(Reject, ReasonOutOfRangePower, now)
	}

	// Step-change guard relative to last accepted sample.
	if g.cfg.MaxStep > 0 && g.lastAccepted != nil {
		if absf(m.ActivePower-g.lastAccepted.ActivePower) > g.cfg.MaxStep {
			g.rejectRun++
			g.advanceHealthByAge(now)
			return g.result(Reject, ReasonStepTooLarge, now)
		}
	}

	// Stuck-data detection. We update the run length before checking
	// so that a long stall is observable in StuckRunLength.
	g.updateStuckRun(m)
	if g.cfg.StuckCycles > 0 && g.stuckRun >= g.cfg.StuckCycles {
		// Stuck data is severe enough to count as full Stale, not just
		// Reject: a frozen meter is not going to magically resume on
		// its own without a real change.
		g.rejectRun++
		g.health = HealthStale
		return g.result(Stale, ReasonStuckData, now)
	}

	// Sample passed every check.
	g.lastAccepted = cloneMeasurement(m)
	g.rejectRun = 0
	g.recoveryRun++
	g.advanceHealthOnAccept()
	return g.result(Accept, ReasonNone, now)
}

func (g *Guard) result(v Verdict, r Reason, now time.Time) EvaluateResult {
	return EvaluateResult{
		Verdict:            v,
		Reason:             r,
		Health:             g.health,
		LastAcceptedAge:    g.ageOfLastAccepted(now),
		StuckRunLength:     g.stuckRun,
		ConsecutiveRejects: g.rejectRun,
	}
}

func (g *Guard) ageOfLastAccepted(now time.Time) time.Duration {
	if g.lastAccepted == nil {
		return 0
	}
	d := now.Sub(g.lastAccepted.Updated)
	if d < 0 {
		return 0
	}
	return d
}

func (g *Guard) stateAccepted(state string) bool {
	if state == "" {
		// Empty state means the field was not present in the JSON.
		// We treat that as accepted because some firmware revisions
		// omit it for healthy readings.
		return true
	}
	s := strings.ToLower(state)
	for _, allowed := range g.cfg.AcceptedStates {
		if s == strings.ToLower(allowed) {
			return true
		}
	}
	return false
}

func (g *Guard) updateStuckRun(m *fritz.Measurement) {
	if !g.haveLastRaw {
		g.haveLastRaw = true
		g.lastRawPower = m.RawPower
		g.lastRawVoltage = m.RawVoltage
		g.stuckRun = 1
		return
	}
	same := g.lastRawPower == m.RawPower
	if g.cfg.StuckMatchVoltage {
		same = same && g.lastRawVoltage == m.RawVoltage
	}
	if same {
		g.stuckRun++
	} else {
		g.stuckRun = 1
		g.lastRawPower = m.RawPower
		g.lastRawVoltage = m.RawVoltage
	}
}

// advanceHealthOnAccept transitions the health state machine when an
// Accept happens. Recovery from HealthStale / HealthDegraded requires
// `cfg.RecoverySamples` consecutive Accepts.
func (g *Guard) advanceHealthOnAccept() {
	switch g.health {
	case HealthInit, HealthOk:
		g.health = HealthOk
		g.recoveryRun = 0
	case HealthDegraded, HealthStale:
		if g.recoveryRun >= g.cfg.RecoverySamples {
			g.health = HealthOk
			g.recoveryRun = 0
		}
	}
}

// advanceHealthByAge applies the freshness budget without touching the
// state machine on its own. Called from both Evaluate (on Reject) and
// NotePollFailure to catch the "no samples for a long time" case.
func (g *Guard) advanceHealthByAge(now time.Time) {
	if g.lastAccepted == nil {
		// Init -> stays Init until we have at least one accepted sample.
		// But after one full MaxAge from the very first evaluation
		// attempt, escalate to Stale so consumers see something other
		// than HealthInit forever.
		if !g.firstEvaluatedAt.IsZero() &&
			now.Sub(g.firstEvaluatedAt) > g.cfg.MaxAge &&
			g.health == HealthInit {
			g.health = HealthStale
		}
		return
	}
	age := now.Sub(g.lastAccepted.Updated)
	switch {
	case age >= g.cfg.MaxAge:
		g.health = HealthStale
		g.recoveryRun = 0
	case g.rejectRun > 0 && g.health == HealthOk:
		g.health = HealthDegraded
	}
}

// markEvaluated records the wall-clock time of the current evaluation.
// The first evaluation also sets firstEvaluatedAt, which is what drives
// the Init->Stale transition before we ever see a healthy sample.
func (g *Guard) markEvaluated(now time.Time) {
	if g.firstEvaluatedAt.IsZero() {
		g.firstEvaluatedAt = now
	}
	g.lastEvaluatedAt = now
}

func absf(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func cloneMeasurement(m *fritz.Measurement) *fritz.Measurement {
	c := *m
	return &c
}

// Describe returns a one-line human-readable summary of the guard's
// state. Useful for logs and the /healthz endpoint.
func (g *Guard) Describe(now time.Time) string {
	if g.lastAccepted == nil {
		return fmt.Sprintf("health=%s no_samples_yet", g.health)
	}
	return fmt.Sprintf(
		"health=%s last_accepted=%s stuck_run=%d reject_run=%d",
		g.health,
		now.Sub(g.lastAccepted.Updated).Truncate(time.Second),
		g.stuckRun,
		g.rejectRun,
	)
}
