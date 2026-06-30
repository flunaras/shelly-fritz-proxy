package forecast

import "testing"

func TestAdjustFirstCallTrustsInput(t *testing.T) {
	e := New()
	got := e.Adjust(50, 50, 20)
	if got != 50 {
		t.Fatalf("first call: got %v, want 50", got)
	}
}

func TestAdjustWorkedExampleFromDesignNotes(t *testing.T) {
	// Mirrors the exact worked example from forecast-mode.md:
	// 1. Solakon currently outputs 20W.
	// 2. FRITZ reads 50W (new sample) -> reported 50W.
	// 3. Solakon increases its output by 50W to 70W.
	// 4. FRITZ still reads 50W (stale, same raw value), Solakon now at 70W.
	//    Expected: 50 - 70 + 20 = 0.
	e := New()

	got := e.Adjust(50, 50, 20)
	if got != 50 {
		t.Fatalf("step 2: got %v, want 50", got)
	}

	got = e.Adjust(50, 50, 70)
	if got != 0 {
		t.Fatalf("step 4: got %v, want 0", got)
	}
}

func TestAdjustGenuineChangeResetsBaseline(t *testing.T) {
	e := New()
	e.Adjust(50, 50, 20)
	e.Adjust(50, 50, 70) // stale cycle, adjusted to 0

	// FRITZ genuinely refreshes to a new raw value: report it as-is
	// and re-baseline, regardless of what the stale adjustment had
	// been computing.
	got := e.Adjust(10, 10, 90)
	if got != 10 {
		t.Fatalf("genuine refresh: got %v, want 10", got)
	}

	// Next stale cycle now baselines against the new reading.
	got = e.Adjust(10, 10, 100)
	if got != 0 { // 10 - 100 + 90 = 0
		t.Fatalf("post-refresh stale cycle: got %v, want 0", got)
	}
}

func TestReset(t *testing.T) {
	e := New()
	e.Adjust(50, 50, 20)
	e.Reset()
	// After Reset, the next call must be trusted outright again, even
	// with a byte-identical raw value to before.
	got := e.Adjust(50, 50, 999)
	if got != 50 {
		t.Fatalf("after reset: got %v, want 50", got)
	}
}
