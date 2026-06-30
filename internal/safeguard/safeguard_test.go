package safeguard

import (
	"testing"
	"time"

	"github.com/flunaras/shelly-fritz-proxy/internal/fritz"
)

// sampleAt returns a baseline FSE 250 measurement we can mutate per test.
func sampleAt(t time.Time, pW int64, vmV int64) *fritz.Measurement {
	return &fritz.Measurement{
		ActivePower: float64(pW) / 1000,
		Voltage:     float64(vmV) / 1000,
		Current:     1.0,
		Energy:      12345,
		Updated:     t,
		State:       "valid",
		RawPower:    pW,
		RawVoltage:  vmV,
	}
}

func TestAcceptsHealthyFirstSample(t *testing.T) {
	now := time.Now()
	g := NewGuard(Config{})
	r := g.Evaluate(now, sampleAt(now, 1234567, 230500), nil)
	if r.Verdict != Accept {
		t.Fatalf("verdict: got %v, want Accept", r.Verdict)
	}
	if g.Health() != HealthOk {
		t.Fatalf("health: got %v, want HealthOk", g.Health())
	}
}

func TestRejectsOutOfRangeVoltage(t *testing.T) {
	now := time.Now()
	g := NewGuard(Config{})
	r := g.Evaluate(now, sampleAt(now, 500, 320000 /* 320 V */), nil)
	if r.Verdict != Reject {
		t.Fatalf("verdict: got %v, want Reject", r.Verdict)
	}
	if r.Reason != ReasonOutOfRangeVoltage {
		t.Fatalf("reason: got %q, want %q", r.Reason, ReasonOutOfRangeVoltage)
	}
}

func TestRejectsOutOfRangePower(t *testing.T) {
	now := time.Now()
	g := NewGuard(Config{MaxAbsPower: 5000})
	r := g.Evaluate(now, sampleAt(now, 12000000 /* 12 kW */, 230000), nil)
	if r.Verdict != Reject || r.Reason != ReasonOutOfRangePower {
		t.Fatalf("got verdict=%v reason=%q", r.Verdict, r.Reason)
	}
}

func TestRejectsStepTooLarge(t *testing.T) {
	now := time.Now()
	g := NewGuard(Config{MaxStep: 1000})
	// Establish a healthy 500 W baseline.
	g.Evaluate(now, sampleAt(now, 500000, 230000), nil)
	// Spike to 6 kW: step of 5500 W > 1000 W threshold.
	r := g.Evaluate(now.Add(2*time.Second), sampleAt(now.Add(2*time.Second), 6000000, 230000), nil)
	if r.Verdict != Reject || r.Reason != ReasonStepTooLarge {
		t.Fatalf("got verdict=%v reason=%q", r.Verdict, r.Reason)
	}
}

func TestRejectsInvalidState(t *testing.T) {
	now := time.Now()
	g := NewGuard(Config{})
	s := sampleAt(now, 500, 230000)
	s.State = "invalid"
	r := g.Evaluate(now, s, nil)
	if r.Verdict != Reject || r.Reason != ReasonStateNotValid {
		t.Fatalf("got verdict=%v reason=%q", r.Verdict, r.Reason)
	}
}

func TestRejectsDeviceOffline(t *testing.T) {
	now := time.Now()
	g := NewGuard(DefaultConfig())
	r := g.Evaluate(now, sampleAt(now, 500, 230000),
		&fritz.DeviceInfo{ProductName: "FRITZ!Smart Energy 250", IsConnected: false})
	if r.Verdict != Reject || r.Reason != ReasonDeviceOffline {
		t.Fatalf("got verdict=%v reason=%q", r.Verdict, r.Reason)
	}
}

func TestStuckDataDeclaresStale(t *testing.T) {
	now := time.Now()
	g := NewGuard(Config{StuckCycles: 3})
	// Three identical RawPower / RawVoltage samples -> Stale on the third.
	for i := 0; i < 2; i++ {
		r := g.Evaluate(now.Add(time.Duration(i)*time.Second),
			sampleAt(now.Add(time.Duration(i)*time.Second), 5000000, 230000), nil)
		if r.Verdict != Accept {
			t.Fatalf("sample %d: got %v, want Accept", i, r.Verdict)
		}
	}
	r := g.Evaluate(now.Add(3*time.Second), sampleAt(now.Add(3*time.Second), 5000000, 230000), nil)
	if r.Verdict != Stale || r.Reason != ReasonStuckData {
		t.Fatalf("third sample: got verdict=%v reason=%q", r.Verdict, r.Reason)
	}
	if g.Health() != HealthStale {
		t.Fatalf("health: got %v, want HealthStale", g.Health())
	}
}

func TestStuckRunResetsOnChange(t *testing.T) {
	now := time.Now()
	g := NewGuard(Config{StuckCycles: 5})
	for i := 0; i < 3; i++ {
		g.Evaluate(now.Add(time.Duration(i)*time.Second),
			sampleAt(now.Add(time.Duration(i)*time.Second), 5000000, 230000), nil)
	}
	// Changed power -> stuck run should reset to 1.
	r := g.Evaluate(now.Add(4*time.Second),
		sampleAt(now.Add(4*time.Second), 6000000, 230000), nil)
	if r.Verdict != Accept {
		t.Fatalf("change should accept: got %v", r.Verdict)
	}
	if r.StuckRunLength != 1 {
		t.Fatalf("stuck run: got %d, want 1", r.StuckRunLength)
	}
}

func TestFreshnessTimeoutEscalatesToStale(t *testing.T) {
	now := time.Now()
	g := NewGuard(Config{MaxAge: 5 * time.Second})
	g.Evaluate(now, sampleAt(now, 1000000, 230000), nil)
	if g.Health() != HealthOk {
		t.Fatal("baseline should be HealthOk")
	}
	// 10s later, no successful poll: NotePollFailure should escalate.
	r := g.NotePollFailure(now.Add(10 * time.Second))
	if r.Health != HealthStale {
		t.Fatalf("health: got %v, want HealthStale", r.Health)
	}
}

func TestRecoveryRequiresMultipleAccepts(t *testing.T) {
	now := time.Now()
	g := NewGuard(Config{StuckCycles: 2, RecoverySamples: 3})
	// Force stuck -> stale.
	g.Evaluate(now, sampleAt(now, 100000, 230000), nil)
	g.Evaluate(now.Add(time.Second), sampleAt(now.Add(time.Second), 100000, 230000), nil)
	if g.Health() != HealthStale {
		t.Fatalf("setup: not stale yet (got %v)", g.Health())
	}
	// One good sample -> still HealthStale (need 3).
	g.Evaluate(now.Add(2*time.Second), sampleAt(now.Add(2*time.Second), 200000, 230000), nil)
	if g.Health() != HealthStale {
		t.Fatalf("1 accept should not recover: got %v", g.Health())
	}
	g.Evaluate(now.Add(3*time.Second), sampleAt(now.Add(3*time.Second), 300000, 230000), nil)
	if g.Health() != HealthStale {
		t.Fatalf("2 accepts should not recover: got %v", g.Health())
	}
	// 3rd good sample -> recovered.
	g.Evaluate(now.Add(4*time.Second), sampleAt(now.Add(4*time.Second), 400000, 230000), nil)
	if g.Health() != HealthOk {
		t.Fatalf("3 accepts should recover: got %v", g.Health())
	}
}

func TestStuckMatchVoltageOffAllowsConstantPower(t *testing.T) {
	now := time.Now()
	g := NewGuard(Config{StuckCycles: 3, StuckMatchVoltage: false})
	// Same power but jiggling voltage -> stuck because we don't gate on voltage match.
	for i := 0; i < 5; i++ {
		r := g.Evaluate(now.Add(time.Duration(i)*time.Second),
			sampleAt(now.Add(time.Duration(i)*time.Second), 5000000, 230000+int64(i)), nil)
		if i >= 2 && r.Verdict != Stale {
			t.Fatalf("expected stale by i=%d, got %v", i, r.Verdict)
		}
	}
}

func TestNoSamplesAfterMaxAgeFlipsToStale(t *testing.T) {
	now := time.Now()
	g := NewGuard(Config{MaxAge: time.Second})
	// First poll failure - should still be Init.
	r := g.NotePollFailure(now)
	if r.Health != HealthInit {
		t.Fatalf("first failure: got %v, want HealthInit", r.Health)
	}
	// More than MaxAge later with continuing failures.
	r = g.NotePollFailure(now.Add(2 * time.Second))
	if r.Health != HealthStale {
		t.Fatalf("after MaxAge: got %v, want HealthStale", r.Health)
	}
}

func TestLastAcceptedPreservedOnReject(t *testing.T) {
	now := time.Now()
	g := NewGuard(Config{})
	g.Evaluate(now, sampleAt(now, 1000000, 230000), nil)
	la := g.LastAccepted()
	if la == nil || la.RawPower != 1000000 {
		t.Fatal("baseline missing")
	}
	// Glitch sample - voltage way too high.
	g.Evaluate(now.Add(time.Second), sampleAt(now.Add(time.Second), 1000000, 999999), nil)
	la2 := g.LastAccepted()
	if la2 == nil || la2.RawPower != 1000000 || la2.RawVoltage != 230000 {
		t.Fatalf("last accepted should be unchanged: got %+v", la2)
	}
}

func TestDescribe(t *testing.T) {
	now := time.Now()
	g := NewGuard(Config{})
	if got := g.Describe(now); got == "" {
		t.Fatal("Describe should never be empty")
	}
	g.Evaluate(now, sampleAt(now, 1000000, 230000), nil)
	if got := g.Describe(now); got == "" {
		t.Fatal("Describe should never be empty after a sample")
	}
}
