package shelly

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/flunaras/shelly-fritz-proxy/internal/fritz"
	"github.com/flunaras/shelly-fritz-proxy/internal/meter"
	"github.com/flunaras/shelly-fritz-proxy/internal/safeguard"
)

// newTestServer spins up an in-memory Shelly server with a freshly
// constructed Store and (optionally) a single pre-loaded measurement.
// The default StaleConfig is StalePolicyError (the recommended fail-safe).
func newTestServer(t *testing.T, m *fritz.Measurement, mode meter.PhaseMode) (*httptest.Server, *meter.Store) {
	return newTestServerCfg(t, m, mode, StaleConfig{Policy: StalePolicyError})
}

func newTestServerCfg(t *testing.T, m *fritz.Measurement, mode meter.PhaseMode, sc StaleConfig) (*httptest.Server, *meter.Store) {
	t.Helper()
	guard := safeguard.NewGuard(safeguard.DefaultConfig())
	// nil fritz.Client is fine because we never call Run.
	store := meter.NewStore(nil, "AIN", mode, time.Hour, guard, slog.Default())
	if m != nil {
		injectMeasurement(t, store, m)
	}
	srv := New(store, "AABBCC112233", sc, slog.Default())
	mux := http.NewServeMux()
	srv.Register(mux)
	return httptest.NewServer(mux), store
}

func TestShellyDiscovery(t *testing.T) {
	ts, _ := newTestServer(t, nil, meter.PhaseSplit)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/shelly")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var info map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info["gen"].(float64) != 2 {
		t.Fatalf("gen=%v", info["gen"])
	}
	if info["model"] != "SPEM-003CEBEU" {
		t.Fatalf("model=%v", info["model"])
	}
	if info["profile"] != "triphase" {
		t.Fatalf("profile=%v", info["profile"])
	}
	if mac, _ := info["mac"].(string); mac != "AABBCC112233" {
		t.Fatalf("mac=%v", info["mac"])
	}
}

func TestEMGetStatusNoData(t *testing.T) {
	ts, _ := newTestServer(t, nil, meter.PhaseSplit)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/rpc/EM.GetStatus?id=0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	errs, _ := got["errors"].([]any)
	if len(errs) == 0 || errs[0] != "no_data" {
		t.Fatalf("expected no_data error, got %v", got["errors"])
	}
}

func TestEMGetStatusSplit(t *testing.T) {
	m := &fritz.Measurement{
		Voltage:     230,
		ActivePower: 900, // 300 W per phase after split
		Energy:      12345,
		Updated:     time.Now(),
		State:       "valid",
		RawPower:    900000,
		RawVoltage:  230000,
	}
	ts, _ := newTestServer(t, m, meter.PhaseSplit)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/rpc/EM.GetStatus?id=0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"a_act_power", "b_act_power", "c_act_power"} {
		v, ok := got[key].(float64)
		if !ok {
			t.Fatalf("missing %s: %v", key, got[key])
		}
		if v < 299 || v > 301 {
			t.Fatalf("%s: got %v, want ~300", key, v)
		}
	}
	tot, _ := got["total_act_power"].(float64)
	if tot < 899 || tot > 901 {
		t.Fatalf("total: got %v, want ~900", tot)
	}
	for _, key := range []string{"a_voltage", "b_voltage", "c_voltage"} {
		v, _ := got[key].(float64)
		if v != 230 {
			t.Fatalf("%s: got %v, want 230", key, v)
		}
	}
}

func TestRPCJSONEnvelope(t *testing.T) {
	ts, _ := newTestServer(t, nil, meter.PhaseSplit)
	defer ts.Close()
	body := strings.NewReader(`{"id":42,"method":"Shelly.GetDeviceInfo"}`)
	resp, err := http.Post(ts.URL+"/rpc", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if int(got["id"].(float64)) != 42 {
		t.Fatalf("id: got %v", got["id"])
	}
	result, ok := got["result"].(map[string]any)
	if !ok {
		t.Fatalf("missing result: %v", got)
	}
	if result["model"] != "SPEM-003CEBEU" {
		t.Fatalf("result.model: %v", result["model"])
	}
}

func TestRPCUnknownMethod(t *testing.T) {
	ts, _ := newTestServer(t, nil, meter.PhaseSplit)
	defer ts.Close()
	body := strings.NewReader(`{"id":1,"method":"Light.Toggle"}`)
	resp, err := http.Post(ts.URL+"/rpc", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	e, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error envelope: %v", got)
	}
	if int(e["code"].(float64)) != -32601 {
		t.Fatalf("error.code: %v", e["code"])
	}
}

// Sanity for the test harness itself.
func TestServerHandlesContextlessRequest(t *testing.T) {
	// We only check that none of these endpoints panic under load.
	ts, _ := newTestServer(t, nil, meter.PhaseSplit)
	defer ts.Close()
	for _, p := range []string{"/shelly", "/rpc/Sys.GetStatus", "/rpc/EMData.GetStatus?id=0", "/rpc/Shelly.GetStatus"} {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status %d", p, resp.StatusCode)
		}
	}
	_ = context.Background()
}

// fetchEM is a tiny convenience for the stale-policy tests below.
func fetchEM(t *testing.T, ts *httptest.Server) map[string]any {
	t.Helper()
	resp, err := http.Get(ts.URL + "/rpc/EM.GetStatus?id=0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

// freshSample feeds N byte-equal samples into the store to force the
// safeguard into Stale via stuck-data detection.
func forceStuckStale(t *testing.T, store *meter.Store) {
	t.Helper()
	now := time.Now()
	for i := 0; i < 10; i++ {
		store.Apply(&fritz.Measurement{
			Voltage:     230,
			ActivePower: 900,
			Energy:      12345,
			Updated:     now.Add(time.Duration(i) * time.Second),
			State:       "valid",
			RawPower:    900000,
			RawVoltage:  230000,
		})
	}
}

func TestStalePolicyErrorNullsMeasurements(t *testing.T) {
	ts, store := newTestServerCfg(t, nil, meter.PhaseSplit, StaleConfig{Policy: StalePolicyError})
	defer ts.Close()
	forceStuckStale(t, store)
	got := fetchEM(t, ts)

	// Measurements must be null.
	for _, k := range []string{"a_act_power", "b_act_power", "c_act_power", "total_act_power"} {
		if got[k] != nil {
			t.Fatalf("%s: got %v, want nil", k, got[k])
		}
	}
	// errors[] should contain stale_data and stuck_data tags.
	errs, _ := got["errors"].([]any)
	if len(errs) < 2 {
		t.Fatalf("expected at least 2 error tags, got %v", errs)
	}
	if errs[0] != "stale_data" {
		t.Fatalf("first error: got %v, want stale_data", errs[0])
	}
}

func TestStalePolicyFreezeKeepsLastGood(t *testing.T) {
	ts, store := newTestServerCfg(t, nil, meter.PhaseSplit, StaleConfig{Policy: StalePolicyFreeze})
	defer ts.Close()
	forceStuckStale(t, store)
	got := fetchEM(t, ts)

	// Freeze should still report the last good measurements.
	if p, _ := got["total_act_power"].(float64); p < 899 || p > 901 {
		t.Fatalf("total_act_power: got %v, want ~900", p)
	}
	// But errors[] must still flag the stale state.
	errs, _ := got["errors"].([]any)
	if len(errs) == 0 {
		t.Fatal("freeze policy must still set errors[]")
	}
}

func TestStalePolicySafeImportThrottlesInverter(t *testing.T) {
	ts, store := newTestServerCfg(t, nil, meter.PhaseSplit,
		StaleConfig{Policy: StalePolicySafeImport, SafeImportWatts: 8000})
	defer ts.Close()
	forceStuckStale(t, store)
	got := fetchEM(t, ts)

	// Total must be exactly the safe-import value.
	p, _ := got["total_act_power"].(float64)
	if p < 7999 || p > 8001 {
		t.Fatalf("total_act_power: got %v, want ~8000 (safe-import)", p)
	}
	// errors[] is still set so dashboards can tell what's happening.
	errs, _ := got["errors"].([]any)
	if len(errs) == 0 {
		t.Fatal("safe_import policy must still set errors[] for observability")
	}
}

func TestStalePolicyZeroIsDangerouslySilent(t *testing.T) {
	ts, store := newTestServerCfg(t, nil, meter.PhaseSplit, StaleConfig{Policy: StalePolicyZero})
	defer ts.Close()
	forceStuckStale(t, store)
	got := fetchEM(t, ts)

	// Zero policy intentionally reports zero power and the
	// errors[] is also empty because the whole point is to look
	// "healthy". This test exists mostly to document the danger.
	p, _ := got["total_act_power"].(float64)
	if p != 0 {
		t.Fatalf("total_act_power: got %v, want 0", p)
	}
}

func TestEMDataReportsErrorsOnStale(t *testing.T) {
	ts, store := newTestServerCfg(t, nil, meter.PhaseSplit, StaleConfig{Policy: StalePolicyError})
	defer ts.Close()
	forceStuckStale(t, store)

	resp, err := http.Get(ts.URL + "/rpc/EMData.GetStatus?id=0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["errors"]; !ok {
		t.Fatal("EMData.GetStatus should set errors[] when stale")
	}
}

func TestParseStalePolicy(t *testing.T) {
	for in, want := range map[string]StalePolicy{
		"":             StalePolicyError,
		"error":        StalePolicyError,
		"freeze":       StalePolicyFreeze,
		"safe_import":  StalePolicySafeImport,
		"safe-import":  StalePolicySafeImport,
		"zero":         StalePolicyZero,
	} {
		got, err := ParseStalePolicy(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != want {
			t.Fatalf("%q: got %v, want %v", in, got, want)
		}
	}
	if _, err := ParseStalePolicy("nope"); err == nil {
		t.Fatal("expected error for invalid value")
	}
}
