// Package shellyclient implements a minimal HTTP client for the Shelly
// Gen2 RPC API exposed by this proxy's internal/shelly.Server (and, since
// the wire format is the same, by a real Shelly Pro 3 EM). It exists so
// the proxy's own "query" subcommand and any ad-hoc diagnostics can talk
// to the emulated device the same way Solakon ONE or evcc would, without
// pulling in an external Shelly SDK.
//
// The client is read-only by convention: it only ever issues Get*
// methods. Nothing stops a caller from passing a mutating method name to
// Call, but this proxy's server rejects those with -32601 anyway.
package shellyclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client queries a Shelly Gen2 RPC HTTP API at BaseURL (e.g.
// "http://192.168.1.50" or "http://192.168.1.50:80"). It is safe for
// concurrent use.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New builds a Client. baseURL is normalized by trimming a trailing
// slash. If httpClient is nil, a client with a 10s timeout is used.
func New(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		HTTP:    httpClient,
	}
}

// RPCError mirrors the JSON-RPC error envelope returned by the Shelly
// Gen2 RPC API (e.g. {"code": -32601, "message": "Method ... not Found"}).
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// rpcEnvelope decodes the outer {"id":...,"result":...,"error":...}
// shape returned by POST /rpc.
type rpcEnvelope struct {
	ID     any             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *RPCError       `json:"error"`
}

// Call invokes an arbitrary RPC method via POST /rpc and decodes the
// result into out (which may be nil to discard the result). params, if
// non-nil, is marshalled as the request's "params" field.
func (c *Client) Call(ctx context.Context, method string, params any, out any) error {
	reqBody := map[string]any{
		"id":     1,
		"method": method,
	}
	if params != nil {
		reqBody["params"] = params
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("shellyclient: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/rpc", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("shellyclient: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("shellyclient: %s: %w", method, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("shellyclient: %s: read response: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("shellyclient: %s: unexpected status %d: %s", method, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var env rpcEnvelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		return fmt.Errorf("shellyclient: %s: decode envelope: %w", method, err)
	}
	if env.Error != nil {
		return env.Error
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("shellyclient: %s: decode result: %w", method, err)
		}
	}
	return nil
}

// Discover fetches GET /shelly, the unauthenticated discovery payload
// every Shelly Gen2 device serves. Useful as a first liveness/identity
// check before issuing RPC calls.
func (c *Client) Discover(ctx context.Context) (*DeviceDiscovery, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/shelly", nil)
	if err != nil {
		return nil, fmt.Errorf("shellyclient: build request: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("shellyclient: discover: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("shellyclient: discover: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("shellyclient: discover: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out DeviceDiscovery
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("shellyclient: discover: decode: %w", err)
	}
	return &out, nil
}

// DeviceDiscovery is the payload served at GET /shelly.
type DeviceDiscovery struct {
	ID      string `json:"id"`
	MAC     string `json:"mac"`
	Model   string `json:"model"`
	Gen     int    `json:"gen"`
	FwID    string `json:"fw_id"`
	Ver     string `json:"ver"`
	App     string `json:"app"`
	AuthEn  bool   `json:"auth_en"`
	Profile string `json:"profile"`
}

// DeviceInfo is the result of Shelly.GetDeviceInfo.
type DeviceInfo struct {
	ID      string `json:"id"`
	MAC     string `json:"mac"`
	Model   string `json:"model"`
	Gen     int    `json:"gen"`
	FwID    string `json:"fw_id"`
	Ver     string `json:"ver"`
	App     string `json:"app"`
	AuthEn  bool   `json:"auth_en"`
	Profile string `json:"profile"`
}

// GetDeviceInfo calls Shelly.GetDeviceInfo.
func (c *Client) GetDeviceInfo(ctx context.Context) (*DeviceInfo, error) {
	var out DeviceInfo
	if err := c.Call(ctx, "Shelly.GetDeviceInfo", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// EMStatus is the result of EM.GetStatus for the triphase profile. All
// pointer fields are nil when the underlying data is unavailable (e.g.
// stale-policy "error").
type EMStatus struct {
	ID int `json:"id"`

	ACurrent   *float64 `json:"a_current"`
	AVoltage   *float64 `json:"a_voltage"`
	AActPower  *float64 `json:"a_act_power"`
	AAprtPower *float64 `json:"a_aprt_power"`
	APF        *float64 `json:"a_pf"`
	AFreq      *float64 `json:"a_freq"`

	BCurrent   *float64 `json:"b_current"`
	BVoltage   *float64 `json:"b_voltage"`
	BActPower  *float64 `json:"b_act_power"`
	BAprtPower *float64 `json:"b_aprt_power"`
	BPF        *float64 `json:"b_pf"`
	BFreq      *float64 `json:"b_freq"`

	CCurrent   *float64 `json:"c_current"`
	CVoltage   *float64 `json:"c_voltage"`
	CActPower  *float64 `json:"c_act_power"`
	CAprtPower *float64 `json:"c_aprt_power"`
	CPF        *float64 `json:"c_pf"`
	CFreq      *float64 `json:"c_freq"`

	NCurrent       *float64 `json:"n_current"`
	TotalCurrent   *float64 `json:"total_current"`
	TotalActPower  *float64 `json:"total_act_power"`
	TotalAprtPower *float64 `json:"total_aprt_power"`

	Errors []string `json:"errors"`
}

// GetEMStatus calls EM.GetStatus for the given component id (normally 0).
func (c *Client) GetEMStatus(ctx context.Context, id int) (*EMStatus, error) {
	var out EMStatus
	if err := c.Call(ctx, "EM.GetStatus", map[string]any{"id": id}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// EMDataStatus is the result of EMData.GetStatus.
type EMDataStatus struct {
	ID                 int      `json:"id"`
	ATotalActEnergy    float64  `json:"a_total_act_energy"`
	ATotalActRetEnergy float64  `json:"a_total_act_ret_energy"`
	BTotalActEnergy    float64  `json:"b_total_act_energy"`
	BTotalActRetEnergy float64  `json:"b_total_act_ret_energy"`
	CTotalActEnergy    float64  `json:"c_total_act_energy"`
	CTotalActRetEnergy float64  `json:"c_total_act_ret_energy"`
	TotalAct           float64  `json:"total_act"`
	TotalActRet        float64  `json:"total_act_ret"`
	Errors             []string `json:"errors,omitempty"`
}

// GetEMDataStatus calls EMData.GetStatus for the given component id.
func (c *Client) GetEMDataStatus(ctx context.Context, id int) (*EMDataStatus, error) {
	var out EMDataStatus
	if err := c.Call(ctx, "EMData.GetStatus", map[string]any{"id": id}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SysStatus is a partial decode of Sys.GetStatus; only fields useful for
// diagnostics are included.
type SysStatus struct {
	MAC             string `json:"mac"`
	RestartRequired bool   `json:"restart_required"`
	UnixTime        int64  `json:"unixtime"`
	Uptime          int    `json:"uptime"`
}

// GetSysStatus calls Sys.GetStatus.
func (c *Client) GetSysStatus(ctx context.Context) (*SysStatus, error) {
	var out SysStatus
	if err := c.Call(ctx, "Sys.GetStatus", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// HealthDetails mirrors the /healthz/details JSON produced by this
// proxy's makeHealthDetailsHandler. It is not part of the Shelly RPC
// surface, but is exposed by the same HTTP server, so it is convenient
// to query from the same client.
type HealthDetails struct {
	DeviceID               string  `json:"device_id"`
	Health                 string  `json:"health"`
	Reason                 string  `json:"reason"`
	LastAcceptedAgeSeconds float64 `json:"last_accepted_age_seconds"`
	StuckRunLength         int     `json:"stuck_run_length"`
	ConsecutiveRejects     int     `json:"consecutive_rejects"`
	HasEverHadData         bool    `json:"has_ever_had_data"`
	ProductOk              bool    `json:"product_ok"`
}

// GetHealthDetails fetches GET /healthz/details.
func (c *Client) GetHealthDetails(ctx context.Context) (*HealthDetails, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/healthz/details", nil)
	if err != nil {
		return nil, fmt.Errorf("shellyclient: build request: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("shellyclient: health details: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("shellyclient: health details: read response: %w", err)
	}
	// Note: /healthz/details intentionally returns 503 while unhealthy;
	// the body is still valid JSON in that case, so decode it either way.
	var out HealthDetails
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("shellyclient: health details: decode (status %d): %w", resp.StatusCode, err)
	}
	return &out, nil
}

// CallRaw is like Call but returns the raw JSON result without decoding
// it into a typed struct. Useful for --method arguments where the
// caller wants to pretty-print whatever comes back (e.g. Shelly.GetStatus,
// Shelly.GetConfig, WiFi.GetStatus).
func (c *Client) CallRaw(ctx context.Context, method string, params url.Values) (json.RawMessage, error) {
	var p any
	if len(params) > 0 {
		m := map[string]any{}
		for k, vs := range params {
			if len(vs) > 0 {
				m[k] = vs[0]
			}
		}
		p = m
	}
	var raw json.RawMessage
	if err := c.Call(ctx, method, p, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}
