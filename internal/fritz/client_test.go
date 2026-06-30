package fritz

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Test vectors for the legacy MD5 challenge: AVM published an example in
// older revisions of the SessionID documentation that uses challenge
// "1234567z" and password "äbc" and expects MD5 of the UTF-16-LE encoded
// string "1234567z-äbc" => "9e224a41eeefa284df7bb0f26c2913e2".
func TestLegacyChallengeResponseAVMExample(t *testing.T) {
	got, err := computeChallengeResponse("1234567z", "äbc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "1234567z-9e224a41eeefa284df7bb0f26c2913e2"
	if got != want {
		t.Fatalf("legacy response mismatch:\n got=%s\nwant=%s", got, want)
	}
}

// TestPBKDF2ChallengeShape only checks the response shape; we cannot pin a
// specific test vector without disclosing real device data, but we can
// verify the response is "<salt2>$<hex32bytes>" for a valid v2 challenge.
func TestPBKDF2ChallengeShape(t *testing.T) {
	salt1 := make([]byte, 16)
	salt2 := make([]byte, 16)
	for i := range salt1 {
		salt1[i] = byte(i)
		salt2[i] = byte(i + 16)
	}
	challenge := "2$1000$" + hex.EncodeToString(salt1) + "$1000$" + hex.EncodeToString(salt2)
	resp, err := computeChallengeResponse(challenge, "secret")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	prefix := hex.EncodeToString(salt2) + "$"
	if len(resp) != len(prefix)+64 {
		t.Fatalf("response length wrong: got=%d want=%d", len(resp), len(prefix)+64)
	}
	if resp[:len(prefix)] != prefix {
		t.Fatalf("response prefix wrong: got=%q want=%q", resp[:len(prefix)], prefix)
	}
}

// TestGetMeasurementREST verifies that the REST client builds the right URL,
// passes the SID via the "Authorization: AVM-SID <sid>" header (per the
// Smart Home REST API's AVM-SID apiKey security scheme, not a query
// parameter), asks for JSON, and decodes the FSE 250 fields correctly
// (mW -> W, mV -> V, signed power). It also confirms the UID's internal
// space (a normal part of AVM's UID/AIN format, e.g. "11657 0123456") is
// preserved and percent-encoded as %20 on the wire rather than stripped;
// stripping it produces a UID the FRITZ!Box does not recognise and every
// REST call fails with "UID_NOT_FOUND" even though login succeeded.
func TestGetMeasurementREST(t *testing.T) {
	var seenPath, seenRawPath, seenAuth, seenAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenRawPath = r.URL.EscapedPath()
		seenAuth = r.Header.Get("Authorization")
		seenAccept = r.Header.Get("Accept")
		// FSE 250 reporting -1234 W (export) at 230.500 V; energy 12345 Wh.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"UID": "11657 0123456",
			"ain": "11657 0123456",
			"name": "Hausanschluss",
			"unitType": "avmPlugSocket",
			"interfaces": {
				"multimeterInterface": {
					"state":   "valid",
					"voltage": 230500,
					"current": 5366,
					"power":   -1234000,
					"energy":  12345
				}
			}
		}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "user", "pass")
	// Bypass login by preseeding the SID.
	c.sid = "1234567890abcdef"

	m, err := c.GetMeasurement(context.Background(), "11657 0123456")
	if err != nil {
		t.Fatalf("GetMeasurement: %v", err)
	}
	if seenPath != "/api/v0/smarthome/overview/units/11657 0123456" {
		t.Fatalf("path: got %s", seenPath)
	}
	if seenRawPath != "/api/v0/smarthome/overview/units/11657%200123456" {
		t.Fatalf("escaped path: got %s (UID space must be percent-encoded, not stripped)", seenRawPath)
	}
	if seenAuth != "AVM-SID 1234567890abcdef" {
		t.Fatalf("authorization header: got %q", seenAuth)
	}
	if seenAccept != "application/json" {
		t.Fatalf("accept: got %s", seenAccept)
	}
	if m.ActivePower != -1234 {
		t.Fatalf("power: got %v, want -1234", m.ActivePower)
	}
	if m.Voltage != 230.5 {
		t.Fatalf("voltage: got %v, want 230.5", m.Voltage)
	}
	if m.Current != 5.366 {
		t.Fatalf("current: got %v, want 5.366", m.Current)
	}
	if m.Energy != 12345 {
		t.Fatalf("energy: got %v, want 12345", m.Energy)
	}
}

// TestGetMeasurementAcceptsUIDWithoutSpace mirrors TestGetMeasurementREST
// but configures the unit UID as a bare run of 12 digits, the way an
// operator might type it into an environment variable or a shell
// argument without noticing (or being able to easily reproduce) the
// internal space AVM prints on the device sticker. GetMeasurement must
// reinsert the space so the request on the wire is identical to the
// space-containing form; without NormalizeAIN this would instead reach
// the FRITZ!Box as "116570123456" and fail with "UID_NOT_FOUND".
func TestGetMeasurementAcceptsUIDWithoutSpace(t *testing.T) {
	var seenRawPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenRawPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"interfaces":{"multimeterInterface":{"state":"valid","power":-1234000,"voltage":230500,"current":5366,"energy":12345}}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "user", "pass")
	c.sid = "1234567890abcdef"

	if _, err := c.GetMeasurement(context.Background(), "116570123456"); err != nil {
		t.Fatalf("GetMeasurement: %v", err)
	}
	if want := "/api/v0/smarthome/overview/units/11657%200123456"; seenRawPath != want {
		t.Fatalf("escaped path: got %s, want %s (the missing space must be reinserted, not left out)", seenRawPath, want)
	}
}

// TestGetMeasurementCustomBasePath confirms SetBasePath plumbs through and
// is what lets you point this client at fritzhome-cache (default /api/v0).
func TestGetMeasurementCustomBasePath(t *testing.T) {
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		_, _ = w.Write([]byte(`{"interfaces":{"multimeterInterface":{"power":0,"voltage":230000,"current":0,"energy":0}}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "u", "p")
	c.SetBasePath("/custom")
	c.sid = "deadbeef"

	if _, err := c.GetMeasurement(context.Background(), "1234"); err != nil {
		t.Fatal(err)
	}
	if seenPath != "/custom/smarthome/overview/units/1234" {
		t.Fatalf("path: got %s", seenPath)
	}
}

// TestGetMeasurementWithSmartmeterInterface checks that import/export
// counters are picked up from the optional smartmeterInterface fields
// when the firmware exposes them.
func TestGetMeasurementWithSmartmeterInterface(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"interfaces": {
				"multimeterInterface": {"power": 500, "voltage": 230000, "current": 2174, "energy": 9999},
				"smartmeterInterface": {"state": "valid", "smartmeterState": [], "energyImported": 7777, "energyExported": 2222}
			}
		}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "u", "p")
	c.sid = "x"
	m, err := c.GetMeasurement(context.Background(), "1")
	if err != nil {
		t.Fatal(err)
	}
	if m.EnergyIn != 7777 || m.EnergyOut != 2222 {
		t.Fatalf("in/out: %v/%v", m.EnergyIn, m.EnergyOut)
	}
}

// TestSessionReloginOn401 makes sure the client transparently re-logs in
// when the FRITZ!Box (or fritzhome-cache) returns 401. We assert that the
// second call goes through.
func TestSessionReloginOn401(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login_sid.lua":
			// Two challenges (one for first login, one after the 401).
			w.Header().Set("Content-Type", "text/xml")
			_, _ = w.Write([]byte(`<?xml version="1.0"?><SessionInfo><SID>deadbeefcafebabe</SID><Challenge>x</Challenge><BlockTime>0</BlockTime></SessionInfo>`))
		case "/api/v0/smarthome/overview/units/abc":
			calls++
			if calls == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"interfaces":{"multimeterInterface":{"power":1,"voltage":230000,"current":4,"energy":2}}}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "u", "p")
	// Force an immediate login attempt (no preseed).
	c.sid = "stale-sid-pretend-its-valid"

	if _, err := c.GetMeasurement(context.Background(), "abc"); err != nil {
		t.Fatalf("GetMeasurement: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls to unit endpoint, got %d", calls)
	}
}

// TestEmptyUID surfaces a clear error rather than a confusing 404.
func TestEmptyUID(t *testing.T) {
	c := NewClient("http://example.invalid", "u", "p")
	_, err := c.GetMeasurement(context.Background(), "  ")
	if err == nil || !strings.Contains(err.Error(), "empty unit UID") {
		t.Fatalf("expected empty UID error, got %v", err)
	}
}

// TestGetMeasurementTrimsOnlySurroundingWhitespace confirms incidental
// leading/trailing whitespace (e.g. from copy-pasting a config value) is
// trimmed, while the UID's meaningful internal space is left untouched
// for percent-encoding. This is the distinction that matters: AVM's UIDs
// are of the form "<5 digits><space><7 digits>[-<n>]", so removing every
// space (as opposed to just trimming the ends) silently breaks every
// lookup for a physical device or DECT-ULE unit.
func TestGetMeasurementTrimsOnlySurroundingWhitespace(t *testing.T) {
	var seenRawPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenRawPath = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"interfaces":{"multimeterInterface":{"power":0,"voltage":230000,"current":0,"energy":0}}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "u", "p")
	c.sid = "deadbeef"

	if _, err := c.GetMeasurement(context.Background(), "  16000 0036532-1  "); err != nil {
		t.Fatal(err)
	}
	if want := "/api/v0/smarthome/overview/units/16000%200036532-1"; seenRawPath != want {
		t.Fatalf("escaped path: got %s, want %s", seenRawPath, want)
	}
}

// TestSmokeJSONParse keeps us honest if the schema evolves.
func TestSmokeJSONParse(t *testing.T) {
	body := []byte(`{"UID":"x","interfaces":{"multimeterInterface":{"power":-100,"voltage":230000,"current":434,"energy":10}}}`)
	var u unitResponse
	if err := json.Unmarshal(body, &u); err != nil {
		t.Fatal(err)
	}
	if u.Interfaces.Multimeter.Power != -100 {
		t.Fatalf("got %d", u.Interfaces.Multimeter.Power)
	}
}

// TestParentDeviceUID covers both real-world shapes seen from a live
// FRITZ!Box: single-function devices (FSE 200/210 sockets) whose unit
// UID has no suffix at all, and multi-function/multi-unit devices (an
// FSE 250 whose firmware exposes a general "avmMeter" unit "<AIN>-1"
// and a feed-in-only "avmMeterFeedIn" unit "<AIN>-2") whose unit UID is
// "<deviceUID>-<n>". It also covers the same shapes entered without
// AVM's internal AIN space, confirming NormalizeAIN reinserts it before
// the suffix is stripped.
func TestParentDeviceUID(t *testing.T) {
	cases := []struct{ in, want string }{
		{"16000 0036532-1", "16000 0036532"},
		{"16000 0036532-2", "16000 0036532"},
		{"11630 0420063", "11630 0420063"},       // no suffix: no-op
		{"  16000 0036532-1  ", "16000 0036532"}, // surrounding whitespace trimmed
		{"", ""},
		{"-1", "-1"},                        // no device UID left of the dash: leave untouched
		{"160000036532-1", "16000 0036532"}, // no space, with suffix
		{"160000036532-2", "16000 0036532"}, // no space, with suffix
		{"116300420063", "11630 0420063"},   // no space, no suffix
	}
	for _, c := range cases {
		if got := ParentDeviceUID(c.in); got != c.want {
			t.Errorf("ParentDeviceUID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestNormalizeAIN exercises the canonicalization rules directly: the
// standard 12-digit AIN shape gets its space inserted (or left alone)
// regardless of an optional "-<n>" sub-unit suffix, surrounding
// whitespace is always trimmed, and anything that doesn't look like a
// 12-digit AIN is passed through unchanged (aside from that trim) so
// short test UIDs, fritzhome-cache aliases, and other non-standard
// values are never mangled.
func TestNormalizeAIN(t *testing.T) {
	cases := []struct{ in, want string }{
		{"116570123456", "11657 0123456"},          // no space -> inserted
		{"11657 0123456", "11657 0123456"},         // already canonical -> unchanged
		{"160000036532-1", "16000 0036532-1"},      // no space, with suffix
		{"16000 0036532-1", "16000 0036532-1"},     // already canonical, with suffix
		{"  116570123456  ", "11657 0123456"},      // surrounding whitespace + no space
		{"  16000 0036532-1  ", "16000 0036532-1"}, // surrounding whitespace only
		{"", ""},
		{"123", "123"},                       // too short: left alone
		{"abc", "abc"},                       // not digits: left alone
		{"1234567890123", "1234567890123"},   // 13 digits: not the AIN shape, left alone
		{"-1", "-1"},                         // no device UID left of the dash
		{"160000036532-x", "160000036532-x"}, // non-digit suffix: not treated as a sub-unit
	}
	for _, c := range cases {
		if got := NormalizeAIN(c.in); got != c.want {
			t.Errorf("NormalizeAIN(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// fakeUnit and fakeFritzBox let ResolveUnitUID's tests simulate a FRITZ!Box
// serving several units under one device without spinning up real crypto or
// a real login flow (ResolveUnitUID never calls Login itself; the tests
// preseed c.sid).
type fakeUnit struct {
	unitType      string
	hasMultimeter bool
}

// newFakeFritzBox returns an httptest.Server that serves
// /smarthome/overview/devices/{deviceUID} from deviceUnitUIDs, and
// /smarthome/overview/units/{UID} from units (a UID missing from units
// results in a FRITZ!-shaped "UID_NOT_FOUND" 400, matching the real box).
func newFakeFritzBox(t *testing.T, deviceUID string, deviceUnitUIDs []string, units map[string]fakeUnit) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/smarthome/overview/devices/", func(w http.ResponseWriter, r *http.Request) {
		uid := strings.TrimPrefix(r.URL.Path, "/api/v0/smarthome/overview/devices/")
		if uid != deviceUID {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":[{"message":"[UID_NOT_FOUND] no valid device for given UID","code":2001}]}`))
			return
		}
		uids, _ := json.Marshal(deviceUnitUIDs)
		_, _ = w.Write([]byte(`{"UID":"` + deviceUID + `","unitUids":` + string(uids) + `}`))
	})
	mux.HandleFunc("/api/v0/smarthome/overview/units/", func(w http.ResponseWriter, r *http.Request) {
		uid := strings.TrimPrefix(r.URL.Path, "/api/v0/smarthome/overview/units/")
		u, ok := units[uid]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":[{"message":"[UID_NOT_FOUND] no valid unit for given uid","code":2001}]}`))
			return
		}
		if !u.hasMultimeter {
			_, _ = w.Write([]byte(`{"UID":"` + uid + `","unitType":"` + u.unitType + `","interfaces":{}}`))
			return
		}
		_, _ = w.Write([]byte(`{"UID":"` + uid + `","unitType":"` + u.unitType +
			`","interfaces":{"multimeterInterface":{"state":"valid","voltage":230000,"current":100,"power":500,"energy":1}}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestResolveUnitUIDFastPath confirms that when the configured value
// already has a usable multimeterInterface, ResolveUnitUID returns it
// unchanged without ever touching the device-discovery endpoint.
func TestResolveUnitUIDFastPath(t *testing.T) {
	var deviceEndpointHit bool
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/smarthome/overview/devices/", func(w http.ResponseWriter, r *http.Request) {
		deviceEndpointHit = true
		w.WriteHeader(http.StatusBadRequest)
	})
	mux.HandleFunc("/api/v0/smarthome/overview/units/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"UID":"11657 0123456","unitType":"avmPlugSocket","interfaces":{"multimeterInterface":{"state":"valid","voltage":230000,"current":100,"power":500,"energy":1}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL, "u", "p")
	c.sid = "deadbeef"

	got, err := c.ResolveUnitUID(context.Background(), "11657 0123456")
	if err != nil {
		t.Fatalf("ResolveUnitUID: %v", err)
	}
	if got != "11657 0123456" {
		t.Fatalf("got %q, want unchanged configured value", got)
	}
	if deviceEndpointHit {
		t.Fatal("fast path should never call the device-discovery endpoint")
	}
}

// TestResolveUnitUIDFastPathNormalizesMissingSpace confirms the fast
// path also succeeds when the operator configured fritz-unit as a bare
// run of digits (no internal AIN space): NormalizeAIN must canonicalize
// it before the very first GetMeasurement attempt so this still counts
// as the fast path (the device-discovery endpoint is never hit), and
// the value returned is the canonical, space-containing form rather
// than the raw configured one.
func TestResolveUnitUIDFastPathNormalizesMissingSpace(t *testing.T) {
	var deviceEndpointHit bool
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/smarthome/overview/devices/", func(w http.ResponseWriter, r *http.Request) {
		deviceEndpointHit = true
		w.WriteHeader(http.StatusBadRequest)
	})
	mux.HandleFunc("/api/v0/smarthome/overview/units/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"UID":"11657 0123456","unitType":"avmPlugSocket","interfaces":{"multimeterInterface":{"state":"valid","voltage":230000,"current":100,"power":500,"energy":1}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL, "u", "p")
	c.sid = "deadbeef"

	got, err := c.ResolveUnitUID(context.Background(), "116570123456")
	if err != nil {
		t.Fatalf("ResolveUnitUID: %v", err)
	}
	if got != "11657 0123456" {
		t.Fatalf("got %q, want the canonical space-containing form", got)
	}
	if deviceEndpointHit {
		t.Fatal("fast path should never call the device-discovery endpoint, even for a no-space input")
	}
}

// TestResolveUnitUIDDiscoversGeneralMeterOverFeedIn covers the FSE 250
// case this feature exists for: the configured value is the bare device
// UID (no multimeterInterface of its own), and discovery must prefer
// the "avmMeter" sub-unit over "avmMeterFeedIn" regardless of the order
// AVM lists them in.
func TestResolveUnitUIDDiscoversGeneralMeterOverFeedIn(t *testing.T) {
	srv := newFakeFritzBox(t, "16000 0036532",
		[]string{"16000 0036532-2", "16000 0036532-1"}, // feed-in listed first on purpose
		map[string]fakeUnit{
			"16000 0036532-1": {unitType: "avmMeter", hasMultimeter: true},
			"16000 0036532-2": {unitType: "avmMeterFeedIn", hasMultimeter: true},
		})

	c := NewClient(srv.URL, "u", "p")
	c.sid = "deadbeef"

	got, err := c.ResolveUnitUID(context.Background(), "16000 0036532")
	if err != nil {
		t.Fatalf("ResolveUnitUID: %v", err)
	}
	if got != "16000 0036532-1" {
		t.Fatalf("got %q, want the avmMeter sub-unit", got)
	}
}

// TestResolveUnitUIDRefusesFeedInOnly confirms that if the only
// discoverable measurement unit looks feed-in-only, ResolveUnitUID
// refuses to auto-select it (returning an error) rather than risking
// wrong-sign data reaching the power regulator.
func TestResolveUnitUIDRefusesFeedInOnly(t *testing.T) {
	srv := newFakeFritzBox(t, "16000 0036532",
		[]string{"16000 0036532-2"},
		map[string]fakeUnit{
			"16000 0036532-2": {unitType: "avmMeterFeedIn", hasMultimeter: true},
		})

	c := NewClient(srv.URL, "u", "p")
	c.sid = "deadbeef"

	_, err := c.ResolveUnitUID(context.Background(), "16000 0036532")
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	if !strings.Contains(err.Error(), "feed-in") {
		t.Fatalf("expected a feed-in-only refusal, got: %v", err)
	}
}

// TestResolveUnitUIDNoUsableUnit confirms a clear error (not a panic or
// a silently wrong answer) when the device's units have no
// multimeterInterface at all.
func TestResolveUnitUIDNoUsableUnit(t *testing.T) {
	srv := newFakeFritzBox(t, "16000 0036532",
		[]string{"16000 0036532-3"},
		map[string]fakeUnit{
			"16000 0036532-3": {unitType: "avmDECTHandset", hasMultimeter: false},
		})

	c := NewClient(srv.URL, "u", "p")
	c.sid = "deadbeef"

	_, err := c.ResolveUnitUID(context.Background(), "16000 0036532")
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	if !strings.Contains(err.Error(), "usable multimeterInterface") {
		t.Fatalf("expected a no-usable-unit error, got: %v", err)
	}
}

// TestResolveUnitUIDDeviceFetchFails confirms a clear, wrapped error
// when even the parent-device fallback lookup fails (e.g. a genuine
// typo in fritz-unit that matches no device at all).
func TestResolveUnitUIDDeviceFetchFails(t *testing.T) {
	srv := newFakeFritzBox(t, "16000 0036532", nil, nil)

	c := NewClient(srv.URL, "u", "p")
	c.sid = "deadbeef"

	_, err := c.ResolveUnitUID(context.Background(), "99999 9999999")
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	if !strings.Contains(err.Error(), "99999 9999999") {
		t.Fatalf("expected the error to mention the configured UID, got: %v", err)
	}
}
