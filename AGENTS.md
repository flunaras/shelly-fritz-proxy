# AGENTS.md — shelly-fritz-proxy

## Project Overview

`shelly-fritz-proxy` is a **single-binary Go service** that pretends to be a
**Shelly Pro 3 EM** on the local network and feeds it with live measurements
polled from a **FRITZ!Smart Energy 250** connected to a FRITZ!Box. It exists
so consumers that only speak the Shelly Pro 3 EM API — most prominently the
**Solakon ONE** inverter — can use a FRITZ! bidirectional energy meter as
their grid sensor.

Key constraints:

- **Single binary, single Go module, no plugins.** All functionality lives
  in one `cmd/shelly-fritz-proxy` binary; subcommands branch from a single
  entry point.
- **Stateless across restarts.** No on-disk state, no database, no cache
  files. The only persistent inputs are the config file and environment
  variables; everything else is rebuilt from upstream on each cycle.
- **Safe by default.** Every safeguard knob ships with a conservative
  default value; the stale-data fail-safe policy is `error` (not `freeze`,
  not `safe_import`, **never** `zero`) so a downstream Solakon ONE pauses
  regulation rather than acts on wrong-sign data.
- **Same build, packaging, systemd and config conventions as the sibling
  projects** [`fritzhome-cache`](https://github.com/flunaras/fritzhome-cache)
  and [`solakon-one-fritz-powerregulator`](https://github.com/flunaras/solakon-one-fritz-powerregulator)
  so a shared CI matrix and a single ops runbook can drive all three.

---

## Repository Layout

```
shelly-fritz-proxy/
├── go.mod                              — Go module manifest
├── VERSION                             — semver string read by docker/build.sh
├── README.md                           — user-facing documentation
├── AGENTS.md                           — this file
├── LICENSE                             — license text (TODO)
├── .gitignore                          — also excludes build/ and out/
│
├── cmd/shelly-fritz-proxy/             — entry point and CLI
│   ├── main.go                         — flag/env/config parsing, subcommand dispatch
│   └── main_test.go                    — config precedence and flag tests
│
├── internal/                           — all implementation packages
│   ├── fritz/                          — FRITZ! Smart Home REST API client
│   │   ├── client.go                   — login, GetDevice, GetMeasurement
│   │   └── client_test.go              — challenge-response + REST parsing
│   ├── iniconf/                        — minimal `key = value` INI loader
│   │   ├── iniconf.go
│   │   └── iniconf_test.go
│   ├── safeguard/                      — stale/stuck/glitch detection state machine
│   │   ├── safeguard.go
│   │   └── safeguard_test.go
│   ├── meter/                          — shared in-memory state driven by the poller
│   │   ├── store.go                    — Store: phase mapping + guard wiring + forecast-mode hook
│   │   └── testing.go                  — Apply() helper for sibling-package tests
│   ├── shelly/                         — Gen2 RPC HTTP server emulating SPEM-003CEBEU
│   │   ├── server.go                   — /shelly, /rpc, /rpc/<Method>, stale policies
│   │   ├── server_test.go              — discovery + EM.GetStatus + stale-policy tests
│   │   └── testing_helpers_test.go     — sibling-package test shims
│   ├── shellyclient/                   — Shelly Gen2 RPC HTTP client, used by the `query` subcommand
│   │   └── client.go
│   ├── forecast/                       — forecast-mode compensation engine (see below)
│   │   ├── forecast.go
│   │   └── forecast_test.go
│   ├── battery/                        — pluggable solar-battery Reader interface + type registry
│   │   ├── battery.go
│   │   ├── battery_test.go
│   │   └── solakon/                    — Reader implementation for the FoxESS Solakon ONE (Modbus TCP)
│   │       ├── solakon.go
│   │       └── solakon_test.go
│   ├── modbus/                         — minimal Modbus TCP client (FC 0x03 Read Holding Registers)
│   │   ├── client.go
│   │   └── client_test.go
│   └── mdns/                           — hand-rolled mDNS responder for Bonjour discovery
│       └── responder.go
│
├── data/                               — package payload (installed under /etc and /usr)
│   ├── shelly-fritz-proxy.conf         — fully commented INI default
│   ├── shelly-fritz-proxy.service      — systemd unit, DynamicUser, hardening, CAP_NET_BIND_SERVICE
│   ├── nfpm.yaml                       — single packaging manifest (DEB + RPM)
│   └── scripts/                        — DEB/RPM lifecycle scripts
│       ├── postinstall.sh
│       ├── preremove.sh
│       └── postremove.sh
│
├── docker/                             — distro-targeted build scripts
│   ├── Dockerfile.builder              — one image (golang + nfpm) for all targets
│   └── build.sh                        — same --distro alias set as fritzhome-cache
│
├── docs/                               — design documentation
│   └── architecture.md                 — long-form architecture (read before changing data flow)
│
└── Dockerfile                          — runtime container for users who prefer `docker run`
```

---

## What this tool does (and does not)

### Does

- Logs into a FRITZ!Box via `/login_sid.lua?version=2` (PBKDF2 modern, MD5
  fallback).
- Polls **one** Smart Home unit (default 2 s interval) via the documented
  REST API: `GET /api/v0/smarthome/overview/units/{UID}`.
- Runs every sample through a **safeguard state machine** that classifies
  it as Accept / Reject / Stale based on freshness, byte-equality (stuck
  cache), plausibility bounds, and step-change magnitude.
- Maps the single-channel FRITZ measurement onto L1/L2/L3 according to a
  configurable `phase-mode` (split | a).
- Serves a Shelly Gen2 RPC HTTP API on `:80` that consumers like Solakon
  ONE poll on every cycle (`EM.GetStatus`, `EMData.GetStatus`,
  `Shelly.GetDeviceInfo`, `Shelly.GetStatus`, `Shelly.GetConfig`,
  `Sys.GetStatus`, ...).
- When the safeguard says "stale", applies a configurable fail-safe
  policy to the response: `error` (default), `freeze`, `safe_import`, or
  `zero` (documented as dangerous).
- Announces itself via mDNS as `shellypro3em-<mac>._http._tcp.local` and
  `_shelly._tcp.local` so Solakon ONE discovers it on the LAN.
- Verifies the configured unit is on a permitted product (default:
  `FRITZ!Smart Energy 250`) before going live; refuses to start
  otherwise.

### Does not

- **No outbound websocket / Shelly Cloud integration.** Solakon ONE does
  not need it.
- **No Modbus emulation.** Real Shelly Pro 3EM exposes Modbus TCP/502
  optionally; most consumers use RPC. Add it later if a client actually
  needs it.
- **No write endpoints.** Every mutating RPC method returns
  `-32601 Method not Found`. The proxy is read-only by design.
- **No control loop.** Reading is decoupled from any inverter command
  pipeline; that responsibility belongs to the downstream consumer
  (Solakon ONE) or to a sibling tool like
  [`solakon-one-fritz-powerregulator`](https://github.com/flunaras/solakon-one-fritz-powerregulator).

---

## FRITZ! Smart Home REST API — Quick Reference

Cross-checked against the official AVM specification (FRITZ!OS 8.20,
`SmarthomeRestApiFRITZOS82.html`) and against the
`flunaras/fritzhome-cache` project's reverse-engineered notes.

### Authentication

| Step | Method | URL | Notes |
|---|---|---|---|
| 1 | GET | `/login_sid.lua?version=2` | Returns XML `<Challenge>` and current `<SID>` |
| 2 | GET | `/login_sid.lua?version=2&username=…&response=…` | Returns XML with issued `<SID>` |

Challenge variants:
- **PBKDF2** (`"2$iter1$salt1$iter2$salt2"`) — two-pass PBKDF2-HMAC-SHA256.
- **MD5** (legacy) — MD5 of `"<challenge>-<password>"` encoded as UTF-16LE.

Both are implemented inline in `internal/fritz/client.go` so the project
depends only on `golang.org/x/net/dns/dnsmessage` (for mDNS) and the Go
standard library — **no `golang.org/x/crypto`**.

SID is passed as an `Authorization: AVM-SID <sid>` header on every REST
call — this is what the Smart Home REST API's `AVM-SID` security scheme
(`type: apiKey, in: header, name: Authorization`) actually requires,
unlike the legacy AHA-HTTP-Interface's `?sid=...` query parameter. On
HTTP 401/403 the client transparently re-logs in once and retries.

### REST endpoints used by this proxy

| Method | Path | Frequency | Used for |
|---|---|---|---|
| GET | `/api/v0/smarthome/overview/devices/{UID}` | startup + every ~30 polls | product allowlist check, `isConnected` |
| GET | `/api/v0/smarthome/overview/units/{UID}` | every `poll` cycle | `multimeterInterface` live values |

The unit-UID endpoint is the one
[`fritzhome-cache`](https://github.com/flunaras/fritzhome-cache) caches.
Pointing `--fritz-url` at a `fritzhome-cache` instance is the supported,
recommended setup when more than one consumer polls the same FSE 250.

### `multimeterInterface` field semantics

The REST API returns the FSE 250's live values as integers in the wire
units below. The fritz client stores them both as float (for display)
and as raw integers (`RawVoltage`, `RawCurrent`, `RawPower`, `RawEnergy`
on `fritz.Measurement`) so the stuck-data safeguard can compare samples
byte-for-byte without float-precision artifacts.

| Field | Wire unit | Sign on FSE 250 | Notes |
|---|---|---|---|
| `state` | string | — | `"valid"` when trustworthy |
| `voltage` | mV | always ≥ 0 | grid voltage at the meter |
| `current` | mA | magnitude | RMS; not signed in current firmware |
| `power` | mW | **signed** | + import / − export |
| `energy` | Wh | always ≥ 0 | cumulative bidirectional throughput |

The official schema documents only one cumulative `energy` counter.
Some firmware revisions expose `energyImported` / `energyExported`
under `smartmeterInterface`; the fritz client picks them up when present
but does not depend on them.

---

## Architecture

```
   ┌───────────────────────────────────────────────────────────────────┐
   │                          shelly-fritz-proxy                       │
   │                                                                   │
   │   main()                                                          │
   │     │                                                             │
   │     ├─ parseConfig()  (CLI > env > INI file > builtin defaults)   │
   │     ├─ subcommand "check"  → runCheck()  →  exit 0/1              │
   │     └─ default subcommand  → runDaemon()                          │
   │           │                                                       │
   │           ├─ startup product-allowlist check (refuse if mismatch) │
   │           │                                                       │
   │           ├─ goroutine: meter.Store.Run()                         │
   │           │     loop every `poll`:                                │
   │           │       1. fritz.Client.GetMeasurement(unit)            │
   │           │       2. safeguard.Guard.Evaluate(sample, deviceInfo) │
   │           │       3. on Accept: apply phase mapping, update State │
   │           │          on Reject/Stale: update health only          │
   │           │                                                       │
   │           ├─ goroutine: http.Server (port 80 by default)          │
   │           │     handlers:                                         │
   │           │       /shelly             → Shelly.GetDeviceInfo      │
   │           │       /rpc, /rpc/<method> → dispatch table            │
   │           │       /healthz            → plain text (k8s probe)    │
   │           │       /healthz/details    → JSON diagnostics          │
   │           │                                                       │
   │           └─ goroutine: mdns.Responder.Run()                      │
   │                 answers _shelly._tcp.local / _http._tcp.local     │
   └───────────────────────────────────────────────────────────────────┘
            │                              │                          │
            ▼                              ▼                          ▼
     FRITZ!Box (or             Solakon ONE / EVCC /             Solakon discovery
     fritzhome-cache)          openWB / Home Assistant          via mDNS browse
     (REST API)                (HTTP polls)
```

The full long-form version with sequence diagrams, register maps,
safeguard state machine, and deployment layout is in
[`docs/architecture.md`](docs/architecture.md).

---

## Components

### 1. `fritz` package (`internal/fritz/`)

Thin REST client. Public surface:

```go
type Client struct { ... }

func NewClient(baseURL, user, password string) *Client
func (c *Client) SetBasePath(p string)
func (c *Client) Login(ctx context.Context) error
func (c *Client) GetDevice     (ctx context.Context, deviceUID string) (*DeviceInfo, error)
func (c *Client) GetMeasurement(ctx context.Context, unitUID   string) (*Measurement, error)
func (c *Client) ResolveUnitUID(ctx context.Context, configuredUID string) (string, error)
func ParentDeviceUID(unitUID string) string
func NormalizeAIN(uid string) string
```

- Login challenge/response (PBKDF2 + MD5) is inlined; no `x/crypto`.
- `Measurement` carries both float and raw-integer fields so the
  safeguard's byte-equality detector is precise.
- TLS verification of the FRITZ!Box is disabled by default
  (`InsecureSkipVerify: true`) because most FRITZ!Boxes ship a
  self-signed cert. Operators that front their box with a real cert can
  replace `Client.http` after construction.
- `NormalizeAIN` canonicalizes AVM's AIN/unit UID shape (`"<5 digits>
  <7 digits>"`, optionally `-<n>` suffixed) so `fritz-unit` can be
  entered either exactly as printed on the device sticker (with its
  internal space, e.g. `"11657 0123456"`) or as a bare run of digits
  with the space omitted (`"116570123456"`) — both reach the
  FRITZ!Box as the identical, correctly-spaced UID. The space is not
  cosmetic: a live FRITZ!Box returns `UID_NOT_FOUND` for the same
  digits with it stripped. Anything that isn't the standard 12-digit
  shape is returned trimmed but otherwise unchanged, so it is safe to
  call on any UID. `GetMeasurement`, `GetDevice`, `ParentDeviceUID`,
  and `ResolveUnitUID` all normalize their UID argument through this
  function, and `main.go`'s `run()` additionally normalizes
  `cfg.FritzUnitUID` once right after parsing so logs and the
  auto-resolved-unit comparison (see `ResolveUnitUID` below) see the
  canonical form too.
- `ParentDeviceUID` strips one trailing `-<digits>` suffix from a unit
  UID to recover its owning device's UID. Needed because the single
  `fritz-unit` config value is a *unit* UID (for `GetMeasurement`), but
  some FSE 250 firmware revisions expose it as `<AIN>-1` (general
  "avmMeter") or `<AIN>-2` ("avmMeterFeedIn") rather than a bare `<AIN>`
  equal to the *device* UID `GetDevice` needs. `main.go` and
  `meter.Store.refreshDevice` both call it before calling `GetDevice`.
- `ResolveUnitUID` makes the `<AIN>-1`/`<AIN>-2` split above transparent
  to the operator: given the configured `fritz-unit` value, it first
  tries `GetMeasurement` directly (one REST call, no behaviour change
  for devices where that already works), and only on failure falls back
  to fetching the parent device's `UnitUIDs` and probing each one,
  preferring `UnitType == "avmMeter"` over anything that looks
  feed-in-only (substring-matched, case-insensitive, e.g.
  `avmMeterFeedIn`). It deliberately refuses (returns an error) rather
  than ever auto-selecting a feed-in-only unit, since that would feed
  wrong-sign data to a power regulator. Both `runCheck` and `runDaemon`
  in `main.go` call it before `GetMeasurement`; `runDaemon` treats a
  resolution failure as best-effort (WARN + fall back to the raw
  configured value) to match the allowlist check's "briefly
  unreachable" tolerance, while `runCheck` treats it as a normal fatal
  `check failed:` error, consistent with `check` being the strict,
  installer-facing subcommand.

### 2. `iniconf` package (`internal/iniconf/`)

Minimal, dependency-free `key = value` parser. Supports `#` and `;`
comments, quoted strings, and trailing comments. Returns
`os.IsNotExist`-detectable errors so callers can distinguish "no file"
from "malformed file".

### 3. `safeguard` package (`internal/safeguard/`)

The runtime safety state machine. Detailed coverage is in
[`docs/architecture.md`](docs/architecture.md#safeguard-state-machine).
Public surface:

```go
type Config struct { MaxAge, StuckCycles, MaxStep, ... }
func DefaultConfig() Config

type Guard struct { ... }
func NewGuard(cfg Config) *Guard

type Verdict int      // Accept | Reject | Stale
type Health  int      // HealthInit | HealthOk | HealthDegraded | HealthStale
type Reason  string   // stable strings for logs and Shelly errors[]

func (g *Guard) Evaluate       (now time.Time, m *fritz.Measurement, dev *fritz.DeviceInfo) EvaluateResult
func (g *Guard) NotePollFailure(now time.Time)                                              EvaluateResult
func (g *Guard) Health() Health
func (g *Guard) LastAccepted() *fritz.Measurement
```

Defaults (set via `DefaultConfig()`): `MaxAge=30s`, `StuckCycles=5`,
`StuckMatchVoltage=true`, `MinVoltage=180`, `MaxVoltage=280`,
`MaxAbsPower=30 kW`, `MaxStep=15 kW`, `RecoverySamples=3`,
`RequireConnected=true`, `AcceptedStates=[valid]`.

**Reasons** (stable strings, flow through to Shelly `errors[]`):

```
poll_failure  interface_state_invalid  device_offline
out_of_range_voltage  out_of_range_power  step_too_large
stuck_data  freshness_timeout
```

### 4. `meter` package (`internal/meter/`)

Shared, thread-safe in-memory state. Owns:

- the polling goroutine (`Store.Run`),
- the `safeguard.Guard`,
- the per-poll `apply` that maps the FSE 250's single channel onto
  L1/L2/L3 according to `PhaseMode`,
- the last `DeviceInfo` (refreshed lazily every ~30 polls so
  `RequireConnected` stays accurate),
- the `HealthSnapshot` consumed by the Shelly server and `/healthz`.

```go
type PhaseMode int          // PhaseSplit (default) | PhaseA
type State struct {...}     // copied out under RLock
type HealthSnapshot struct {...}

func NewStore(c *fritz.Client, unitUID string, mode PhaseMode,
              period time.Duration, guard *safeguard.Guard,
              logger *slog.Logger) *Store
func (s *Store) SetProductOk(ok bool)
func (s *Store) Run(ctx context.Context)
func (s *Store) Snapshot() (State, HealthSnapshot, bool)
```

### 5. `shelly` package (`internal/shelly/`)

The Gen2 RPC HTTP server. Models the read-only endpoints of a real
**Shelly Pro 3 EM** (`SPEM-003CEBEU`, `gen=2`, `profile=triphase`).

```go
type StalePolicy int            // StalePolicyError (default) | Freeze | SafeImport | Zero
type StaleConfig struct { Policy StalePolicy; SafeImportWatts float64 }

type Server struct { ... }
func New(store *meter.Store, mac string, staleCfg StaleConfig,
         logger *slog.Logger) *Server
func (s *Server) Register(mux *http.ServeMux)
```

Implemented methods:

```
Shelly.GetDeviceInfo   Shelly.GetStatus      Shelly.GetConfig
Shelly.ListMethods     Shelly.Reboot         (no-op)
Sys.GetStatus          Sys.GetConfig
WiFi.GetStatus         WiFi.GetConfig
EM.GetStatus           EM.GetConfig          EM.GetCTTypes
EMData.GetStatus       EMData.GetConfig
Eth.GetStatus          Eth.GetConfig
Cloud.GetStatus        Cloud.GetConfig       (stub, always disabled)
MQTT.GetStatus         MQTT.GetConfig        (stub, always disabled)
BLE.GetStatus          BLE.GetConfig         (stub, always disabled)
```

Any other method returns JSON-RPC error code `-32601`.

**Stale-policy mapping** (applied to `EM.GetStatus` and
`EMData.GetStatus` when `Snapshot().Health != HealthOk`):

| Policy | Measurement fields | `errors[]` |
|---|---|---|
| `error` (default) | all `null` | `["stale_data", <reason>]` |
| `freeze` | last accepted values, unchanged | `["stale_data", <reason>]` |
| `safe_import` | synthesized positive grid-import at `SafeImportWatts` | `["stale_data", <reason>]` |
| `zero` | `0` everywhere, no `errors[]` flag (intentionally invisible) | empty |

### 6. `mdns` package (`internal/mdns/`)

Hand-rolled multicast DNS responder. Answers queries for
`_shelly._tcp.local` and `_http._tcp.local`; publishes A, PTR, SRV and
TXT records for `shellypro3em-<mac>.local`. Uses
`golang.org/x/net/dns/dnsmessage` and nothing else.

If you ever hit edge cases (multi-interface hosts, IPv6, fragmented
queries), swap it out for `github.com/grandcat/zeroconf` and delete this
package; it is not on any hot path.

### 7. `shellyclient` package (`internal/shellyclient/`)

A minimal HTTP client for the Shelly Gen2 RPC API exposed by
`internal/shelly.Server` (and, since the wire format is the same, by a
real Shelly Pro 3 EM). Backs the `query` subcommand. Public surface:

```go
type Client struct { BaseURL string; HTTP *http.Client }

func New(baseURL string, httpClient *http.Client) *Client
func (c *Client) Call(ctx context.Context, method string, params any, out any) error
func (c *Client) CallRaw(ctx context.Context, method string, params url.Values) (json.RawMessage, error)
func (c *Client) Discover(ctx context.Context) (*DeviceDiscovery, error)
func (c *Client) GetDeviceInfo(ctx context.Context) (*DeviceInfo, error)
func (c *Client) GetEMStatus(ctx context.Context, id int) (*EMStatus, error)
func (c *Client) GetEMDataStatus(ctx context.Context, id int) (*EMDataStatus, error)
func (c *Client) GetSysStatus(ctx context.Context) (*SysStatus, error)
func (c *Client) GetHealthDetails(ctx context.Context) (*HealthDetails, error)
```

`Call`/`CallRaw` decode the JSON-RPC envelope and surface protocol
errors as `*RPCError` (mirrors the `{"code":...,"message":...}` shape
`internal/shelly` returns for e.g. `-32601 Method not Found`). Read-only
by convention: it only ever issues `Get*` methods, though nothing stops
a caller from passing a mutating method name to `Call` — the server on
the other end rejects those anyway.

### 8. Forecast mode: `forecast`, `battery`, `battery/solakon`, `modbus`

Together these implement "forecast mode" (`--forecast-mode`): a
compensation layer for consumers (Solakon ONE included) that poll this
proxy much faster than the FRITZ!Box refreshes its cached measurement,
which would otherwise let the consumer mistake a stale repeated
reading for a genuine new one and escalate its own output without
bound. See the README's "Forecast mode" section for the full worked
example this design is built to match exactly.

**`forecast` package (`internal/forecast/`)** — the compensation engine
itself. Stateful, keyed off the FRITZ! sample's raw (byte-comparable)
power value so a genuinely-unchanged household load is never confused
with a stuck FRITZ! cache and vice versa:

```go
type Engine struct { ... }
func New() *Engine
func (e *Engine) Adjust(rawFritz int64, fritzWatts, batterySum float64) float64
func (e *Engine) Reset()
```

`Adjust` returns `fritzWatts` unchanged whenever `rawFritz` differs
from the last call (a genuine new sample), remembering both the
reading and `batterySum` as the new baseline. Otherwise it returns
`lastFritzWatts - batterySum + lastBatteryWatts`. `Reset` is called by
`meter.Store` whenever the safeguard rejects a sample or a poll fails,
so a stale/irrelevant baseline is never compared against once real
data resumes.

**`battery` package (`internal/battery/`)** — the pluggable interface
and a type-name registry, so new solar-battery backends can be added
without touching call sites:

```go
type Reader interface {
    ReadPowerWatts(ctx context.Context) (float64, error)
    Close() error
}
type DeviceConfig struct { Type, Host string; Port, UnitID int; Timeout time.Duration }
type Factory func(cfg DeviceConfig) (Reader, error)

func Register(typeName string, f Factory) // called from each impl's init()
func New(cfg DeviceConfig) (Reader, error)
```

Sign convention: positive = exporting/injecting power toward the
household or grid, matching `fritz.Measurement.ActivePower`'s own
sign — this matters because forecast mode sums every configured
device's output together.

**`battery/solakon` package (`internal/battery/solakon/`)** — the only
implementation currently shipped, for a **FoxESS Solakon ONE**. Reads
register `39134` (`ACTIVE_POWER`: the inverter's current grid export
power, +export/-import) over Modbus TCP. Register map and wire format
(int32 spanning 2 holding registers, FoxESS big-endian *word* order —
high word first — raw value already in watts, no scaling) were
cross-checked against the sibling project
[`solakon-one-fritz-powerregulator`](https://github.com/flunaras/solakon-one-fritz-powerregulator)'s
own C++ Modbus client (`src/solakonapi.cpp/.h` on its `feature/initial`
branch) rather than guessed, since a wrong register/scale would
silently feed garbage into forecast mode's control loop. Registers are
used exactly as documented by FoxESS: no SunSpec-style base offset.
Defaults: port `502`, unit/slave ID `1` (matching that sibling
project's own `config.h` defaults).

**`modbus` package (`internal/modbus/`)** — the minimal Modbus TCP
client `battery/solakon` is built on. Implements only what's needed:
MBAP framing plus function code `0x03` (Read Holding Registers).
Opens a fresh TCP connection per call rather than holding one open;
this proxy polls at most once every few seconds, so the extra
handshake latency is immaterial and it sidesteps having to reason
about a long-lived connection's staleness or partial-write recovery.

```go
type Client struct { Addr string; UnitID byte; Timeout time.Duration }
func New(addr string, unitID byte, timeout time.Duration) *Client
func (c *Client) ReadHoldingRegisters(addr uint16, quantity uint16) ([]byte, error)
func DecodeInt32BigWordFirst(data []byte) (int32, error)
func DecodeUint16(data []byte) (uint16, error)
```

### 9. `cmd/shelly-fritz-proxy` (entry point)

`main.go` is the only Go file with side effects (signal handling, real
HTTP listener, real flag parsing). Owns:

- `parseConfig` — three-pass: extract `--config` from argv, load INI,
  register the real flag set with file/env values as defaults, then
  `Parse(args)`. Precedence is **CLI > env > config file > builtin
  default**.
- `runDaemon` and `runCheck` subcommands.
- `runQuery` — the `query` subcommand, with its own independent flag
  set (deliberately outside `parseConfig`/`config`, since it needs
  none of the FRITZ! credentials or safeguard knobs and should stay
  usable even against a proxy instance running on another host).
- `parseBatteryDevices` — parses `--battery-devices` into
  `[]battery.DeviceConfig`; wired into `runDaemon` when
  `--forecast-mode` is set, and reused as-is by `runCheckBattery`.
  Blank-imports `internal/battery/solakon` so the `"solakon"` type is
  registered.
- `runCheckBattery` — the `check-battery` subcommand: builds a
  `battery.Reader` for every configured device via `battery.New` and
  reads it once, printing one line per device (`batteryDeviceLabel`
  formats the identifier). Dispatched in `run()` *before* the FRITZ!
  credential checks, since it needs neither `fritz-unit` nor
  `fritz-password` -- unlike `check`/the daemon, it goes through the
  normal `parseConfig` precedence rather than having its own
  independent flag set (there is no reason to duplicate
  `battery-devices`/`battery-timeout` parsing a second time the way
  `runQuery` duplicates the Shelly-target flags).
- `makeHealthHandler` / `makeHealthDetailsHandler`.
- `productAllowed` / `splitAllowlist`.

---

## Configuration

Three sources, in increasing order of precedence:

1. **INI config file** at `/etc/shelly-fritz-proxy/shelly-fritz-proxy.conf`
   (or `--config <path>`). Same key names as the CLI flags without the
   leading `--`.
2. **Environment variables** (uppercased, hyphens to underscores, e.g.
   `FRITZ_URL`, `STALE_POLICY`, `STUCK_CYCLES`).
3. **CLI flags**.

The full key/env/default table is maintained in
[`README.md`](README.md#configuration-via-environment) and in the
inline help (`shelly-fritz-proxy --help`).

A bool field that is "off by default" in the schema does not need an
env var override to disable it — leave the env unset, set the file value
to `false`, or pass `--<flag>=false`. The default is reflected in the
flag registration; missing CLI / missing env / missing file all collapse
to the same builtin default.

### Subcommands

| Subcommand | Purpose | Exit code |
|---|---|---|
| (none)     | Run the daemon (default) | runs until SIGTERM/SIGINT |
| `check`    | Verify the FRITZ side end-to-end: log in, fetch the unit's device metadata, match the product allowlist, then read one measurement and print a summary | 0 = OK, 1 = first failing check |
| `query`    | Query the Shelly Gen2 RPC API of a running proxy instance (or a real Shelly Pro 3 EM) for ad-hoc diagnostics; has its own independent flag set (`--target`, `--method`, `--id`, `--discover`, `--health`, `--timeout`) and does not touch FRITZ! credentials or config | 0 = OK, 1 = request/decode error |
| `check-battery` | Read the current power output of every device configured via `--battery-devices` once and print it; a debugging aid for forecast-mode setup. Uses the normal `parseConfig` precedence (CLI > env > file > default) but never touches FRITZ! credentials or starts the daemon | 0 = OK, 1 = a device failed to construct or read |

---

## systemd unit

Installed at `/usr/lib/systemd/system/shelly-fritz-proxy.service`.
Key design choices:

- **`DynamicUser=yes`** — systemd allocates a transient unprivileged
  UID/GID at start; no pre-created service account.
- **`AmbientCapabilities=CAP_NET_BIND_SERVICE`** plus matching
  `CapabilityBoundingSet` — the only capability needed to bind port 80
  without being root. mDNS does not need extra caps because
  `224.0.0.251:5353` is unprivileged.
- Heavy sandboxing on by default: `ProtectSystem=strict`,
  `ProtectHome=yes`, `PrivateTmp=yes`, `PrivateDevices=yes`,
  `ProtectKernelTunables`, `ProtectKernelModules`,
  `ProtectControlGroups`, `RestrictNamespaces`, `RestrictRealtime`,
  `RestrictSUIDSGID`, `LockPersonality`, `MemoryDenyWriteExecute`,
  `SystemCallArchitectures=native`,
  `SystemCallFilter=@system-service`, `~@privileged @resources`.
- `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6`.
- Reads the INI file via `--config /etc/.../shelly-fritz-proxy.conf`.

To override individual settings without touching the file, use
`systemctl edit shelly-fritz-proxy` (drop-in).

---

## Build System

### Native build (host toolchain)

```bash
go build -o shelly-fritz-proxy ./cmd/shelly-fritz-proxy
```

Requires Go 1.22 or newer. The single external dependency is
`golang.org/x/net` (for `dns/dnsmessage`).

### Docker build (all distro packages)

```bash
./docker/build.sh                       # all six distro aliases
./docker/build.sh --distro debian-12-x86_64
./docker/build.sh --distro raspbian-bookworm-armhf
```

**One** Docker image (`docker/Dockerfile.builder`) cross-compiles every
target — this is the deliberate departure from `fritzhome-cache`'s
six-image setup. Justification: Go cross-compiles natively (`GOOS=linux
GOARCH=...`), produces fully static ELFs (`CGO_ENABLED=0`), and `nfpm`
emits both `.deb` and `.rpm` from one manifest. The per-distro
complexity that `fritzhome-cache` needs only exists because C++ has to
match distro-specific glibc / OpenSSL ABIs.

**Alias / output mapping** (kept identical to `fritzhome-cache` so the
same CI matrix can drive both projects):

| Alias | GOOS | GOARCH | GOARM | Family | Format | Path |
|---|---|---|---|---|---|---|
| `opensuse-tumbleweed-x86_64`   | linux | amd64 | —  | opensuse | rpm | `out/opensuse/tumbleweed/x86_64/`   |
| `opensuse-tumbleweed-aarch64`  | linux | arm64 | —  | opensuse | rpm | `out/opensuse/tumbleweed/aarch64/`  |
| `debian-12-x86_64`             | linux | amd64 | —  | debian   | deb | `out/debian/12/amd64/`              |
| `raspbian-bookworm-aarch64`    | linux | arm64 | —  | raspbian | deb | `out/raspbian/bookworm/arm64/`      |
| `raspbian-bookworm-armhf`      | linux | arm   | 7  | raspbian | deb | `out/raspbian/bookworm/armhf/`      |
| `raspbian-bullseye-armhf`      | linux | arm   | 7  | raspbian | deb | `out/raspbian/bullseye/armhf/`      |

`nfpm` packaging: `data/nfpm.yaml` is rendered through a small `sed`
pipeline in `build.sh` (envsubst-style) and fed to
`nfpm pkg --packager deb|rpm`. The same manifest produces both formats.

---

## Coding Style

- **Standard library first.** Adding a dependency requires a strong
  justification in the PR description. Only `golang.org/x/net` is
  currently imported beyond the stdlib.
- **`internal/` for everything except `cmd/`.** No public Go API
  surface; this is an application, not a library. Promote a package
  out of `internal/` only when it is genuinely intended for reuse.
- **`log/slog` for all logging.** No `fmt.Printf` to stdout/stderr from
  inside library packages; the entry point owns the logger and threads
  it via `logger.With("component", "<name>")`.
- **Errors return values, never panic.** Exception: `main()` calls
  `os.Exit(1)` on a fatal startup error, which is the only acceptable
  abnormal termination path.
- **No global state in `internal/` packages.** All goroutine-shared
  state lives inside a `Store` / `Guard` / `Server` value. The one
  exception (`deviceRefreshCounter` in `meter/store.go`) is documented
  with a comment.
- **Concurrency:** every package that mutates shared state uses
  `sync.RWMutex` plus, for read-heavy paths, a `sync/atomic` snapshot
  flag (`hasData`). Goroutines respect a `context.Context` for
  cancellation and never spawn unsupervised background work.
- **Test alongside the code.** Every package with logic has a
  `*_test.go`. The test layout uses table-driven tests where the
  cardinality is reasonable.
- **No emojis in source files, comments, log output, or docs.**

---

## Testing

Run the full suite:

```bash
go test ./... -count=1
```

Specific package:

```bash
go test ./internal/safeguard -count=1 -v
```

Coverage focuses on the parts most likely to silently regress:

- `internal/fritz/client_test.go` — login challenge math (MD5 + PBKDF2),
  REST request shape (path, SID `Authorization` header, Accept header),
  unit parsing for the FSE 250's `multimeterInterface`.
- `internal/safeguard/safeguard_test.go` — every Verdict / Reason / Health
  transition, including recovery hold-down.
- `internal/shelly/server_test.go` — discovery payload, EM.GetStatus
  both healthy and under all four StalePolicy modes, JSON-RPC envelope
  shape, unknown-method error.
- `internal/iniconf/iniconf_test.go` — quoted values, trailing comments,
  missing files, empty files.
- `internal/forecast/forecast_test.go` — the worked stale-cache
  compensation example from the README, genuine-refresh re-baselining,
  and `Reset`.
- `internal/modbus/client_test.go` — MBAP frame encode/decode and
  Read Holding Registers against a fake in-process TCP server.
- `internal/battery/battery_test.go` — Register/New registry lookup and
  unknown-type error.
- `internal/battery/solakon/solakon_test.go` — ACTIVE_POWER decoding
  (including negative/import values) against a fake in-process Modbus
  server.
- `cmd/shelly-fritz-proxy/main_test.go` — config precedence
  (CLI > env > file > default), explicit-vs-implicit `--config`
  semantics, invalid value rejection, `parseBatteryDevices` parsing and
  validation, `batteryDeviceLabel` formatting, and `runCheckBattery`'s
  error paths (empty `battery-devices`, unknown device type).

There are no integration tests against a real FRITZ!Box in the
repository. For ad-hoc end-to-end validation, drive the daemon with a
small Go HTTP stub (see the snippet in `docs/architecture.md`).

---

## Implementation Gotchas (worth remembering)

1. **The FSE 250 returns mW; the Shelly API expects W.** Conversion is
   centralised in `fritz.Client.GetMeasurement`.

2. **Power is signed on the FSE 250 (+import, −export); the Shelly Pro
   3 EM is also signed.** The sign survives unchanged through the
   `phase-mode` mapping.

3. **The REST API does not expose grid frequency.** The proxy reports a
   constant `50.0 Hz` and documents this in `README.md` and
   `data/shelly-fritz-proxy.conf`.

4. **The REST API only has one cumulative `energy` integer.** All of it
   goes into `total_act`; `total_act_ret` stays `0` unless the
   smartmeterInterface optional fields are present.

5. **Stuck-cache detection compares raw integers, not floats.** The
   `Raw*` fields on `fritz.Measurement` exist for this reason; float
   roundtrips would mask byte-equal samples behind ULP differences.

6. **mDNS lives on UDP 5353 multicast `224.0.0.251`.** When you run the
   binary outside the systemd unit (e.g. for ad-hoc testing) and bind
   to a high port, mDNS still binds to 5353 if `--no-mdns` is not set.

7. **The startup product-allowlist check is best-effort, not blocking,
   when the FRITZ!Box is briefly unreachable.** A clear WARN is logged
   ("startup device-info fetch failed; allowlist check deferred") and
   the daemon proceeds. The `check` subcommand is strict and exits
   non-zero — that is the right tool for installer scripts.

8. **`StalePolicyZero` is included but documented as dangerous.** It
   reports `0 W` with no `errors[]` flag, which would make Solakon ONE
   increase output unbounded. Never use it as a default; the docstring
   says so loudly.

9. **`flag.NewFlagSet` is used twice in `parseConfig`** because the
   first pass needs to discover `--config` without erroring on the
   other (not-yet-registered) flags. The helper `flagValue` reads argv
   directly to avoid registering anything.

10. **`internal/meter/testing.go` is built into the production binary**
    because the file lacks a `_test.go` suffix. This is intentional —
    sibling-package tests in `internal/shelly` need access to the
    `Apply()` helper. It is a thin shim and adds no overhead.

11. **`poll` cannot meaningfully go below 2 s.** The FRITZ!Box's
    DECT-ULE link to the FSE 250 only refreshes every ~2 s. Polling
    faster does not yield fresher data, but it does cause the
    stuck-data safeguard to trip during normal operation. Any
    "performance" PR that lowers the default `poll` is almost
    certainly wrong; see [`docs/architecture.md` — Real-time vs polled
    data](docs/architecture.md#real-time-vs-polled-data) for the full
    latency budget and the rejected mitigation list.

12. **Predictive interpolation between FRITZ polls is explicitly out
    of scope.** It would mean lying to the consumer about data
    freshness, which directly contradicts the safeguard layer's
    "fail honest, fail safe" principle. The architecture doc
    enumerates this and other rejected ideas under the "Mitigations
    that are NOT recommended" subsection — read it before proposing
    any latency-reduction scheme.

13. **`fritz-unit` is a unit UID, not always the device UID.** Most
    single-function devices report a bare unit UID identical to the
    device UID, but some FSE 250 firmware revisions split metering into
    `<AIN>-1` / `<AIN>-2` sub-units, neither of which equals the bare
    device UID that `GetDevice` needs. Always derive the device UID via
    `fritz.ParentDeviceUID(cfg.FritzUnitUID)` before calling
    `GetDevice`; passing the raw unit UID fails with `UID_NOT_FOUND`
    and silently defeats `RequireConnected`.

14. **Forecast mode's adjustment operates only on the value reported to
    Shelly consumers, never on the safeguard's own input.** `meter.Store`
    runs the safeguard against the real, unadjusted `fritz.Measurement`
    first; only a sample that the safeguard already Accepted gets passed
    through `forecast.Engine.Adjust`. Feeding an already-adjusted value
    back into the safeguard would corrupt its stuck-cache detector
    (which relies on byte-exact equality of the *real* upstream reading)
    with a value forecast mode may have deliberately changed.

15. **Solakon Modbus register addresses are used literally, no SunSpec
    offset.** Register `39134` (`ACTIVE_POWER`) and friends are passed
    directly into the Modbus PDU exactly as documented by FoxESS — do
    not apply a `-30001`/`+1`/`-40001` style base-address adjustment
    some other Modbus device families use. Getting this wrong would
    silently read the wrong register and feed garbage into forecast
    mode's control loop; when adding a new register, cross-check against
    `solakon-one-fritz-powerregulator`'s own C++ client
    (`src/solakonapi.cpp/.h`) rather than guessing.

16. **`defaultConfigPath` is a `var`, not a `const`, specifically so
    tests can override it.** `parseConfig` falls back to it whenever
    neither `--config` nor `$CONFIG` is given. A hardcoded
    `/etc/shelly-fritz-proxy/shelly-fritz-proxy.conf` const would make
    any test asserting "no config file" defaults silently pick up a
    real config file on a machine that happens to have this proxy
    actually installed. `main_test.go`'s `scrub()` helper points it at
    a guaranteed-nonexistent path inside `t.TempDir()` and restores the
    original on cleanup; any new test that needs "no config file"
    semantics should call `scrub(t)` rather than relying on the
    filesystem state of whatever host runs the tests. The var also
    doubles as a legitimate packaging hook
    (`-ldflags="-X main.defaultConfigPath=/other/path"`) for a distro
    with a non-standard `/etc` layout.

---

## Reference projects

| Project | Role |
|---|---|
| [`flunaras/fritzhome-cache`](https://github.com/flunaras/fritzhome-cache) | Source of build/docker/packaging conventions, FRITZ! REST API analysis, INI config layout, systemd unit template, and CLI ergonomics |
| [`flunaras/solakon-one-fritz-powerregulator`](https://github.com/flunaras/solakon-one-fritz-powerregulator) | Source of the safeguard design (stuck-data detection, product-name filter, fail-safe philosophy, settling delays, debounce counters), and of the Solakon ONE Modbus register map (`src/solakonapi.cpp/.h`) that `internal/battery/solakon` is built against |
| [Shelly Gen2 device docs](https://shelly-api-docs.shelly.cloud/gen2/Devices/Gen2/ShellyPro3EM) | Reference for the RPC payload shapes that this proxy emulates (`EM.GetStatus`, `EMData.GetStatus`, `Shelly.GetDeviceInfo`, ...) |
| [FRITZ! Smart Home REST API spec](https://fritz.support/resources/SmarthomeRestApiFRITZOS82.html) | The upstream API the proxy consumes |

---

## When extending this project

Before adding a new feature, check:

- **Is it read-only?** Anything that writes to the FRITZ!Box belongs in
  a different tool. The Shelly endpoint surface should also remain
  read-only.
- **Does it have a safe default?** A new safeguard knob must ship with
  a conservative default value. A new fail-safe mode must not be more
  permissive than `error`.
- **Is the failure path obvious?** Every new control path needs an
  answer to "what happens when the FRITZ!Box is unreachable?". The
  default answer is "report HealthStale and let the consumer apply
  its policy".
- **Does it survive a stuck cache?** New code paths should never assume
  that a fresh `multimeterInterface` value means the underlying meter
  has actually moved.
