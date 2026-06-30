// Package fritz is a small client for the FRITZ! Smart Home REST API
// (introduced with FRITZ!OS 7.50+ and stable since FRITZ!OS 8.20). It is
// specifically tailored to read live measurement data from the
// FRITZ!Smart Energy 250 (a bidirectional DIN-rail energy meter connected
// to a FRITZ!Box via DECT-ULE).
//
// The Smart Home REST API is preferred over the legacy AHA-HTTP-Interface
// because it allows transparent caching by services like
// https://github.com/flunaras/fritzhome-cache, which sits between this
// client and the FRITZ!Box and serves
// `GET /api/v0/smarthome/overview/units/{UID}` from its own cache.
//
// References:
//   - https://fritz.support/resources/SmarthomeRestApiFRITZOS82.html
//   - https://fritz.support/resources/HTTP_Session-ID_EN.pdf
package fritz

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

// Default REST API base path; same value that fritzhome-cache understands.
const defaultBasePath = "/api/v0"

// Client talks to a FRITZ!Box (or to a fritzhome-cache instance fronting
// one) using the Smart Home REST API. It transparently logs in and
// re-logs in on session expiry.
type Client struct {
	BaseURL  string // upstream, e.g. https://fritz.box or http://fritzhome-cache:8080
	BasePath string // path prefix, default "/api/v0"
	User     string
	Password string

	http *http.Client

	mu  sync.Mutex
	sid string // valid session id or "0000000000000000" if logged out
}

// NewClient creates a REST client pointed at baseURL. Pass a fritzhome-cache
// URL here to benefit from request coalescing and caching. The base path
// defaults to "/api/v0"; override via SetBasePath if your deployment differs.
//
// FRITZ!Box uses a self-signed certificate by default, so the embedded
// http.Client skips TLS verification when talking over https. If you front
// your FRITZ!Box or fritzhome-cache with a proper cert and want strict
// verification, set Client.http manually after construction.
func NewClient(baseURL, user, password string) *Client {
	return &Client{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		BasePath: defaultBasePath,
		User:     user,
		Password: password,
		http: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}
}

// SetBasePath overrides the REST base path (e.g. "" for direct calls
// against AVM staging firmware that does not yet use the /api/v0 prefix).
func (c *Client) SetBasePath(p string) {
	p = strings.TrimRight(p, "/")
	if p != "" && !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	c.BasePath = p
}

// sessionInfo is the small subset of /login_sid.lua that we care about.
type sessionInfo struct {
	XMLName   xml.Name `xml:"SessionInfo"`
	SID       string   `xml:"SID"`
	Challenge string   `xml:"Challenge"`
	BlockTime int      `xml:"BlockTime"`
}

const invalidSID = "0000000000000000"

// Login obtains a session id from the FRITZ!Box and stores it on the client.
// FRITZ!OS uses PBKDF2-SHA256 challenges since 7.24; we support both that and
// the legacy MD5 challenge for older devices.
//
// The /login_sid.lua endpoint is intentionally NOT cached by
// fritzhome-cache, so this call always reaches the FRITZ!Box.
func (c *Client) Login(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loginLocked(ctx)
}

func (c *Client) loginLocked(ctx context.Context) error {
	info, err := c.fetchSession(ctx, "")
	if err != nil {
		return fmt.Errorf("fritz: initial /login_sid.lua: %w", err)
	}
	if info.SID != invalidSID {
		c.sid = info.SID
		return nil
	}
	if info.BlockTime > 0 {
		return fmt.Errorf("fritz: login temporarily blocked for %ds (too many failed attempts)", info.BlockTime)
	}

	response, err := computeChallengeResponse(info.Challenge, c.Password)
	if err != nil {
		return fmt.Errorf("fritz: cannot compute challenge response: %w", err)
	}

	info, err = c.fetchSession(ctx, response)
	if err != nil {
		return fmt.Errorf("fritz: login /login_sid.lua: %w", err)
	}
	if info.SID == invalidSID {
		return errors.New("fritz: login failed (wrong user or password)")
	}
	c.sid = info.SID
	return nil
}

func (c *Client) fetchSession(ctx context.Context, response string) (*sessionInfo, error) {
	u := c.BaseURL + "/login_sid.lua?version=2"
	if response != "" {
		params := url.Values{}
		params.Set("username", c.User)
		params.Set("response", response)
		u += "&" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var info sessionInfo
	if err := xml.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("parse SessionInfo: %w (body=%q)", err, body)
	}
	return &info, nil
}

// computeChallengeResponse returns the response string expected by FRITZ!OS
// for the given challenge. The format is determined by the challenge prefix:
//
//   - "2$<iter1>$<salt1>$<iter2>$<salt2>" -> PBKDF2-SHA256 (modern)
//   - anything else                       -> legacy MD5(challenge-utf16le-password)
func computeChallengeResponse(challenge, password string) (string, error) {
	if strings.HasPrefix(challenge, "2$") {
		parts := strings.Split(challenge, "$")
		if len(parts) != 5 {
			return "", fmt.Errorf("unexpected pbkdf2 challenge format: %q", challenge)
		}
		iter1, err1 := strconv.Atoi(parts[1])
		iter2, err2 := strconv.Atoi(parts[3])
		if err1 != nil || err2 != nil {
			return "", fmt.Errorf("invalid iteration counts in challenge %q", challenge)
		}
		salt1, err1 := hex.DecodeString(parts[2])
		salt2, err2 := hex.DecodeString(parts[4])
		if err1 != nil || err2 != nil {
			return "", fmt.Errorf("invalid salts in challenge %q", challenge)
		}
		hash1 := pbkdf2Sha256([]byte(password), salt1, iter1, 32)
		hash2 := pbkdf2Sha256(hash1, salt2, iter2, 32)
		return parts[4] + "$" + hex.EncodeToString(hash2), nil
	}
	// Legacy MD5(challenge-utf16le-password)
	// FRITZ replaces any code point > 255 with "."
	pw := []rune(password)
	for i, r := range pw {
		if r > 255 {
			pw[i] = '.'
		}
	}
	combined := utf16le(challenge + "-" + string(pw))
	sum := md5.Sum(combined)
	return challenge + "-" + hex.EncodeToString(sum[:]), nil
}

func utf16le(s string) []byte {
	runes := []rune(s)
	u16 := utf16.Encode(runes)
	out := make([]byte, 0, len(u16)*2)
	for _, c := range u16 {
		out = append(out, byte(c), byte(c>>8))
	}
	return out
}

// pbkdf2Sha256 implements PBKDF2 with HMAC-SHA256. We inline a small version
// to avoid bringing in golang.org/x/crypto.
func pbkdf2Sha256(password, salt []byte, iter, keyLen int) []byte {
	hashLen := sha256.Size
	numBlocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, numBlocks*hashLen)
	var block [4]byte
	for i := 1; i <= numBlocks; i++ {
		block[0] = byte(i >> 24)
		block[1] = byte(i >> 16)
		block[2] = byte(i >> 8)
		block[3] = byte(i)
		u := hmacSha256(password, append(salt, block[:]...))
		t := append([]byte(nil), u...)
		for j := 1; j < iter; j++ {
			u = hmacSha256(password, u)
			for k := range t {
				t[k] ^= u[k]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

func hmacSha256(key, msg []byte) []byte {
	const blockSize = 64
	if len(key) > blockSize {
		s := sha256.Sum256(key)
		key = s[:]
	}
	if len(key) < blockSize {
		k := make([]byte, blockSize)
		copy(k, key)
		key = k
	}
	ipad := make([]byte, blockSize)
	opad := make([]byte, blockSize)
	for i := 0; i < blockSize; i++ {
		ipad[i] = key[i] ^ 0x36
		opad[i] = key[i] ^ 0x5c
	}
	inner := sha256.Sum256(append(ipad, msg...))
	outer := sha256.Sum256(append(opad, inner[:]...))
	return outer[:]
}

// callREST performs a single REST call, transparently re-logging in if the
// SID is invalid. apiPath should start with "/smarthome/...".
func (c *Client) callREST(ctx context.Context, apiPath string) ([]byte, error) {
	c.mu.Lock()
	if c.sid == "" || c.sid == invalidSID {
		if err := c.loginLocked(ctx); err != nil {
			c.mu.Unlock()
			return nil, err
		}
	}
	sid := c.sid
	c.mu.Unlock()

	body, status, err := c.doREST(ctx, sid, apiPath)
	// 401 / 403 indicate session expiry on the FRITZ!Box; 401 on fritzhome-cache.
	if err == nil && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		c.mu.Lock()
		if err := c.loginLocked(ctx); err != nil {
			c.mu.Unlock()
			return nil, err
		}
		sid = c.sid
		c.mu.Unlock()
		body, status, err = c.doREST(ctx, sid, apiPath)
	}
	if err != nil {
		return nil, err
	}
	if status/100 != 2 {
		return nil, fmt.Errorf("fritz: REST returned status %d (body=%q)", status, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func (c *Client) doREST(ctx context.Context, sid, apiPath string) ([]byte, int, error) {
	if !strings.HasPrefix(apiPath, "/") {
		apiPath = "/" + apiPath
	}
	u := c.BaseURL + c.BasePath + apiPath

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	// The Smart Home REST API's "AVM-SID" security scheme is an apiKey
	// carried in the Authorization header (not a query parameter, unlike
	// the legacy AHA-HTTP-Interface). See the official OpenAPI spec
	// (SmarthomeRestApiFRITZOS82.yaml, components.securitySchemes.AVM-SID:
	// "type: apiKey, in: header, name: Authorization") and
	// github.com/flunaras/fritzhome (FritzApi::buildRestRequest), which
	// both confirm this. Sending the SID as ?sid=... instead makes every
	// REST call fail even though the /login_sid.lua handshake succeeded.
	req.Header.Set("Authorization", "AVM-SID "+sid)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return body, resp.StatusCode, err
}

// Measurement is a snapshot of a single FRITZ!Smart Energy 250 (or 200/210).
// Values that the meter does not report are returned as 0/empty so callers
// can detect them. Units match the Shelly Pro 3EM EM.GetStatus payload.
//
// The Raw* fields preserve the wire-format integer values (mV/mA/mW/Wh)
// from the FRITZ! Smart Home REST API so safe-guards can compare samples
// byte-for-byte without float precision artifacts. A FRITZ!Smart Energy
// 250 that has not refreshed its DECT-ULE feed will repeat *the exact same
// integers* for many polls in a row — RawPower in particular is the most
// reliable signal that the meter is stuck.
type Measurement struct {
	Voltage     float64   // V (0 if not reported)
	Current     float64   // A  (computed from Power/Voltage if voltage is known)
	ActivePower float64   // W, signed: positive = import, negative = export
	Frequency   float64   // Hz (typically 50 in EU; meter may not report)
	Energy      float64   // Wh, cumulative since meter reset (total throughput)
	EnergyIn    float64   // Wh import, 0 if not reported separately
	EnergyOut   float64   // Wh export, 0 if not reported separately
	Updated     time.Time // when the measurement was sampled by us

	// Raw integer values exactly as returned by the REST API. These are
	// what the safe-guard layer compares byte-for-byte for stuck-data
	// detection. Units: mV, mA, mW, Wh.
	RawVoltage int64
	RawCurrent int64
	RawPower   int64
	RawEnergy  int64

	// State as reported by multimeterInterface. Anything other than
	// "valid" indicates the FRITZ!Box does not consider the reading
	// trustworthy and the proxy should treat the sample as unreliable.
	State string

	// UnitType as reported by the FRITZ!Box (e.g. "avmMeter",
	// "avmMeterFeedIn", "avmPlugSocket"). Used by ResolveUnitUID to
	// distinguish a general bidirectional grid reading from a
	// feed-in-only one; not otherwise consumed by the safeguard or the
	// Shelly emulation.
	UnitType string
}

// unitResponse is the trimmed-down structure of
// `GET /smarthome/overview/units/{UID}`. We only decode the fields that
// carry live measurement values; everything else in the JSON is ignored.
type unitResponse struct {
	UID        string `json:"UID"`
	AIN        string `json:"ain"`
	Name       string `json:"name"`
	UnitType   string `json:"unitType"`
	Interfaces struct {
		Multimeter *multimeterInterface `json:"multimeterInterface,omitempty"`
		Smartmeter *smartmeterInterface `json:"smartmeterInterface,omitempty"`
	} `json:"interfaces"`
}

// multimeterInterface is documented in the FRITZ! Smart Home REST API:
//
//	state:   "valid" | "invalid" | "unknown" | ...
//	voltage: integer millivolts (228055 -> 228.055 V)
//	current: integer milliamps (200 -> 0.2 A)
//	power:   integer milliwatts; on the FSE 250 this is SIGNED
//	         (negative = export to grid).
//	energy:  integer watt-hours (cumulative; bidirectional throughput on FSE 250).
type multimeterInterface struct {
	State   string `json:"state"`
	Voltage int64  `json:"voltage"`
	Current int64  `json:"current"`
	Power   int64  `json:"power"`
	Energy  int64  `json:"energy"`
}

// smartmeterInterface only carries status flags in the documented schema
// (no measurements). We keep the type around so future firmware that
// extends it for the FSE 250 (e.g. with energyImported/energyExported)
// can be picked up without rewiring callers.
type smartmeterInterface struct {
	State           string   `json:"state"`
	SmartmeterState []string `json:"smartmeterState"`
	// Optional fields some firmware revisions add; ignored if absent.
	EnergyImported int64 `json:"energyImported,omitempty"`
	EnergyExported int64 `json:"energyExported,omitempty"`
}

// GetMeasurement reads one unit's live values via the REST API.
//
// `unitUID` is the unit's `UID` field as reported by the FRITZ!Box (it
// usually equals the device AIN; for DECT-ULE units like the FSE 250 this
// is the 12-digit number on the device sticker, e.g. "16000 0036532-1").
// AVM's UIDs legitimately contain an internal space as part of the
// identifier itself (confirmed against a live FRITZ!Box and against
// github.com/flunaras/fritzhome's request builder, which percent-encodes
// the UID as-is via QUrl::toPercentEncoding instead of removing the
// space), so stripping it produces a UID the FRITZ!Box does not
// recognise and the REST call fails with "UID_NOT_FOUND". unitUID is
// passed through NormalizeAIN, which trims incidental leading/trailing
// whitespace and, for the standard 12-digit AIN shape, inserts the
// space back in if the caller omitted it (e.g. an operator who typed
// "116570123456" instead of the "11657 0123456" AVM prints on the
// sticker) -- both forms reach the FRITZ!Box as the identical,
// correctly-spaced UID. The (possibly reinserted) internal space is
// preserved and percent-encoded by url.PathEscape below.
func (c *Client) GetMeasurement(ctx context.Context, unitUID string) (*Measurement, error) {
	cleanUID := NormalizeAIN(unitUID)
	if cleanUID == "" {
		return nil, errors.New("fritz: empty unit UID")
	}
	path := "/smarthome/overview/units/" + url.PathEscape(cleanUID)

	body, err := c.callREST(ctx, path)
	if err != nil {
		return nil, err
	}
	var unit unitResponse
	if err := json.Unmarshal(body, &unit); err != nil {
		return nil, fmt.Errorf("fritz: parse unit response: %w (body=%q)", err, body)
	}
	if unit.Interfaces.Multimeter == nil {
		return nil, fmt.Errorf("fritz: unit %q has no multimeterInterface", cleanUID)
	}
	mm := unit.Interfaces.Multimeter

	m := &Measurement{
		Updated:    time.Now(),
		RawVoltage: mm.Voltage,
		RawCurrent: mm.Current,
		RawPower:   mm.Power,
		RawEnergy:  mm.Energy,
		State:      mm.State,
		UnitType:   unit.UnitType,
	}
	// mW -> W; the FSE 250 reports a signed value (negative on export).
	m.ActivePower = float64(mm.Power) / 1000.0
	// Wh cumulative.
	m.Energy = float64(mm.Energy)
	if mm.Voltage > 0 {
		m.Voltage = float64(mm.Voltage) / 1000.0
	}
	if mm.Current > 0 {
		// Prefer the FRITZ-reported current (also signed in firmware that
		// supports it; otherwise it is a magnitude only).
		m.Current = float64(mm.Current) / 1000.0
	} else if m.Voltage > 0 {
		// Derive a magnitude from |P|/U; better than nothing.
		m.Current = abs(m.ActivePower) / m.Voltage
	}

	// Optional fields some firmware revisions expose under smartmeterInterface.
	if sm := unit.Interfaces.Smartmeter; sm != nil {
		if sm.EnergyImported > 0 {
			m.EnergyIn = float64(sm.EnergyImported)
		}
		if sm.EnergyExported > 0 {
			m.EnergyOut = float64(sm.EnergyExported)
		}
	}
	return m, nil
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// DeviceInfo is the trimmed-down response of
// `GET /smarthome/overview/devices/{UID}`. Only the fields the startup
// suitability check inspects are retained.
type DeviceInfo struct {
	UID                string `json:"UID"`
	AIN                string `json:"ain"`
	Name               string `json:"name"`
	ProductName        string `json:"productName"`
	ProductCategory    string `json:"productCategory"`
	HardwareModelID    string `json:"hardwareModelId"`
	FirmwareVersion    string `json:"firmwareVersion"`
	Manufacturer       string `json:"manufacturer"`
	IsConnected        bool   `json:"isConnected"`
	LastConnectionTime int64  `json:"lastConnectionTime"`
	IsZigbeeDevice     bool   `json:"isZigbeeDevice"`
	// UnitUIDs lists every unit (child) UID belonging to this physical
	// device. A single-function device (e.g. most FSE 200/210 sockets)
	// has exactly one entry, usually equal to UID itself. Some FSE 250
	// firmware revisions instead expose two: a general "avmMeter" unit
	// and a feed-in-only "avmMeterFeedIn" unit. Used by ResolveUnitUID.
	UnitUIDs []string `json:"unitUids"`
}

// GetDevice fetches the device metadata for a given UID. This is meant
// for one-shot use at startup (or from the 'check' subcommand) to verify
// that the configured unit is on a supported product such as the
// FRITZ!Smart Energy 250; do not call it on every poll cycle.
//
// As with GetMeasurement, deviceUID is passed through NormalizeAIN: only
// incidental leading/trailing whitespace is trimmed, and the standard
// 12-digit AIN's internal space is preserved (or inserted, if the
// caller omitted it) and percent-encoded by url.PathEscape.
func (c *Client) GetDevice(ctx context.Context, deviceUID string) (*DeviceInfo, error) {
	clean := NormalizeAIN(deviceUID)
	if clean == "" {
		return nil, errors.New("fritz: empty device UID")
	}
	path := "/smarthome/overview/devices/" + url.PathEscape(clean)
	body, err := c.callREST(ctx, path)
	if err != nil {
		return nil, err
	}
	var dev DeviceInfo
	if err := json.Unmarshal(body, &dev); err != nil {
		return nil, fmt.Errorf("fritz: parse device response: %w (body=%q)", err, body)
	}
	return &dev, nil
}

// NormalizeAIN canonicalizes a FRITZ! Smart Home unit/device UID (AIN)
// so it can be entered either exactly as printed on the device sticker
// ("<5 digits> <7 digits>", e.g. "11657 0123456") or as a single run of
// digits with the space omitted ("116570123456"). Both forms occur in
// practice: the sticker shows the space, but it is easily lost when
// copy-pasting into a shell argument, an environment variable, or a
// systemd drop-in, or an operator simply doesn't type it. An optional
// trailing "-<digits>" sub-unit suffix (see ParentDeviceUID, e.g.
// "-1"/"-2") is recognised and preserved either way.
//
// Only the standard 12-digit AIN shape is recognised and reformatted;
// anything else (a fritzhome-cache alias, a short UID used in tests,
// ...) is returned with just the surrounding whitespace trimmed, i.e.
// unchanged from what plain strings.TrimSpace would have produced. It
// is therefore always safe to call on an arbitrary configured UID.
// GetMeasurement, GetDevice, ParentDeviceUID, and ResolveUnitUID all
// call this so the internal space the FRITZ!Box expects (see the
// GetMeasurement doc comment) is present on the wire no matter which
// way the operator wrote fritz-unit.
func NormalizeAIN(uid string) string {
	trimmed := strings.TrimSpace(uid)

	base, suffix := trimmed, ""
	if idx := strings.LastIndexByte(trimmed, '-'); idx > 0 && idx < len(trimmed)-1 {
		if isAllDigits(trimmed[idx+1:]) {
			base, suffix = trimmed[:idx], trimmed[idx:]
		}
	}

	digits := strings.ReplaceAll(base, " ", "")
	if len(digits) != 12 || !isAllDigits(digits) {
		return trimmed
	}
	return digits[:5] + " " + digits[5:] + suffix
}

// isAllDigits reports whether s is non-empty and consists entirely of
// ASCII digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ParentDeviceUID derives a physical device's UID from one of its unit
// UIDs, so callers that only have a unit UID (e.g. the single
// `fritz-unit` config value used for GetMeasurement) can still call
// GetDevice for the owning physical device.
//
// AVM's own convention for multi-function devices is
// "<deviceUID>-<n>" (confirmed both in the official OpenAPI examples,
// e.g. unitUids ["13077 0137031-1"] for device UID "13077 0137031", and
// live against a FRITZ!Smart Energy 250 whose firmware splits metering
// into a general "avmMeter" unit "<AIN>-1" and a feed-in-only
// "avmMeterFeedIn" unit "<AIN>-2"). Single-function devices such as most
// FSE 200/210 sockets instead report a bare unit UID identical to the
// device UID, with no suffix at all.
//
// unitUID is passed through NormalizeAIN first, so this also
// transparently handles an AIN entered without its internal space. The
// trailing "-<digits>" suffix, if present, is then stripped, which
// recovers the device UID in both cases; it is a no-op when unitUID has
// no such suffix. Without this, GetDevice("<AIN>-1") fails with
// "UID_NOT_FOUND" even though the unit and its measurements are read
// successfully via GetMeasurement, which silently defeats the
// RequireConnected safeguard (it never sees a non-nil DeviceInfo) and
// makes the startup product-allowlist check and the `check` subcommand
// fail for any such device.
func ParentDeviceUID(unitUID string) string {
	trimmed := NormalizeAIN(unitUID)
	idx := strings.LastIndexByte(trimmed, '-')
	if idx <= 0 || idx == len(trimmed)-1 {
		// No dash, dash at the very start (no device UID left of it), or
		// a trailing dash with nothing after it: nothing sensible to
		// strip, leave the value untouched.
		return trimmed
	}
	if !isAllDigits(trimmed[idx+1:]) {
		return trimmed
	}
	return trimmed[:idx]
}

// ResolveUnitUID figures out which unit UID actually has a usable
// multimeterInterface, given a single configured value that may be
// either an already-correct unit UID or a bare device UID (or a unit
// UID belonging to a device that splits metering across several
// sub-units).
//
// configuredUID is passed through NormalizeAIN before anything else, so
// an AIN entered without its internal space (e.g. "116570123456"
// instead of "11657 0123456") is canonicalized up front; both the fast
// path below and every candidate probed during discovery therefore use,
// and this function returns, the same correctly-spaced UID regardless
// of how the operator wrote fritz-unit.
//
// It first tries configuredUID directly via GetMeasurement: this is the
// common case (single-function devices, or an already-correct
// "<AIN>-1"-style value) and costs exactly one REST call, with no
// behaviour change from before ResolveUnitUID existed. Only if that
// fails does it fall back to full discovery: fetch the parent device
// (via ParentDeviceUID) and probe every unit in its UnitUIDs, preferring
// one whose UnitType is exactly "avmMeter" (the general bidirectional
// grid-connection reading confirmed live on an FSE 250) over any other
// unit that also has a usable multimeterInterface.
//
// Safety: a unit whose UnitType looks like a feed-in-only accounting
// channel (e.g. AVM's "avmMeterFeedIn") is never auto-selected, not even
// as a last resort, because it does not report grid import — using it
// as this proxy's measurement source would feed wrong-sign data to a
// power regulator such as Solakon ONE (see AGENTS.md, "Safe by
// default"). If every usable candidate looks feed-in-only, or if no
// candidate has a multimeterInterface at all, ResolveUnitUID returns an
// error rather than guessing; callers should surface that as a
// configuration problem, not paper over it.
func (c *Client) ResolveUnitUID(ctx context.Context, configuredUID string) (string, error) {
	configuredUID = NormalizeAIN(configuredUID)
	if configuredUID == "" {
		return "", errors.New("fritz: empty unit UID")
	}

	// Fast path: the configured value already names a usable unit.
	if _, err := c.GetMeasurement(ctx, configuredUID); err == nil {
		return configuredUID, nil
	}

	deviceUID := ParentDeviceUID(configuredUID)
	dev, err := c.GetDevice(ctx, deviceUID)
	if err != nil {
		return "", fmt.Errorf("fritz: unit %q has no usable measurement, and its parent device %q could not be fetched to look for one: %w",
			configuredUID, deviceUID, err)
	}
	if len(dev.UnitUIDs) == 0 {
		return "", fmt.Errorf("fritz: device %q (resolved from unit %q) reports no units at all", deviceUID, configuredUID)
	}

	var generalUnits, otherUnits, feedInUnits, tried []string
	for _, uid := range dev.UnitUIDs {
		m, mErr := c.GetMeasurement(ctx, uid)
		if mErr != nil {
			tried = append(tried, fmt.Sprintf("%s (%s)", uid, mErr))
			continue
		}
		switch {
		case m.UnitType == "avmMeter":
			generalUnits = append(generalUnits, uid)
		case looksFeedInOnly(m.UnitType):
			feedInUnits = append(feedInUnits, uid)
		default:
			otherUnits = append(otherUnits, uid)
		}
	}
	if len(generalUnits) > 0 {
		return generalUnits[0], nil
	}
	if len(otherUnits) > 0 {
		return otherUnits[0], nil
	}
	if len(feedInUnits) > 0 {
		return "", fmt.Errorf("fritz: device %q only exposes feed-in-only measurement unit(s) %v; "+
			"refusing to auto-select one because it does not report grid import and would feed wrong-sign "+
			"data to a power regulator -- set fritz-unit to the correct unit UID explicitly",
			deviceUID, feedInUnits)
	}
	return "", fmt.Errorf("fritz: no unit of device %q has a usable multimeterInterface (tried: %v)", deviceUID, tried)
}

// looksFeedInOnly reports whether a unitType string looks like a
// feed-in-only (export-only) accounting channel, e.g. AVM's
// "avmMeterFeedIn". Matched case-insensitively and by substring so
// unanticipated variants of the same concept are still caught instead
// of silently trusted.
func looksFeedInOnly(unitType string) bool {
	return strings.Contains(strings.ToLower(unitType), "feedin")
}
