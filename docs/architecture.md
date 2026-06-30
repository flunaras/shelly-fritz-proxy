# Architecture — shelly-fritz-proxy

## Overview

`shelly-fritz-proxy` is a **single-binary, multi-goroutine Go service**
that pretends to be a Shelly Pro 3 EM on the local network and feeds it
with measurements polled from a FRITZ!Smart Energy 250. It has no
persistent on-disk state, no database, no event loop framework, and one
process-wide control loop driven by a `time.Ticker`.

```
┌───────────────────────────────────────────────────────────────────────┐
│                        shelly-fritz-proxy                             │
│                                                                       │
│   main()                                                              │
│     │                                                                 │
│     ├─ parseConfig()  (subcommand, INI file, env, flags)              │
│     │                                                                 │
│     ├─ subcommand "check"                                             │
│     │     └─ runCheck()  →  one round-trip preflight; exit 0/1        │
│     │                                                                 │
│     └─ default subcommand                                             │
│           └─ runDaemon()                                              │
│                ├─ optional startup product-allowlist check            │
│                │                                                      │
│                ├─ goroutine: meter.Store.Run(ctx)                     │
│                │   loop every `poll`:                                 │
│                │     fritz.Client.GetMeasurement(unit) →              │
│                │     safeguard.Guard.Evaluate(sample, dev) →          │
│                │     on Accept: apply phase mapping into State        │
│                │     on Reject/Stale: update HealthSnapshot only      │
│                │                                                      │
│                ├─ goroutine: http.Server on :80                       │
│                │   /shelly             /healthz                       │
│                │   /rpc/<Method>       /healthz/details (JSON)        │
│                │                                                      │
│                └─ goroutine: mdns.Responder.Run(ctx)                  │
│                    answers _shelly._tcp.local + _http._tcp.local      │
│                                                                       │
└───────────────────────────────────────────────────────────────────────┘
         │                                              │
         ▼                                              ▼
   FRITZ!Box  (or fritzhome-cache)               Solakon ONE / EVCC / …
   REST API + login_sid.lua                      HTTP RPC + mDNS browse
```

The three long-lived goroutines (poller, HTTP server, mDNS responder)
all watch the same `context.Context` derived from
`signal.NotifyContext(SIGINT, SIGTERM)` and shut down cleanly on signal.

---

## Module breakdown

### `cmd/shelly-fritz-proxy/main.go` — entry point and CLI

- Three-pass flag/env/file parsing:
  1. **Pre-pass** picks `--config` directly from `os.Args` so the INI
     file can be loaded before the real `flag.FlagSet` is registered.
  2. **File load** via `iniconf.ParseFile`. A missing default file is
     non-fatal; a missing user-specified `--config` or `$CONFIG` is.
  3. **Real flag parsing** registers every option with a default
     computed from the chain `env > file > builtin`, then `fs.Parse`
     applies CLI overrides on top.
- Owns signal handling (`signal.NotifyContext`) and the graceful
  shutdown timeout (3 s).
- Dispatches to one of two subcommands:
  - `runDaemon` — the production code path.
  - `runCheck` — one-shot preflight; logs in, fetches `DeviceInfo`,
    checks the product-name allowlist, reads one measurement, exits
    0 / 1.

### `internal/iniconf/iniconf.go` — INI parser

Minimal `key = value` parser. Supports:

- `#` and `;` whole-line comments.
- Trailing comments after a whitespace + `#` / `;`.
- Single- and double-quoted values (quotes are stripped).
- Sections (`[name]`) — recognised and silently ignored so the schema
  stays forward-compatible.
- `ErrEmpty` to distinguish "file present but blank" from "file present
  with values".

No external dependencies; pure stdlib.

### `internal/fritz/client.go` — FRITZ!Box REST client

Synchronous HTTP client backed by `http.Client{ Timeout: 10s, ... }`.

**Authentication flow** (PBKDF2 modern, MD5 legacy):

```
GET /login_sid.lua?version=2
  → parse <SessionInfo><Challenge>…</Challenge><SID>…</SID></>
  if SID != "0000000000000000": already authenticated, reuse
  else if Challenge starts with "2$":
        PBKDF2-HMAC-SHA256(password, salt1, iter1, 32 B)
        → PBKDF2-HMAC-SHA256(intermediate, salt2, iter2, 32 B)
        → response = "<salt2>$<hex of final hash>"
  else (legacy):
        response = "<challenge>-" + lowercase hex of
                   MD5(UTF-16LE(challenge + "-" + password))
GET /login_sid.lua?version=2&username=<u>&response=<r>
  → parse new <SID>; "0000000000000000" = auth failed
```

Both crypto paths are implemented inline (`pbkdf2Sha256`, `hmacSha256`,
`utf16le`) so the project does not depend on `golang.org/x/crypto`.

**REST request shape:**

```
GET {BaseURL}{BasePath}/smarthome/overview/units/{UID}
Accept: application/json
Authorization: AVM-SID <sid>
```

`BasePath` defaults to `/api/v0` (matching what `fritzhome-cache`
caches). The SID is carried in the `Authorization` header as `AVM-SID
<sid>` because that is what the Smart Home REST API's `AVM-SID`
security scheme actually specifies (`type: apiKey, in: header, name:
Authorization` in AVM's published OpenAPI spec) — unlike the legacy
AHA-HTTP-Interface, which does use `?sid=...`. `github.com/flunaras/
fritzhome` (`FritzApi::buildRestRequest`) and `fritzhome-cache`
(`router.cpp`'s `truncate_sid`, which reads the SID straight out of the
`Authorization` header) both confirm this. Sending the SID as a query
parameter instead makes the `/login_sid.lua` handshake succeed while
every subsequent REST call is rejected — a bug this client used to have.
On a 401/403 response the client transparently re-logs in once and
retries; any other non-2xx response is returned as an error with the
body trimmed and quoted.

**`Measurement` struct.** Carries both physical-unit floats (V, A, W,
Hz, Wh) and the raw wire integers (mV, mA, mW, Wh) plus the
`multimeterInterface.state` string. The raw integers are what the
safeguard's stuck-data detector compares byte-for-byte; relying on the
float conversion alone would mask byte-equal samples behind ULP
precision artifacts.

**`DeviceInfo` struct.** Trimmed-down response of
`/smarthome/overview/devices/{UID}`: `ProductName`, `IsConnected`,
`Manufacturer`, `FirmwareVersion`. Used by the startup allowlist check
and refreshed lazily (~ every 30 polls) by the meter store so
`RequireConnected` stays accurate.

**`NormalizeAIN(uid string) string`.** AVM's AIN/unit UID format is
"`<5 digits> <7 digits>`" (optionally followed by a `-<n>` sub-unit
suffix), and the internal space is meaningful: a live FRITZ!Box returns
`UID_NOT_FOUND` for the same digits with the space stripped. The
sticker on the device shows the space, but it is easy to lose when
copy-pasting into a shell argument, an environment variable, or a
systemd drop-in, so `NormalizeAIN` accepts either form and reinserts
the space when it is missing from an otherwise-standard 12-digit AIN.
Anything that does not match that exact shape (a fritzhome-cache
alias, a short UID used only in tests, ...) is returned unchanged
apart from trimming incidental surrounding whitespace, so it is always
safe to call on an arbitrary configured value. `GetMeasurement`,
`GetDevice`, `ParentDeviceUID`, and `ResolveUnitUID` all normalize
their UID argument through this function, and `main.go` additionally
normalizes `cfg.FritzUnitUID` once immediately after parsing so every
log line and the auto-resolved-unit comparison (see `ResolveUnitUID`
below) also see the canonical form.

**`ParentDeviceUID(unitUID string) string`.** The single `fritz-unit`
config value is a *unit* UID (what `GetMeasurement` needs), but
`GetDevice` needs the owning physical *device*'s UID, and those are not
always the same string. Most single-function devices (e.g. FSE 200/210
sockets) report a bare unit UID identical to the device UID. Some FSE
250 firmware revisions instead split metering into two sub-units,
`<AIN>-1` (general "avmMeter") and `<AIN>-2` ("avmMeterFeedIn") — this
was confirmed against a live FSE 250, whose `/smarthome/overview/units/`
response has no unit UID equal to the bare device AIN at all.
`ParentDeviceUID` strips one trailing `-<digits>` suffix, if present,
to recover the device UID in both cases; both `main.go` (the startup
allowlist check and the `check` subcommand) and
`meter.Store.refreshDevice` call it before calling `GetDevice`. Without
it, `GetDevice("<AIN>-1")` fails with `UID_NOT_FOUND` even though
`GetMeasurement("<AIN>-1")` succeeds — which silently defeats the
`RequireConnected` safeguard (it only rejects when `DeviceInfo` is
non-nil) and makes the `check` subcommand fail despite fully correct
credentials and configuration.

**`ResolveUnitUID(ctx, configuredUID string) (string, error)`.** The
inverse problem from `ParentDeviceUID`: if the operator points
`fritz-unit` at the bare device UID (as the FSE 200/210 convention and
the config file's own doc comment suggest), `GetMeasurement` fails with
`UID_NOT_FOUND` on an FSE 250 that splits metering into sub-units, even
though `GetDevice` on that same value succeeds fine — "the device
exists but the unit does not". `ResolveUnitUID` tries `configuredUID`
directly first (one REST call, no change in behaviour when it already
works), and only on failure fetches the parent device's `UnitUIDs` and
probes each with `GetMeasurement`, preferring the one whose `UnitType`
is exactly `"avmMeter"` over any other unit that also has a
`multimeterInterface`. It never auto-selects a unit whose `UnitType`
looks feed-in-only (case-insensitive substring match on `"feedin"`,
e.g. `"avmMeterFeedIn"`) — such a unit does not report grid import, so
treating it as the proxy's measurement source would feed wrong-sign
data to a power regulator; ResolveUnitUID returns an error in that case
instead of guessing. `runCheck` treats a resolution failure as a normal
fatal `check failed:` error (consistent with `check` being strict), and
prints a `unit :` line when the resolved value differs from what is
configured, nudging the operator to update `fritz-unit`. `runDaemon`
treats it as best-effort, exactly like the allowlist check right next
to it: on failure it logs a WARN and falls back to polling the raw
configured value, rather than refusing to start over what might be a
transient FRITZ!Box outage at boot. Because `main.go` already
normalized `cfg.FritzUnitUID` via `NormalizeAIN` before either
subcommand runs, and `ResolveUnitUID` normalizes its own argument
again as a defensive no-op, the "differs from what is configured"
comparison only fires for a genuine sub-unit switch (bare device UID
resolving to an `<AIN>-1`/`<AIN>-2` sub-unit) — an AIN typed without
its internal space never triggers it, since both sides of the
comparison are already canonicalized to the same value.

### `internal/safeguard/safeguard.go` — guard state machine

Owns the sample-by-sample decision of what to do with the most recent
FRITZ! reading. The Guard is single-threaded by construction: only the
poller goroutine calls `Evaluate` / `NotePollFailure`, and the meter
store snapshots the resulting `HealthSnapshot` under its own RWMutex
before serving any HTTP request.

**Inputs to `Evaluate(now, sample, devInfo)`:**

| Input | Source |
|---|---|
| `sample.State` | `multimeterInterface.state` |
| `sample.Voltage` (V) | `multimeterInterface.voltage / 1000` |
| `sample.ActivePower` (W, signed) | `multimeterInterface.power / 1000` |
| `sample.RawPower` (mW) | raw int for byte-equality |
| `sample.RawVoltage` (mV) | raw int for byte-equality |
| `devInfo.IsConnected` | `/devices/{UID}` (lazily refreshed) |

**Decision order** (first hit wins; later checks short-circuit):

```
if RequireConnected and devInfo and !devInfo.IsConnected:
    Reject  device_offline
if not in AcceptedStates (default {"valid"}):
    Reject  interface_state_invalid
if Voltage outside [MinVoltage, MaxVoltage]:
    Reject  out_of_range_voltage
if MaxAbsPower > 0 and |ActivePower| > MaxAbsPower:
    Reject  out_of_range_power
if MaxStep > 0 and lastAccepted and |ΔActivePower| > MaxStep:
    Reject  step_too_large
update stuck-run length (RawPower [+ RawVoltage]):
if stuck-run ≥ StuckCycles:
    Stale   stuck_data        ← also flips Health=HealthStale
otherwise:
    Accept  (no reason)
```

`NotePollFailure(now)` is called by the poller when the entire REST
round-trip fails (network error, malformed JSON, …). It does not change
the lastAccepted measurement but advances the freshness clock so that
`MaxAge` continues to count down, eventually escalating `Health` to
`HealthStale` even if `Evaluate` is never called.

**Health state machine:**

```
                     ┌──────────────┐
                     │  HealthInit  │ ────── no sample ever accepted
                     └──────┬───────┘        and now − firstEvaluatedAt
                            │                > MaxAge
                            │                       │
                       first Accept                 ▼
                            │                ┌───────────────┐
                            ▼                │  HealthStale  │
                     ┌──────────────┐        └──────┬────────┘
                     │   HealthOk   │ ◄────────────┘
                     └──┬────┬──────┘  RecoverySamples
                        │    │         consecutive Accepts
            any Reject  │    │ freshness expired or
                        │    │ stuck_data detected
                        ▼    ▼
              ┌─────────────┐  ┌──────────────┐
              │HealthDegraded│  │ HealthStale  │
              └──────┬──────┘   └──────────────┘
                     │
              one Accept clears
              the degraded flag
                     ▼
                HealthOk
```

`Reason` values are stable strings — they flow through to logs, to the
`/healthz/details` JSON, and to the Shelly `errors[]` array unchanged.
This is intentional so log scrapers and dashboards can key off them.

### `internal/meter/store.go` — shared in-memory state

Bridges the FRITZ-side (`fritz.Client`, `safeguard.Guard`) with the
Shelly-side (`shelly.Server`) and is the only place where the
single-phase FRITZ reading is split across three phases.

**Per-cycle sequence inside `Store.refresh`:**

```
1. fritz.Client.GetMeasurement(unit) → sample
   if error:
       safeguard.Guard.NotePollFailure(now) → update HealthSnapshot only
       return
2. if (poll counter % 30 == 0):
       go fritz.Client.GetDevice(unit)  → refresh DeviceInfo asynchronously
3. safeguard.Guard.Evaluate(now, sample, lastKnownDevice) → result
4. if result.Verdict == Accept:
       apply phase mapping into State,
       update HealthSnapshot,
       publish via Snapshot()
   else:
       leave State frozen,
       update HealthSnapshot.{Health, Reason, …}
```

**Phase mapping:**

| `phase-mode` | a_act_power | b_act_power | c_act_power | total_act_power |
|---|---|---|---|---|
| `split` (default) | P/3 | P/3 | P/3 | P |
| `a` | P | 0 | 0 | P |

`split` is the default because Solakon ONE (and most other consumers)
only act on `total_act_power`, where the split mode is mathematically
correct. The `a` mode is the most honest representation for clients
that look at per-phase values — but it breaks clients that require all
three phases to be non-null.

**Concurrency:** `Store` uses `sync.RWMutex` to protect `state`,
`lastDevice`, `health`, and `productOk`, plus `sync/atomic.Bool` for
the "have we ever had data" flag (read on every HTTP request). The
polling goroutine is the sole writer; HTTP handlers are readers.

### `internal/shelly/server.go` — Gen2 RPC HTTP server

Implements just enough of the Shelly Pro 3 EM (`SPEM-003CEBEU`,
`gen=2`, `profile=triphase`) for Solakon ONE and other clients to
recognise the proxy as a real Shelly during onboarding and to poll it
for live grid measurements.

**HTTP surface:**

| Path | Method | Purpose |
|---|---|---|
| `/shelly` | GET | Device discovery; first thing every Shelly client requests |
| `/rpc` | POST | JSON-RPC envelope `{id, method, params}` |
| `/rpc/<Method>` | GET | Shorthand; query string becomes params |

The dispatcher recognises (and answers) the following RPC methods:

```
Shelly.GetDeviceInfo  Shelly.GetStatus  Shelly.GetConfig
Shelly.ListMethods    Shelly.Reboot              (no-op)
Sys.GetStatus         Sys.GetConfig
WiFi.GetStatus        WiFi.GetConfig
EM.GetStatus          EM.GetConfig    EM.GetCTTypes
EMData.GetStatus      EMData.GetConfig
Eth.GetStatus         Eth.GetConfig
Cloud.GetStatus       Cloud.GetConfig            (stub, disabled)
MQTT.GetStatus        MQTT.GetConfig             (stub, disabled)
BLE.GetStatus         BLE.GetConfig              (stub, disabled)
```

Any other method returns the JSON-RPC error code `-32601 Method <X> not
Found`, exactly mirroring real-Shelly behaviour.

**Stale-policy logic.** Every `EM.GetStatus` / `EMData.GetStatus` call
inspects the meter store's current `HealthSnapshot`. If `Health !=
HealthOk` the response is built according to the configured
`StaleConfig.Policy`:

| Policy | Measurement fields | `errors[]` | Solakon ONE expected behaviour |
|---|---|---|---|
| `StalePolicyError` (default) | every numeric field is `null` | `["stale_data", "<reason>"]` | Pauses regulation, holds last setpoint |
| `StalePolicyFreeze` | last accepted values, frozen | `["stale_data", "<reason>"]` | Depends on client; graphs stay continuous |
| `StalePolicySafeImport` | synthesised positive grid-import at `SafeImportWatts` | `["stale_data", "<reason>"]` | Throttles inverter to minimum |
| `StalePolicyZero` | `0` everywhere | empty (intentionally) | **Increases output unbounded — DANGEROUS** |

The Shelly server is otherwise stateless: it builds every response
fresh from the current `Store.Snapshot()` tuple, so concurrent HTTP
clients always see a coherent view of the most recent acceptance.

### `internal/mdns/responder.go` — multicast DNS responder

Hand-rolled answerer for `_http._tcp.local` and `_shelly._tcp.local`
queries. Uses only `golang.org/x/net/dns/dnsmessage` and the stdlib;
no third-party zeroconf dependency.

On startup it picks the interface that owns the configured (or
auto-detected) outbound IP, joins the `224.0.0.251:5353` multicast
group, sends one gratuitous announcement, then enters an answer loop.
It listens for queries with a 1-second read deadline so the shutdown
context can interrupt it.

Records published:

```
shellypro3em-<mac>.local.        A    <IP>
_http._tcp.local.                PTR  shellypro3em-<mac>._http._tcp.local.
shellypro3em-<mac>._http._tcp.   SRV  0 0 80 shellypro3em-<mac>.local.
shellypro3em-<mac>._http._tcp.   TXT  gen=2 app=Pro3EM id=… ver=1.4.4 arch=esp32
(and the same for _shelly._tcp.local)
```

If the responder ever hits a multi-interface or IPv6 edge case, the
intended escape hatch is to delete this package and depend on
`github.com/grandcat/zeroconf` instead — `internal/mdns` is not on any
hot path.

---

## Control flow examples

### Healthy steady-state poll

```
Poller goroutine          fritz.Client          FRITZ!Box                Guard         Store
      │                         │                    │                     │             │
      │── GetMeasurement ──────►│                    │                     │             │
      │                         │── GET /api/v0/.. ─►│                     │             │
      │                         │◄── 200 JSON ───────│                     │             │
      │◄─── Measurement ────────│                    │                     │             │
      │                                                                    │             │
      │── Evaluate(now, m, dev) ────────────────────────────────────────►│             │
      │◄── {Verdict:Accept, Health:HealthOk, Reason:""} ─────────────────│             │
      │                                                                                    │
      │── apply(m, result) ────────────────────────────────────────────────────────────►│
      │       (phase split, update State + HealthSnapshot)                                │
      │                                                                                    │
      │── sleep until next tick                                                            │
```

### Stuck-cache detection

```
Cycle 1   sample.RawPower=900000 RawVoltage=230000   →  Accept   stuckRun=1
Cycle 2   sample.RawPower=900000 RawVoltage=230000   →  Accept   stuckRun=2
Cycle 3   sample.RawPower=900000 RawVoltage=230000   →  Accept   stuckRun=3
Cycle 4   sample.RawPower=900000 RawVoltage=230000   →  Accept   stuckRun=4
Cycle 5   sample.RawPower=900000 RawVoltage=230000   →  Stale  stuck_data   stuckRun=5
                                                            │
                                                            └─► Health = HealthStale
                                                                Shelly.EM.GetStatus
                                                                now applies stale-policy
```

### Cold-start with FRITZ!Box unreachable

```
runDaemon: fritz.GetDevice → timeout
        WARN "startup device-info fetch failed; allowlist check deferred"
        store.SetProductOk(false)  (defaults to false anyway)
        proceed to HTTP/mDNS setup

Poller cycle 1: GetMeasurement → timeout
        Guard.NotePollFailure(now)
        Health stays HealthInit (no samples yet)

Poller cycle 2..N: same
        eventually now − firstEvaluatedAt > MaxAge
        Health flips Init → Stale
        HTTP /healthz now returns 503 stale reason=poll_failure
```

---

## HTTP endpoint reference

### `GET /shelly`

Returns the device identification used by Shelly-aware clients during
discovery. The MAC is the value of `--mac` (or auto-defaulted), `gen`
is always `2`, `model` is always `SPEM-003CEBEU`, `profile` is always
`triphase`.

```json
{
  "name": null,
  "id":   "shellypro3em-aabbcc112233",
  "mac":  "AABBCC112233",
  "slot": 0,
  "model": "SPEM-003CEBEU",
  "gen":   2,
  "fw_id": "20241011-114449/1.4.4-g6d2a586",
  "ver":   "1.4.4",
  "app":   "Pro3EM",
  "auth_en": false,
  "auth_domain": null,
  "profile": "triphase"
}
```

### `POST /rpc` / `GET /rpc/<Method>`

Standard Shelly Gen2 RPC envelope. Successful response:

```json
{
  "id":     42,
  "src":    "shellypro3em-aabbcc112233",
  "result": { … method-specific payload … }
}
```

Error response (unknown method, malformed params):

```json
{
  "id":    42,
  "src":   "shellypro3em-aabbcc112233",
  "error": { "code": -32601, "message": "Method Light.Toggle not Found" }
}
```

### `GET /healthz`

Plain-text endpoint for trivial probes (k8s liveness, docker
healthcheck, monit). Returns one of:

| Body | HTTP | Meaning |
|---|---|---|
| `init\n` | 503 | No sample has ever been accepted |
| `ok\n` | 200 | Guard `HealthOk` |
| `degraded\n` | 200 | Guard `HealthDegraded` (last sample rejected but freshness still good) |
| `stale reason=<x>\n` | 503 | Guard `HealthStale`; consumers should treat the proxy as untrustworthy |

### `GET /healthz/details`

JSON document for dashboards. Returns the full `HealthSnapshot` plus
the resolved safeguard configuration. Returns 503 when the guard is
`Init` or `Stale`, 200 otherwise:

```json
{
  "device_id": "shellypro3em-aabbcc112233",
  "health":    "ok",
  "reason":    "",
  "last_accepted_age_seconds": 1.42,
  "stuck_run_length":   1,
  "consecutive_rejects": 0,
  "has_ever_had_data":   true,
  "product_ok":          true,
  "safeguard_config": {
    "max_age":             "30s",
    "stuck_cycles":        5,
    "stuck_match_voltage": true,
    "recovery_samples":    3,
    "min_voltage":         180,
    "max_voltage":         280,
    "max_abs_power":       30000,
    "max_step":            15000,
    "require_connected":   true
  }
}
```

---

## Build system

### CMake-free, Go-native

Unlike the sibling C++ projects there is no CMake, no toolchain file,
and no `find_package` calls. Cross-compilation is built into the Go
toolchain:

```sh
GOOS=linux GOARCH=arm64 \
  go build -trimpath -ldflags='-s -w' -o out/... ./cmd/shelly-fritz-proxy
GOOS=linux GOARCH=arm GOARM=7 \
  go build -trimpath -ldflags='-s -w' -o out/... ./cmd/shelly-fritz-proxy
```

`CGO_ENABLED=0` is set in the builder image so the resulting binary
links to no shared library at all (not even glibc) and runs on
glibc-based and musl-based distros alike.

### Docker pipeline

A **single** builder image (`docker/Dockerfile.builder`) installs Go and
nfpm. `docker/build.sh` then iterates over the six target aliases,
invoking `go build` with the appropriate `GOOS`/`GOARCH`/`GOARM` and
`nfpm pkg` with the appropriate `--packager deb|rpm`.

| Alias | GOOS | GOARCH | GOARM | Family | Format | Output |
|---|---|---|---|---|---|---|
| `opensuse-tumbleweed-x86_64`   | linux | amd64 | —  | opensuse | rpm | `out/opensuse/tumbleweed/x86_64/`   |
| `opensuse-tumbleweed-aarch64`  | linux | arm64 | —  | opensuse | rpm | `out/opensuse/tumbleweed/aarch64/`  |
| `debian-12-x86_64`             | linux | amd64 | —  | debian   | deb | `out/debian/12/amd64/`              |
| `raspbian-bookworm-aarch64`    | linux | arm64 | —  | raspbian | deb | `out/raspbian/bookworm/arm64/`      |
| `raspbian-bookworm-armhf`      | linux | arm   | 7  | raspbian | deb | `out/raspbian/bookworm/armhf/`      |
| `raspbian-bullseye-armhf`      | linux | arm   | 7  | raspbian | deb | `out/raspbian/bullseye/armhf/`      |

The aliases and `out/<family>/<distro>/<arch>/` layout match
`fritzhome-cache` so a shared CI matrix can drive both projects.

### Why one image instead of six

`fritzhome-cache` uses one Dockerfile per distro because C++ needs
distro-matched glibc and OpenSSL ABIs at link time. Go does not: the
standard library is statically linked, OpenSSL is not used (TLS comes
from `crypto/tls`), and the only system call the runtime makes through
the dynamic loader is for DNS — which `CGO_ENABLED=0` replaces with
Go's own resolver. So a Debian-based image with the Go cross-compiler
produces correct binaries for every target.

### Packaging

`data/nfpm.yaml` is one manifest that nfpm interprets as both `.deb`
and `.rpm` depending on `--packager`. The build script renders it
through a small `sed` envsubst pipeline (`${VERSION}`, `${RELEASE}`,
`${NFPM_ARCH}`, `${BINARY}`, `${MAINTAINER}`, `${VENDOR}`) before
invoking nfpm. The conf file is marked `type: config|noreplace` so a
package upgrade never clobbers user-edited values.

---

## Deployment

### Installed file layout

| Path | Contents | Mode |
|---|---|---|
| `/usr/bin/shelly-fritz-proxy` | binary | 0755 |
| `/etc/shelly-fritz-proxy/shelly-fritz-proxy.conf` | INI config (marked conffile) | 0644 |
| `/usr/lib/systemd/system/shelly-fritz-proxy.service` | systemd unit | 0644 |
| `/usr/share/doc/shelly-fritz-proxy/README.md` | this file's user-facing README | 0644 |

### systemd unit

```ini
[Service]
ExecStart=/usr/bin/shelly-fritz-proxy --config /etc/shelly-fritz-proxy/shelly-fritz-proxy.conf
DynamicUser=yes
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged @resources
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
Restart=on-failure
RestartSec=5s
```

- `DynamicUser=yes` allocates a transient unprivileged UID/GID at
  service start; no pre-created service account is needed.
- `AmbientCapabilities=CAP_NET_BIND_SERVICE` lets that dynamic user
  bind to port 80 (every other capability is removed by
  `CapabilityBoundingSet`).
- mDNS works without additional capabilities because
  `224.0.0.251:5353` is unprivileged.

Override individual settings with `systemctl edit shelly-fritz-proxy`
(creates a drop-in under `/etc/systemd/system/shelly-fritz-proxy.service.d/`).

---

## Safeguard configuration reference

| Key | Default | Failure mode it catches |
|---|---|---|
| `max-age` | `30s` | Network / auth stall, total poll failure |
| `stuck-cycles` | `5` | FRITZ!Box internal cache stall |
| `stuck-match-voltage` | `true` | False positives on constant loads |
| `recovery-samples` | `3` | Trusting a single sample after a long stall |
| `min-voltage` | `180 V` | Implausibly low voltage glitch |
| `max-voltage` | `280 V` | Implausibly high voltage glitch |
| `max-abs-power` | `30 kW` | Implausibly large power glitch |
| `max-step` | `15 kW` | Single-sample spike next to stable reading |
| `require-connected` | `true` | FRITZ!Box reports the meter offline |
| `product-allowlist` | `FRITZ!Smart Energy 250` | Wiring up an FSE 200 (unidirectional) by mistake |

| `stale-policy` | What `EM.GetStatus` returns when health != Ok | Solakon ONE expected behaviour |
|---|---|---|
| `error` (default) | nulled measurements, `errors:["stale_data","<reason>"]` | Pauses regulation |
| `freeze` | last accepted values, `errors:["stale_data","<reason>"]` | Depends on client |
| `safe_import` | synthesised positive grid-import at `safe-import-watts` | Throttles inverter to minimum |
| `zero` | `0` everywhere, no `errors[]` | **Increases output unbounded — DANGEROUS** |

The complete user-facing description of every knob is in
[`data/shelly-fritz-proxy.conf`](../data/shelly-fritz-proxy.conf).

---

## Real-time vs polled data

A real Shelly Pro 3 EM sits in the panel with the conductors running
through its CTs and emits fresh measurements continuously — its
measurement chip integrates the voltage and current waveforms each
mains cycle (every 20 ms at 50 Hz). HTTP polls always return the most
recent integration result, so the only latency between a load change
and the consumer seeing it is the consumer's own polling cycle.

A FRITZ!Smart Energy 250 is a DECT-ULE device hanging off a FRITZ!Box.
Its measurement chip is comparable, but the data has to traverse a
slow radio link, the FRITZ!Box's internal cache, and our proxy before
the consumer sees it. Each stage adds latency.

### End-to-end latency budget

```
load change at the breaker
   │
   ├── 0 to ~1 s    ── FSE 250 chip integrates the new RMS values
   │
   ├── 0 to ~2 s    ── DECT-ULE radio cycle
   │                   (FRITZ!Box's internal cache updates)
   │                   (worst case under interference: up to 10 s)
   │
   ├── 0 to `poll`  ── shelly-fritz-proxy polls the REST API
   │                   (default `poll = 2s`)
   │
   ├── 0 to ~1 s    ── Solakon ONE polls our Shelly endpoint
   │                   (Solakon's own ~1 Hz polling cycle)
   │
   └── ~1 s         ── Solakon updates the inverter setpoint;
                       the inverter ramps to the new value

Typical end-to-end latency : 3 to 7 seconds
Worst case before safeguards trip : tens of seconds
```

For comparison, a real Shelly Pro 3 EM in the same loop collapses the
first two stages and one polling boundary, giving an end-to-end
latency of roughly **0.5 to 1.5 seconds** — three to five times
faster.

The Shelly Gen2 API itself implicitly acknowledges that even a real
Shelly is a sampling sensor, not a continuous one: the documented
`total_active_power_change` webhook only fires when total active power
has changed by **at least 100 W and 5%**, and the per-phase
`active_power_change` threshold is **10 W and 5%**. Below that the
device does not consider the change worth reporting.

### Where this latency does and does not matter

| Use case | Affected by 3–7 s latency? |
|---|---|
| Solakon ONE zero-feed-in regulation on a Balkonkraftwerk | **No** — load changes are dominated by appliances (kettles, washing machines, EV chargers) that switch on for minutes at a time. A few seconds of accidental feed-in or under-feed averages out to fractions of a Wh per event. |
| Cumulative energy accounting (`EMData.GetStatus`, billing-grade kWh) | **No** — the cumulative counter is what matters; the per-cycle latency of `power` does not affect totals. |
| Dashboards and charts | **No** — human readers do not perceive 5 s. |
| Live demos showing "see, when I switch the kettle on, the inverter immediately responds" | **Yes** — there will be a 3–7 s visible delay vs ~1 s for a real Shelly. |
| Fast PID-style regulation, e.g. tracking a load that changes faster than the loop can react | **Yes** — would hunt or ring. **Do not use this proxy for that.** |
| Closed-loop grid-frequency stabilisation | **N/A** — the proxy does not expose grid frequency at all (FRITZ! REST API does not measure it). |

### Specific failure modes the latency creates

**1. Ringing on fast load transients.** When a load (e.g. a 1500 W
kettle) switches on and back off in less than the loop latency, the
inverter ramps up to cover it and then continues to push the same
1500 W into the grid for the remainder of the latency window before it
sees that the load has gone. This is sub-optimal but not dangerous: a
German Balkonkraftwerk inverter is limited to 800 W feed-in, and over
a day the cumulative effect is a few Wh of "wasted" surplus that
would have gone to the grid anyway once the battery filled up.

**2. Quantisation noise causing hunting.** If you set `poll` shorter
than the FRITZ!Box's own DECT-ULE refresh interval, consecutive REST
polls will sometimes return byte-equal values (the FRITZ!Box's
internal cache hasn't refreshed) and sometimes show small differences
(±5 to ±10 W) from where in the integration window the measurement
was taken. This would make the inverter setpoint wobble around the
true value. The safeguards turn this into a *visible failure* rather
than silent hunting — `stuck-cycles` will trip after enough byte-equal
samples — but the symptom is "regulation pauses every few minutes for
no obvious reason".

**3. DECT-ULE dropout.** This is the worst-case failure: the radio
link drops (interference, distance, low battery in some other
DECT-ULE device on the same FRITZ!Box), and the FRITZ!Box keeps
serving the last seen value for tens of seconds before flipping
`isConnected`. **Already mitigated** by the `max-age` safeguard
(default 30 s): the proxy declares the data stale and the configured
`stale-policy` takes over, normally pausing regulation.

### Why a lower `poll` interval is not better

The minimum useful `poll` interval is bounded by the FRITZ!Box's
DECT-ULE refresh rate to the FSE 250, which is approximately every
**2 s** under normal conditions. Polling faster than this:

- does not give you fresher data,
- generates avoidable network and CPU load on both the proxy and the
  FRITZ!Box,
- and almost certainly causes the `stuck-cycles` safeguard to trip
  during normal operation because the FRITZ!Box will repeatedly return
  byte-equal values.

`poll = 2s` (the default) is the right value for most setups. If you
run a `fritzhome-cache` instance in front of the FRITZ!Box and several
consumers all polling the same unit, you can keep `poll = 2s` on the
proxy without adding any FRITZ!Box load — `fritzhome-cache` coalesces
identical in-flight requests.

Going *slower* than 2 s (e.g. `poll = 5s`) is safe and reduces network
chatter; the only effect is slightly worse end-to-end latency. For a
Solakon ONE Balkonkraftwerk this is usually fine.

### Mitigations that are NOT recommended

These options would *appear* to help with latency but each introduces
its own failure mode that is worse than the latency itself. They are
listed here so future contributors do not re-invent them.

| Idea | Why it is rejected |
|---|---|
| **Predictive interpolation** (extrapolate `last_power + dP/dt × t` between FRITZ polls) | Lies to the consumer about data freshness. Works for processes with known dynamics (PV irradiance models); arbitrary household loads cannot be predicted, so this would invent transients that are not happening and miss real transients. The whole point of the safeguard layer is to be *honest* about data freshness. |
| **Adaptive polling** (poll faster when data is changing, back off when stable) | Marginal benefit — the FRITZ!Box still only updates every ~2 s, so polling at 500 ms after a change does not get you the next sample any sooner. Adds state-machine complexity for no real-world gain. |
| **Server-Sent Events / WebSocket push to consumers** | Solakon ONE polls; it does not subscribe. Push helps only clients that already support Shelly's `NotifyStatus` (Home Assistant, EVCC), and those have other meter integrations that bypass this proxy entirely. |
| **Buy a second meter (a real Shelly Pro 3 EM)** | This is the honest answer if you actually need sub-second latency — the FSE 250 is not built for that. But it defeats the purpose of using your existing FRITZ! meter. |

### What you can do if latency does become a real problem

In practice, the safeguard layer's `stale-policy = error` (default)
ensures the proxy fails safe, and Solakon ONE's loose ~1 Hz polling
cycle absorbs the per-cycle latency. The few-seconds delay does not
cause observable problems for typical zero-feed-in regulation.

If you ever do observe regulation problems specifically attributable
to latency, the right next step is **not** more proxy complexity but:

1. Inspect `GET /healthz/details` and look at `last_accepted_age_seconds`
   over time. If it consistently sits at 5+ seconds, you have a slow
   FRITZ!Box / FSE 250 link rather than a proxy issue.
2. Move the FSE 250 closer to the FRITZ!Box (DECT-ULE range), reduce
   2.4 GHz interference, or check the meter's reception in the
   FRITZ!Box web UI.
3. Lengthen `poll` to `5s` or `10s` and lengthen `max-age`
   proportionally. Less frequent updates from a stable source are
   better than frequent updates with jitter.
4. If none of that fixes it, the FSE 250 is the wrong tool for your
   specific control loop — install a real Shelly Pro 3 EM (or a
   wired-Modbus three-phase meter) and use this proxy only for
   energy-counting purposes.

---

## Dependencies

| Dependency | Version | Why |
|---|---|---|
| Go stdlib | 1.22+ | `log/slog`, `net/http`, `crypto/{tls,md5,sha256}`, `encoding/{json,xml,hex}` |
| `golang.org/x/net/dns/dnsmessage` | v0.56.0 | mDNS message packing/unpacking |
| `nfpm` | v2.39.0 | DEB + RPM packaging from one YAML (build-time only, not runtime) |

`nfpm` ships in the builder image and is never linked into the binary.

---

## Error handling conventions

- **No panics in `internal/`.** Every fallible function returns
  `(result, error)`. The single tolerated exception is `main()`
  printing a fatal line and calling `os.Exit(1)`.
- **Errors are values.** They flow up unchanged with `%w` wrapping
  where context is added. Callers that want to branch on a particular
  failure use `errors.Is` / `errors.As`.
- **Logging stays at the boundary.** Library packages take a
  `*slog.Logger` only when they own a goroutine. Pure-logic packages
  (`safeguard`, `iniconf`) do not log at all.
- **Network failures degrade, not kill.** `meter.Store.refresh` logs at
  WARN and calls `Guard.NotePollFailure`; the daemon stays up and the
  Shelly endpoints start reporting `HealthStale` after the freshness
  budget elapses.
- **Mutating RPC calls return `-32601`.** The proxy is read-only.
- **The product-allowlist check is best-effort at startup** so a
  briefly-unreachable FRITZ!Box does not block restarts. Operators that
  need strict pre-flight use the `check` subcommand instead.

---

## Coding conventions

- Go 1.22+; `go vet ./...` and `go test ./...` are the bar.
- `internal/` for every package; no public Go API surface.
- `log/slog` only; no `log.Printf` / `fmt.Println` outside `main` and
  test code.
- `sync.RWMutex` + `sync/atomic` for read-heavy shared state.
  `sync.Mutex` for write-heavy.
- Context propagation: every function that can block on I/O takes
  `context.Context`; goroutines select on `ctx.Done()`.
- Tests live next to the code (`*_test.go`); table-driven where the
  cardinality is reasonable.
- No emojis in source files, comments, log output, or documentation.

---

## Ad-hoc validation recipe

When you change anything that could affect a downstream Solakon ONE,
walk through the failure modes by hand using a tiny FRITZ stub:

```go
// fritz-stub.go (drop into a tmp dir, `go run` it)
package main

import (
    "encoding/json"
    "fmt"
    "log"
    "net/http"
    "sync/atomic"
)

var poll atomic.Int64

func main() {
    http.HandleFunc("/login_sid.lua", func(w http.ResponseWriter, r *http.Request) {
        fmt.Fprint(w, `<?xml version="1.0"?><SessionInfo><SID>deadbeefdeadbeef</SID><Challenge>x</Challenge><BlockTime>0</BlockTime></SessionInfo>`)
    })
    http.HandleFunc("/api/v0/smarthome/overview/devices/116570123456", func(w http.ResponseWriter, r *http.Request) {
        json.NewEncoder(w).Encode(map[string]any{
            "UID": "116570123456", "ain": "116570123456",
            "productName": "FRITZ!Smart Energy 250", "isConnected": true,
        })
    })
    http.HandleFunc("/api/v0/smarthome/overview/units/116570123456", func(w http.ResponseWriter, r *http.Request) {
        n := poll.Add(1)
        json.NewEncoder(w).Encode(map[string]any{
            "interfaces": map[string]any{
                "multimeterInterface": map[string]any{
                    "state":   "valid",
                    "voltage": 230000 + int(n%5),
                    "current": 4000,
                    "power":   -1234000 + int(n*1000),
                    "energy":  12345 + int(n),
                },
            },
        })
    })
    log.Fatal(http.ListenAndServe(":8085", nil))
}
```

Then:

```sh
shelly-fritz-proxy \
  --fritz-url http://127.0.0.1:8085 \
  --fritz-user u --fritz-password p \
  --fritz-unit 116570123456 \
  --listen 127.0.0.1:9876 \
  --poll 300ms --stuck-cycles 3 \
  --no-mdns --log-level info \
  --stale-policy error

# in another shell
curl 'http://127.0.0.1:9876/rpc/EM.GetStatus?id=0'
curl   http://127.0.0.1:9876/healthz
curl   http://127.0.0.1:9876/healthz/details
```

Stop the stub mid-stream to verify the proxy degrades to `stale
reason=poll_failure`. Patch the stub to return a constant payload to
verify the stuck-data path produces `stale reason=stuck_data`.
