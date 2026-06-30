// Package shelly serves a (subset of the) Shelly Gen2 RPC HTTP API for a
// virtual "Shelly Pro 3 EM" (in triphase profile). Only the read-only
// endpoints that Solakon ONE (and most other consumers) actually call are
// implemented; configuration setters return method-not-found errors.
//
// References:
//   - https://shelly-api-docs.shelly.cloud/gen2/Devices/Gen2/ShellyPro3EM
//   - https://shelly-api-docs.shelly.cloud/gen2/ComponentsAndServices/EM
//   - https://shelly-api-docs.shelly.cloud/gen2/ComponentsAndServices/EMData
package shelly

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/flunaras/shelly-fritz-proxy/internal/meter"
	"github.com/flunaras/shelly-fritz-proxy/internal/safeguard"
)

// StalePolicy describes what the EM.GetStatus / EMData.GetStatus
// responses look like when the underlying FRITZ data has been
// classified as stale by the safeguard. Picking the right policy is
// the single most important safety knob in this proxy.
type StalePolicy int

const (
	// StalePolicyError returns the structurally-valid Shelly response
	// with nulled measurements and errors: ["stale_data"]. A real
	// Shelly Pro 3 EM does this on power_meter_failure. Solakon ONE
	// *should* pause regulation when it sees this. This is the
	// recommended default.
	StalePolicyError StalePolicy = iota
	// StalePolicyFreeze returns the last accepted measurements
	// unchanged, but still sets errors: ["stale_data"]. Use when you
	// want dashboards to stay continuous while still flagging the
	// problem to clients that respect errors[].
	StalePolicyFreeze
	// StalePolicySafeImport reports a large positive grid-import value
	// (configured via SafeImportWatts). Solakon will throttle the
	// inverter to its minimum output because it thinks the house is
	// drawing more than the inverter can supply. Pick a value larger
	// than your peak inverter output. Use only when Solakon mishandles
	// the StalePolicyError mode.
	StalePolicySafeImport
	// StalePolicyZero reports 0 W power and 0 errors. DANGEROUS.
	// Never the default - included only because some clients require
	// explicit "everything fine" responses to work at all. Solakon
	// will *increase* output unbounded, defeating the entire point of
	// having a guard.
	StalePolicyZero
)

// ParseStalePolicy maps the config-file string to a StalePolicy.
func ParseStalePolicy(s string) (StalePolicy, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "error":
		return StalePolicyError, nil
	case "freeze":
		return StalePolicyFreeze, nil
	case "safe_import", "safe-import":
		return StalePolicySafeImport, nil
	case "zero":
		return StalePolicyZero, nil
	}
	return 0, errors.New("stale-policy must be one of: error|freeze|safe_import|zero")
}

// StaleConfig bundles the user-tunable parts of stale-data handling.
type StaleConfig struct {
	Policy StalePolicy
	// SafeImportWatts is the value reported as total_act_power when
	// Policy == StalePolicySafeImport. Default 5000 W.
	SafeImportWatts float64
}

// Server emulates a Shelly Pro 3 EM (triphase profile).
type Server struct {
	Store       *meter.Store
	MAC         string // upper-case, no separators, e.g. "AABBCCDDEEFF"
	DeviceID    string // lower-case "shellypro3em-<mac>" by convention
	Logger      *slog.Logger
	StaleConfig StaleConfig

	StartTime time.Time

	bootTS  time.Time
	rpcSeq  atomic.Uint64
	rebootR atomic.Uint64
}

// New builds a Server. The MAC is reused both in /shelly responses and
// in the synthetic mDNS host name. It does not need to match the
// FRITZ!Box's MAC; pick something stable and unique on your LAN.
//
// staleCfg controls the fail-safe behaviour when the safeguard flags
// the upstream data as stale. Pass StaleConfig{} for the recommended
// defaults (StalePolicyError, 5000 W safe-import).
func New(store *meter.Store, mac string, staleCfg StaleConfig, logger *slog.Logger) *Server {
	mac = strings.ToUpper(strings.ReplaceAll(mac, ":", ""))
	if staleCfg.SafeImportWatts <= 0 {
		staleCfg.SafeImportWatts = 5000
	}
	return &Server{
		Store:       store,
		MAC:         mac,
		DeviceID:    "shellypro3em-" + strings.ToLower(mac),
		Logger:      logger,
		StaleConfig: staleCfg,
		StartTime:   time.Now(),
		bootTS:      time.Now(),
	}
}

// Register adds the Shelly endpoints to the given mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/shelly", s.handleShelly)
	mux.HandleFunc("/rpc", s.handleRPC)        // POST {"id":..,"method":..,"params":..}
	mux.HandleFunc("/rpc/", s.handleRPCMethod) // GET /rpc/<Method>?<params>
}

// handleShelly serves the device discovery info that Shelly returns at /shelly.
// This is the first thing that any Shelly-aware client checks.
func (s *Server) handleShelly(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.shellyInfo())
}

func (s *Server) shellyInfo() map[string]any {
	return map[string]any{
		"name":      nil,
		"id":        s.DeviceID,
		"mac":       s.MAC,
		"slot":      0,
		"model":     "SPEM-003CEBEU",
		"gen":       2,
		"fw_id":     "20241011-114449/1.4.4-g6d2a586",
		"ver":       "1.4.4",
		"app":       "Pro3EM",
		"auth_en":   false,
		"auth_domain": nil,
		"profile":   "triphase",
	}
}

// rpcRequest is the JSON-RPC envelope used by Gen2 devices.
type rpcRequest struct {
	ID     any             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	ID     any   `json:"id"`
	Src    string `json:"src,omitempty"`
	Dst    string `json:"dst,omitempty"`
	Result any    `json:"result,omitempty"`
	Error  *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req rpcRequest
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if len(body) == 0 {
			http.Error(w, "empty body", http.StatusBadRequest)
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		req.Method = strings.TrimPrefix(r.URL.Path, "/rpc")
		req.Method = strings.TrimPrefix(req.Method, "/")
		// /rpc?id=..&method=..&params=..  is rarely used; ignore.
	}
	result, rerr := s.dispatch(req.Method, req.Params)
	id := req.ID
	if id == nil {
		id = s.rpcSeq.Add(1)
	}
	resp := rpcResponse{ID: id, Src: s.DeviceID, Result: result, Error: rerr}
	writeJSON(w, http.StatusOK, resp)
}

// handleRPCMethod serves the GET /rpc/<Method> shorthand. Query parameters
// are forwarded as a json object to dispatch; we only need to cope with
// scalar params (e.g. ?id=0) which is what all the read-only methods use.
func (s *Server) handleRPCMethod(w http.ResponseWriter, r *http.Request) {
	method := strings.TrimPrefix(r.URL.Path, "/rpc/")
	if method == "" {
		http.NotFound(w, r)
		return
	}
	// Build a minimal params object from the query string.
	params := map[string]any{}
	for k, vs := range r.URL.Query() {
		if len(vs) == 0 {
			continue
		}
		v := vs[0]
		// Try int, then bool, fall back to string.
		switch v {
		case "true":
			params[k] = true
		case "false":
			params[k] = false
		default:
			if n, err := jsonNumber(v); err == nil {
				params[k] = n
			} else {
				params[k] = v
			}
		}
	}
	raw, _ := json.Marshal(params)
	result, rerr := s.dispatch(method, raw)
	if rerr != nil {
		writeJSON(w, http.StatusOK, rerr)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// dispatch dispatches one RPC call. It never returns an HTTP error; protocol
// errors are returned via rpcError so the caller can wrap them appropriately.
func (s *Server) dispatch(method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	// --- Device-level ---
	case "Shelly.GetDeviceInfo":
		return s.shellyInfo(), nil
	case "Shelly.GetStatus":
		return s.fullStatus(), nil
	case "Shelly.GetConfig":
		return s.fullConfig(), nil
	case "Shelly.ListMethods":
		return map[string]any{"methods": []string{
			"Shelly.GetDeviceInfo", "Shelly.GetStatus", "Shelly.GetConfig",
			"Shelly.ListMethods", "Sys.GetStatus", "Sys.GetConfig",
			"WiFi.GetStatus", "WiFi.GetConfig",
			"EM.GetStatus", "EM.GetConfig", "EMData.GetStatus", "EMData.GetConfig",
		}}, nil
	case "Shelly.Reboot":
		s.rebootR.Add(1)
		return map[string]any{}, nil

	// --- Sys ---
	case "Sys.GetStatus":
		return s.sysStatus(), nil
	case "Sys.GetConfig":
		return s.sysConfig(), nil

	// --- WiFi (Solakon checks reachability) ---
	case "WiFi.GetStatus":
		return s.wifiStatus(), nil
	case "WiFi.GetConfig":
		return s.wifiConfig(), nil

	// --- EM (the important bit) ---
	case "EM.GetStatus":
		return s.emStatus(), nil
	case "EM.GetConfig":
		return s.emConfig(), nil
	case "EM.GetCTTypes":
		return map[string]any{"supported": []string{"120A", "400A"}}, nil

	// --- EMData (cumulative energies) ---
	case "EMData.GetStatus":
		return s.emDataStatus(), nil
	case "EMData.GetConfig":
		return map[string]any{"id": 0, "name": nil}, nil

	// --- Ethernet / Cloud / MQTT / BLE stubs ---
	case "Eth.GetStatus":
		return map[string]any{"ip": nil}, nil
	case "Eth.GetConfig":
		return map[string]any{"enable": false}, nil
	case "Cloud.GetStatus":
		return map[string]any{"connected": false}, nil
	case "Cloud.GetConfig":
		return map[string]any{"enable": false, "server": nil}, nil
	case "MQTT.GetStatus":
		return map[string]any{"connected": false}, nil
	case "MQTT.GetConfig":
		return map[string]any{"enable": false}, nil
	case "BLE.GetStatus":
		return map[string]any{}, nil
	case "BLE.GetConfig":
		return map[string]any{"enable": false}, nil
	}
	return nil, &rpcError{Code: -32601, Message: "Method " + method + " not Found"}
}

// --- Payload builders ---

func (s *Server) sysStatus() map[string]any {
	return map[string]any{
		"mac":                  s.MAC,
		"restart_required":     false,
		"time":                 time.Now().Format("15:04"),
		"unixtime":             time.Now().Unix(),
		"uptime":               int(time.Since(s.bootTS).Seconds()),
		"ram_size":             253900,
		"ram_free":             107412,
		"fs_size":              524288,
		"fs_free":              159744,
		"cfg_rev":              17,
		"kvs_rev":              0,
		"schedule_rev":         0,
		"webhook_rev":          0,
		"available_updates":    map[string]any{},
		"reset_reason":         3,
	}
}

func (s *Server) sysConfig() map[string]any {
	return map[string]any{
		"device": map[string]any{
			"name":          "Shelly Pro 3EM (FRITZ proxy)",
			"mac":           s.MAC,
			"fw_id":         "20241011-114449/1.4.4-g6d2a586",
			"profile":       "triphase",
			"discoverable":  true,
			"eco_mode":      false,
		},
		"location": map[string]any{
			"tz":  "Europe/Berlin",
			"lat": nil,
			"lon": nil,
		},
		"debug": map[string]any{
			"mqtt": map[string]any{"enable": false},
			"websocket": map[string]any{"enable": false},
			"udp": map[string]any{"addr": nil},
		},
		"ui_data":              map[string]any{},
		"rpc_udp":              map[string]any{},
		"sntp":                 map[string]any{"server": "time.google.com"},
		"cfg_rev":              17,
	}
}

func (s *Server) wifiStatus() map[string]any {
	return map[string]any{
		"sta_ip": "",
		"status": "got ip",
		"ssid":   "fake-wifi",
		"rssi":   -60,
	}
}

func (s *Server) wifiConfig() map[string]any {
	return map[string]any{
		"ap": map[string]any{"enable": false},
		"sta": map[string]any{"ssid": "fake-wifi", "enable": true},
	}
}

func (s *Server) fullStatus() map[string]any {
	return map[string]any{
		"sys":      s.sysStatus(),
		"wifi":     s.wifiStatus(),
		"ble":      map[string]any{},
		"cloud":    map[string]any{"connected": false},
		"mqtt":     map[string]any{"connected": false},
		"em:0":     s.emStatus(),
		"emdata:0": s.emDataStatus(),
		"eth":      map[string]any{"ip": nil},
	}
}

func (s *Server) fullConfig() map[string]any {
	return map[string]any{
		"sys":      s.sysConfig(),
		"wifi":     s.wifiConfig(),
		"ble":      map[string]any{"enable": false},
		"cloud":    map[string]any{"enable": false, "server": nil},
		"mqtt":     map[string]any{"enable": false},
		"em:0":     s.emConfig(),
		"emdata:0": map[string]any{"id": 0, "name": nil},
		"eth":      map[string]any{"enable": false},
	}
}

func (s *Server) emConfig() map[string]any {
	return map[string]any{
		"id":                     0,
		"name":                   nil,
		"blink_mode_selector":    "active_energy",
		"phase_selector":         "all",
		"monitor_phase_sequence": false,
		"ct_type":                "120A",
	}
}

// emStatus returns the EM.GetStatus payload that consumers like Solakon
// ONE read on every cycle. It must match the exact field names and types
// from the Shelly Gen2 documentation.
//
// When the safeguard's health is not Ok, the configured StalePolicy
// determines what the payload looks like: error (default, recommended),
// freeze (last good values + errors flag), safe_import (large positive
// power so the inverter throttles), or zero (DANGEROUS).
func (s *Server) emStatus() map[string]any {
	state, health, ok := s.Store.Snapshot()
	if !ok || health.Health != safeguard.HealthOk {
		return s.emStatusUnsafe(state, health, ok)
	}
	return s.emStatusHealthy(state, []string{})
}

// emStatusUnsafe emits the EM.GetStatus payload appropriate to the
// current StalePolicy. The errors[] string used is stable across all
// policies so log scrapers and dashboards can key off it.
func (s *Server) emStatusUnsafe(state meter.State, health meter.HealthSnapshot, hasData bool) map[string]any {
	errs := buildErrors(health, hasData)
	switch s.StaleConfig.Policy {
	case StalePolicyZero:
		return s.emStatusHealthy(meter.State{}, errs)
	case StalePolicyFreeze:
		if !hasData {
			return s.emStatusEmpty(errs)
		}
		return s.emStatusHealthy(state, errs)
	case StalePolicySafeImport:
		// Synthesize an all-import state at SafeImportWatts.
		w := s.StaleConfig.SafeImportWatts
		synth := meter.State{
			A: meter.Phase{Voltage: 230, Current: w / 3 / 230, ActivePower: w / 3, Frequency: 50},
			B: meter.Phase{Voltage: 230, Current: w / 3 / 230, ActivePower: w / 3, Frequency: 50},
			C: meter.Phase{Voltage: 230, Current: w / 3 / 230, ActivePower: w / 3, Frequency: 50},
			TotalCurrent:  w / 230,
			TotalPower:    w,
			TotalEnergy:   state.TotalEnergy,
		}
		return s.emStatusHealthy(synth, errs)
	default: // StalePolicyError
		return s.emStatusEmpty(errs)
	}
}

// emStatusEmpty returns the all-null EM.GetStatus shape with errs[].
func (s *Server) emStatusEmpty(errs []string) map[string]any {
	return map[string]any{
		"id":               0,
		"a_current":        nil, "a_voltage": nil, "a_act_power": nil,
		"a_aprt_power":     nil, "a_pf": nil, "a_freq": nil,
		"a_errors":         []string{}, "a_flags": []string{},
		"b_current":        nil, "b_voltage": nil, "b_act_power": nil,
		"b_aprt_power":     nil, "b_pf": nil, "b_freq": nil,
		"b_errors":         []string{}, "b_flags": []string{},
		"c_current":        nil, "c_voltage": nil, "c_act_power": nil,
		"c_aprt_power":     nil, "c_pf": nil, "c_freq": nil,
		"c_errors":         []string{}, "c_flags": []string{},
		"n_current":        nil,
		"total_current":    nil,
		"total_act_power":  nil,
		"total_aprt_power": nil,
		"user_calibrated_phase": []string{},
		"errors":           errs,
	}
}

func (s *Server) emStatusHealthy(state meter.State, errs []string) map[string]any {
	return map[string]any{
		"id":                    0,
		"a_current":             round(state.A.Current, 3),
		"a_voltage":             round(state.A.Voltage, 1),
		"a_act_power":           round(state.A.ActivePower, 1),
		"a_aprt_power":          round(absf(state.A.ActivePower), 1),
		"a_pf":                  pf(state.A.ActivePower),
		"a_freq":                round(state.A.Frequency, 1),
		"a_errors":              []string{},
		"a_flags":               []string{},
		"b_current":             round(state.B.Current, 3),
		"b_voltage":             round(state.B.Voltage, 1),
		"b_act_power":           round(state.B.ActivePower, 1),
		"b_aprt_power":          round(absf(state.B.ActivePower), 1),
		"b_pf":                  pf(state.B.ActivePower),
		"b_freq":                round(state.B.Frequency, 1),
		"b_errors":              []string{},
		"b_flags":               []string{},
		"c_current":             round(state.C.Current, 3),
		"c_voltage":             round(state.C.Voltage, 1),
		"c_act_power":           round(state.C.ActivePower, 1),
		"c_aprt_power":          round(absf(state.C.ActivePower), 1),
		"c_pf":                  pf(state.C.ActivePower),
		"c_freq":                round(state.C.Frequency, 1),
		"c_errors":              []string{},
		"c_flags":               []string{},
		"n_current":             nil,
		"total_current":         round(state.TotalCurrent, 3),
		"total_act_power":       round(state.TotalPower, 1),
		"total_aprt_power":      round(absf(state.TotalPower), 1),
		"user_calibrated_phase": []string{},
		"errors":                errs,
	}
}

// buildErrors translates the safeguard's health/reason into the
// errors[] strings that EM.GetStatus advertises. We use stable strings
// so consumers can branch on them.
func buildErrors(h meter.HealthSnapshot, hasData bool) []string {
	if !hasData && h.Health == safeguard.HealthInit {
		return []string{"no_data"}
	}
	switch h.Health {
	case safeguard.HealthStale:
		// Map the most informative reason into a stable error tag.
		switch h.Reason {
		case safeguard.ReasonStuckData:
			return []string{"stale_data", "stuck_data"}
		case safeguard.ReasonFreshnessTimeout:
			return []string{"stale_data", "freshness_timeout"}
		case safeguard.ReasonPollFailure:
			return []string{"stale_data", "poll_failure"}
		case safeguard.ReasonDeviceOffline:
			return []string{"stale_data", "device_offline"}
		}
		return []string{"stale_data"}
	case safeguard.HealthDegraded:
		return []string{"degraded"}
	}
	return []string{}
}

func (s *Server) emDataStatus() map[string]any {
	state, health, ok := s.Store.Snapshot()
	stale := !ok || health.Health != safeguard.HealthOk
	// Use last-known energy counters even when stale; cumulative
	// energy is monotonic and almost never wrong even if power is.
	in := state.EnergyIn
	out := state.EnergyOut
	if in == 0 && out == 0 {
		in = state.TotalEnergy
	}
	third := func(v float64) float64 { return round(v/3.0, 3) }
	resp := map[string]any{
		"id":                     0,
		"a_total_act_energy":     third(in),
		"a_total_act_ret_energy": third(out),
		"b_total_act_energy":     third(in),
		"b_total_act_ret_energy": third(out),
		"c_total_act_energy":     third(in),
		"c_total_act_ret_energy": third(out),
		"total_act":              round(in, 3),
		"total_act_ret":          round(out, 3),
	}
	if stale {
		resp["errors"] = buildErrors(health, ok)
	}
	return resp
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func round(v float64, decimals int) float64 {
	if isNaN(v) {
		return 0
	}
	pow := 1.0
	for i := 0; i < decimals; i++ {
		pow *= 10
	}
	return float64(int64(v*pow+sign(v)*0.5)) / pow
}

func absf(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func sign(f float64) float64 {
	if f < 0 {
		return -1
	}
	return 1
}

func isNaN(f float64) bool { return f != f }

func pf(p float64) float64 {
	// We do not know the actual power factor; report 1.0 for non-zero loads
	// and 0 when idle. Consumers like Solakon do not act on PF.
	if absf(p) < 1 {
		return 0
	}
	return 1
}

func jsonNumber(s string) (any, error) {
	var n json.Number
	if err := json.Unmarshal([]byte(s), &n); err != nil {
		return nil, err
	}
	if i, err := n.Int64(); err == nil {
		return i, nil
	}
	if f, err := n.Float64(); err == nil {
		return f, nil
	}
	return nil, fmt.Errorf("not a number: %q", s)
}
