# shelly-fritz-proxy

A small Go service that pretends to be a **Shelly Pro 3 EM** on your local
network and serves live measurements polled from a **FRITZ!Smart Energy 250**
attached to a FRITZ!Box.

It exists for one specific reason: the [Solakon ONE](https://www.solakon.de/)
inverter (and a handful of other devices) only accept a Shelly 3EM / Shelly
Pro 3EM as their smart meter, but you may already have an FSE 250 in your
distribution board and want to reuse it.

## Documentation map

- This README — install, configure, run.
- [`docs/architecture.md`](docs/architecture.md) — long-form architecture:
  module breakdown, control flow examples, HTTP endpoint reference,
  safeguard state machine diagrams, deployment layout.
- [`AGENTS.md`](AGENTS.md) — design contract for contributors and AI
  coding agents: invariants, packaging conventions, gotchas.
- [`data/shelly-fritz-proxy.conf`](data/shelly-fritz-proxy.conf) — every
  config knob with inline commentary.

## How it works

```
+-------------------+   REST API   +-----------+   "Shelly RPC"   +-------------+
| FRITZ!Smart       |   over HTTP  |  this     |  HTTP + mDNS     |  Solakon    |
| Energy 250 (DECT) |------------->|  proxy    |----------------->|  ONE / etc. |
+-------------------+              +-----------+                  +-------------+
                       ^
                       |
              (optionally via
               fritzhome-cache)
```

- The proxy logs into your FRITZ!Box (PBKDF2 challenge response on modern
  firmware, MD5 fallback on legacy boxes) and then polls the
  **FRITZ! Smart Home REST API** at
  `GET /api/v0/smarthome/overview/units/{UID}` for the FSE 250's
  `multimeterInterface` block every `--poll` seconds (default 2 s).
- It exposes a full Gen2 Shelly RPC surface on `--listen` (default `:80`).
  All the read endpoints a Solakon ONE asks for are implemented; mutating
  endpoints return `Method not Found`.
- It announces itself over multicast DNS as
  `shellypro3em-<mac>._http._tcp.local` and `_shelly._tcp.local` so the
  Solakon app discovers it like a real Shelly during onboarding.

## Building distribution packages

The repository ships with a Docker-based build pipeline modelled on the
sibling project [`fritzhome-cache`](https://github.com/flunaras/fritzhome-cache).
A single Docker image (`golang` + `nfpm`) cross-compiles the binary for
every supported target and packages it as both `.deb` and `.rpm`.

```sh
# Build for all six targets (default)
./docker/build.sh

# Build for a single target
./docker/build.sh --distro debian-12-x86_64
./docker/build.sh --distro opensuse-tumbleweed-aarch64
./docker/build.sh --distro raspbian-bookworm-armhf

# Debug build (no symbol stripping)
./docker/build.sh --distro debian-12-x86_64 --build-type Debug
```

Output lands in `out/<family>/<distro>/<arch>/`:

```
out/
├── debian/12/amd64/
│   ├── shelly-fritz-proxy
│   └── shelly-fritz-proxy_0.1.0-1_amd64.deb
├── opensuse/tumbleweed/
│   ├── x86_64/
│   │   ├── shelly-fritz-proxy
│   │   └── shelly-fritz-proxy-0.1.0-1.x86_64.rpm
│   └── aarch64/
│       ├── shelly-fritz-proxy
│       └── shelly-fritz-proxy-0.1.0-1.aarch64.rpm
└── raspbian/
    ├── bookworm/
    │   ├── arm64/
    │   │   ├── shelly-fritz-proxy
    │   │   └── shelly-fritz-proxy_0.1.0-1_arm64.deb
    │   └── armhf/
    │       ├── shelly-fritz-proxy
    │       └── shelly-fritz-proxy_0.1.0-1_armhf.deb
    └── bullseye/armhf/
        ├── shelly-fritz-proxy
        └── shelly-fritz-proxy_0.1.0-1_armhf.deb
```

The available distro aliases are kept identical to `fritzhome-cache` so a
single CI matrix can drive both projects.

### Prerequisites

- Docker (tested with 29.x)
- About 200 MB of free disk for the builder image plus ~10 MB per target

The build does NOT need a Go toolchain on the host. Go's native
cross-compiler runs inside the container; the resulting binaries are
statically linked (`CGO_ENABLED=0`) and run on glibc-based and
musl-based distros alike.

### Manual build (host toolchain)

```sh
go build -o shelly-fritz-proxy ./cmd/shelly-fritz-proxy
```

Requires Go 1.22 or newer.

## Installation

### Debian / Ubuntu / Raspberry Pi OS

```sh
sudo apt install ./out/debian/12/amd64/shelly-fritz-proxy_0.1.0-1_amd64.deb
```

### openSUSE

```sh
sudo zypper install ./out/opensuse/tumbleweed/x86_64/shelly-fritz-proxy-0.1.0-1.x86_64.rpm
```

Both packages install:

| Path                                                             | Contents                              |
| ---------------------------------------------------------------- | ------------------------------------- |
| `/usr/bin/shelly-fritz-proxy`                                    | Executable                            |
| `/etc/shelly-fritz-proxy/shelly-fritz-proxy.conf`                | Configuration file (marked conffile)  |
| `/usr/lib/systemd/system/shelly-fritz-proxy.service`             | systemd unit                          |
| `/usr/share/doc/shelly-fritz-proxy/README.md`                    | This file                             |

After install:

1. Edit `/etc/shelly-fritz-proxy/shelly-fritz-proxy.conf` and set at
   least `fritz-user`, `fritz-password`, and `fritz-unit` (the AIN
   printed on the device, with or without its internal space -- both
   forms are accepted).
2. `sudo systemctl enable --now shelly-fritz-proxy`
3. `sudo journalctl -u shelly-fritz-proxy -f`

## Configuration

Settings can be supplied in three places, in increasing order of priority:

1. **Config file** at `/etc/shelly-fritz-proxy/shelly-fritz-proxy.conf`
   (INI-style `key = value`, `#` comments). Use `--config <path>` to
   load a different file.
2. **Environment variables** (`FRITZ_URL`, `FRITZ_PASSWORD`, ...).
3. **Command-line flags** (`--fritz-url`, `--fritz-password`, ...).

Every option has the same name in all three places (with the leading
`--` stripped on the file/env side, e.g. `--fritz-url` → `fritz-url =`
or `FRITZ_URL=`).

The packaged systemd unit invokes the binary with
`--config /etc/shelly-fritz-proxy/shelly-fritz-proxy.conf`. To override
individual options without editing either file, use a systemd drop-in:

```sh
sudo systemctl edit shelly-fritz-proxy
```

```ini
[Service]
ExecStart=
ExecStart=/usr/bin/shelly-fritz-proxy \
  --config /etc/shelly-fritz-proxy/shelly-fritz-proxy.conf \
  --listen :8080 \
  --log-level debug
```

### systemd unit

The bundled unit uses `DynamicUser=yes`, so no pre-created service
account is required: systemd allocates an unprivileged UID/GID at start
time. To allow that user to bind to port 80, the unit grants
`AmbientCapabilities=CAP_NET_BIND_SERVICE` — no other privileges are
needed.

The unit also enables a long list of sandboxing options
(`ProtectSystem=strict`, `ProtectKernelTunables`, `SystemCallFilter`,
etc.) so the service runs with minimal access to the host.

## Configuration via environment

| Flag                          | Env                          | Default                                              |
| ----------------------------- | ---------------------------- | ---------------------------------------------------- |
| `--config`                    | `CONFIG`                     | `/etc/shelly-fritz-proxy/shelly-fritz-proxy.conf`    |
| `--fritz-url`                 | `FRITZ_URL`                  | `https://fritz.box`                                  |
| `--fritz-base-path`           | `FRITZ_BASE_PATH`            | `/api/v0`                                            |
| `--fritz-user`                | `FRITZ_USER`                 |                                                      |
| `--fritz-password`            | `FRITZ_PASSWORD`             |                                                      |
| `--fritz-unit`                | `FRITZ_UNIT`                 |                                                      |
| `--listen`                    | `LISTEN_ADDR`                | `:80`                                                |
| `--advertise-ip`              | `ADVERTISE_IP`               | autodetect                                           |
| `--mac`                       | `DEVICE_MAC`                 | `AA:BB:CC:11:22:33`                                  |
| `--phase-mode`                | `PHASE_MODE`                 | `split`                                              |
| `--poll`                      | `POLL_PERIOD`                | `2s`                                                 |
| `--no-mdns`                   | `NO_MDNS`                    | false                                                |
| `--log-level`                 | `LOG_LEVEL`                  | `info`                                               |
| `--max-age`                   | `MAX_AGE`                    | `30s`                                                |
| `--stuck-cycles`              | `STUCK_CYCLES`               | `5`                                                  |
| `--stuck-match-voltage`       | `STUCK_MATCH_VOLTAGE`        | `true`                                               |
| `--recovery-samples`          | `RECOVERY_SAMPLES`           | `3`                                                  |
| `--min-voltage`               | `MIN_VOLTAGE`                | `180`                                                |
| `--max-voltage`               | `MAX_VOLTAGE`                | `280`                                                |
| `--max-abs-power`             | `MAX_ABS_POWER`              | `30000`                                              |
| `--max-step`                  | `MAX_STEP`                   | `15000`                                              |
| `--require-connected`         | `REQUIRE_CONNECTED`          | `true`                                               |
| `--stale-policy`              | `STALE_POLICY`               | `error`                                              |
| `--safe-import-watts`         | `SAFE_IMPORT_WATTS`          | `5000`                                               |
| `--product-allowlist`         | `PRODUCT_ALLOWLIST`          | `FRITZ!Smart Energy 250`                             |
| `--product-allowlist-disabled`| `PRODUCT_ALLOWLIST_DISABLED` | false                                                |
| `--forecast-mode`             | `FORECAST_MODE`              | false                                                |
| `--battery-devices`           | `BATTERY_DEVICES`            |                                                      |
| `--battery-timeout`           | `BATTERY_TIMEOUT`            | `5s`                                                 |

## Using with fritzhome-cache

Because the proxy talks the REST API (and only the REST API), you can put
[`fritzhome-cache`](https://github.com/flunaras/fritzhome-cache) between
this proxy and your FRITZ!Box. The cache coalesces overlapping reads of
`/api/v0/smarthome/overview/units/{UID}` and serves them from RAM, which
means the FRITZ!Box only sees one request even if multiple consumers
(this proxy plus Home Assistant plus EVCC) all poll the same unit at
the same time.

Point `fritz-url` at the cache instead of `https://fritz.box`:

```ini
# /etc/shelly-fritz-proxy/shelly-fritz-proxy.conf
fritz-url = http://fritzhome-cache.lan:8080
```

`fritzhome-cache` forwards `/login_sid.lua` unchanged, so the SID
handshake still works as if the proxy were talking to the box directly.
Set `fritzhome-cache --ttl` to a value at or below this proxy's
`poll` interval to avoid serving stale measurements.

## The fundamental caveat

A Shelly Pro 3 EM measures **three phases independently**, the FSE 250
measures **one (the sum at the main breaker)**. The proxy supports two
mapping strategies via `phase-mode`:

| Mode    | What it does                                | When to use                                     |
| ------- | ------------------------------------------- | ----------------------------------------------- |
| `split` | divides the total power evenly across L1/L2/L3 | Default; works with Solakon ONE, EVCC, openWB |
| `a`     | reports everything on L1, leaves L2/L3 at 0    | Most honest, but breaks strict three-phase clients |

`split` produces correct *totals* (which is what most clients act on)
but loses the unbalanced-load information; the FSE 250 cannot
reconstruct that.

## Safeguards (read this if you use it with a power regulator)

FRITZ!Smart Energy 250 meters have a few well-known failure modes that
can produce wrong readings *without warning*:

1. **Stuck cache.** The FRITZ!Box internal cache for the meter sometimes
   does not refresh for a couple of minutes; the REST API keeps
   returning byte-for-byte identical values.
2. **DECT-ULE dropout.** The radio link drops; the FRITZ!Box keeps
   serving the last known value without immediately marking the device
   offline.
3. **One-sample glitches.** Occasional impossible values (32 kW spike,
   320 V over-voltage) that a regulator would treat as a real change.

Acting on bad data with an inverter regulator like Solakon ONE means
either over-feeding into the grid or pulling power that isn't there.
The proxy classifies every incoming sample as Accept / Reject / Stale
and lets you configure what it reports while the data is unsafe.

Inspired by the failure modes documented and addressed in
[`solakon-one-fritz-powerregulator`](https://github.com/flunaras/solakon-one-fritz-powerregulator)
(notably its `--fritz-stuck-cycles` and product-name filter), the
safeguard supports:

| Knob                   | Default                       | Failure mode it catches                   |
| ---------------------- | ----------------------------- | ----------------------------------------- |
| `max-age`              | `30s`                         | Network or auth stall                     |
| `stuck-cycles`         | `5`                           | FRITZ!Box internal cache stall            |
| `stuck-match-voltage`  | `true`                        | False positives on constant loads         |
| `min-voltage`          | `180`                         | Implausibly low voltage glitch            |
| `max-voltage`          | `280`                         | Implausibly high voltage glitch           |
| `max-abs-power`        | `30000`                       | Implausibly large power glitch            |
| `max-step`             | `15000`                       | Single-sample spike next to stable reading|
| `require-connected`    | `true`                        | FRITZ!Box reports the meter offline       |
| `recovery-samples`     | `3`                           | First good sample after a long stall isn't fully trusted yet |
| `product-allowlist`    | `FRITZ!Smart Energy 250`      | Operator accidentally wires up an FSE 200 (unidirectional)   |

When any safeguard fires, the `stale-policy` option determines what the
Shelly endpoints actually report. **Pick this carefully — it is the
single most important safety knob in this proxy.**

| Policy        | What the Shelly returns                                | What Solakon does                                  | When to use         |
| ------------- | ------------------------------------------------------ | -------------------------------------------------- | ------------------- |
| `error`       | `errors: ['stale_data']`, measurements `null`          | Pauses regulation, holds last setpoint (recommended) | **Default**       |
| `freeze`      | Last accepted values + `errors: ['stale_data']`        | Depends on client                                  | When you want continuous graphs |
| `safe_import` | Large positive grid-import (`safe-import-watts`)       | Throttles inverter to minimum                      | When `error` breaks your client |
| `zero`        | `total_act_power: 0`                                   | **Increases output unboundedly — DANGEROUS**       | Never                |

The proxy refuses to start when the configured unit is on a product
that doesn't match `product-allowlist`. As of this writing the FSE 250
is the only bidirectional FRITZ! Smart Home meter and the only one
that can correctly distinguish import from export.

### Pre-flight check

Run the `check` subcommand before enabling the systemd unit:

```sh
sudo shelly-fritz-proxy --config /etc/shelly-fritz-proxy/shelly-fritz-proxy.conf check
```

Sample output:

```
device      : Hausanschluss (116570123456)
product     : FRITZ!Smart Energy 250
manufacturer: AVM
firmware    : 5.30
isConnected : true
allowlist   : ok
state       : valid
voltage     : 230.5 V
current     : 5.366 A
power       : -1234.6 W (export)
energy      : 12345 Wh cumulative
check       : OK
```

Exits non-zero on the first failing check; suitable as a pre-condition
in installer scripts or ansible playbooks.

If `fritz-unit` is set to the bare device UID/AIN but that particular
device only exposes measurements on a sub-unit (some FSE 250 firmware
revisions split metering into `<AIN>-1` and `<AIN>-2`), `check`
auto-resolves the correct one and prints an extra line:

```
unit        : 16000 0036532-1 (auto-resolved; configured fritz-unit "16000 0036532" has no measurement of its own -- consider updating fritz-unit to this value)
```

The daemon does the same at startup (logged as `auto-resolved
measurement unit`). Updating `fritz-unit` to the printed value is
recommended (skips a REST round-trip on every restart) but not
required. Resolution always prefers a general bidirectional reading
(AVM's `avmMeter` unit type) and refuses to auto-select a
feed-in-only unit (`avmMeterFeedIn`), since that would not report grid
import and would feed wrong-sign data to your inverter. This message
is only about a genuine sub-unit switch -- entering `fritz-unit`
without its internal AIN space (e.g. `116570123456` instead of
`11657 0123456`) is normalized transparently and never triggers it.

### Observability

- `GET /healthz` returns `HTTP 200` with `ok` or `degraded` body when
  the safeguard is happy, `HTTP 503` with `stale reason=<x>` when not.
  Suitable for k8s/docker probes.
- `GET /healthz/details` returns a JSON document with the full health
  snapshot plus the resolved safeguard configuration. Use it for
  dashboards and alerting.

## Onboarding with the Solakon app

1. Make sure the host running the proxy is on the same VLAN/subnet as
   your Solakon ONE.
2. **Run the pre-flight check** (see Safeguards above) to verify the
   FRITZ! side end-to-end before the inverter sees anything.
3. Power on the proxy and verify it is reachable: `curl http://<host>/shelly`.
4. In the Solakon app, choose *"Shelly Pro 3EM"* during smart-meter
   setup. The app discovers Shelly devices via mDNS; the proxy should
   appear within a few seconds.

## Forecast mode

A consumer that polls this proxy much faster than the FRITZ!Box
actually refreshes its cached measurement can escalate its own output
without bound. The problem, worked through:

1. Solakon currently outputs 20 W.
2. Solakon queries current power consumption: FRITZ genuinely reports
   50 W import.
3. Solakon increases its own output by 50 W to balance it (now 70 W).
4. Solakon queries again. The FRITZ!Box's internal cache has not
   refreshed yet, so it still reports the *same* 50 W — but the real
   household import is now near zero because Solakon already covered
   it in step 3. Solakon has no way to tell this reading apart from a
   genuine new 50 W import, so it increases output by *another* 50 W.
5. This repeats every cycle until FRITZ's cache finally refreshes,
   overshooting the household load by a large margin in the meantime.

Forecast mode (`--forecast-mode`) counters this by also reading back
the current power output of the configured solar-battery device(s) on
every poll cycle, alongside the FRITZ!Box reading:

- Whenever the FRITZ reading is genuinely new (its raw byte value
  differs from the last poll), it is reported unchanged, and the
  proxy remembers both that reading and the battery's output sum at
  that moment.
- Whenever the FRITZ reading is unchanged from the last poll (almost
  certainly a stale cache, not a coincidentally-static household
  load — see the stuck-cache safeguard above), the proxy instead
  reports:

  ```
  remembered_fritz_value - current_battery_sum + remembered_battery_sum
  ```

  i.e. it backs out however much the battery's own output has moved
  since the last genuine reading. Continuing the example: at step 4,
  `50 - 70 + 20 = 0` is reported instead of the stale `50`, so Solakon
  correctly sees "no further import to cover" and stops escalating.

This adjustment is applied only to the value reported to Shelly
consumers; it never feeds back into the safeguard's own stuck-cache
detection, which continues to operate on the real, unadjusted FRITZ!
reading.

### Configuring forecast mode

Battery devices are configured as a single, semicolon-separated list
of comma-separated `key=value` entries (there is no repeated-flag
support in this project's flag/INI parser):

```ini
forecast-mode = true
battery-devices = type=solakon,host=192.168.1.148,port=502,unit=1
```

Multiple devices (summed together each cycle):

```ini
battery-devices = type=solakon,host=192.168.1.148;type=solakon,host=192.168.1.149
```

Recognized keys per entry: `type` (required), `host` (required),
`port` (default depends on the implementation; 502 for Modbus TCP),
`unit` (Modbus unit/slave ID, default 1), `timeout` (per-device read
timeout, defaults to `--battery-timeout`, itself default `5s`).

The only implementation currently shipped is `solakon`, for a
**FoxESS Solakon ONE**, read over Modbus TCP (register `39134`,
`ACTIVE_POWER` — the inverter's current grid export power, positive
for export and negative for import; register map and wire format
cross-checked against
[`solakon-one-fritz-powerregulator`](https://github.com/flunaras/solakon-one-fritz-powerregulator)'s
own Modbus client). Additional device types can be added by
implementing `internal/battery.Reader` and registering a factory via
`internal/battery.Register` — see `internal/battery/solakon` for a
worked example.

If a configured device's read fails or times out (`--battery-timeout`,
default `5s`), it contributes `0 W` for that cycle rather than
blocking the whole poll or crashing the proxy; the poller logs a
warning. Enabling `forecast-mode` with an empty `battery-devices` list
is a startup error.

### Testing configured battery devices

Use the `check-battery` subcommand to verify `battery-devices` before
(or without) enabling `forecast-mode` — it reads each configured
device's current power output once and prints the result, without
touching FRITZ! credentials or starting the daemon:

```sh
shelly-fritz-proxy --battery-devices "type=solakon,host=192.168.1.148,port=502,unit=1" check-battery
```

Sample output:

```
[0] solakon@192.168.1.148 port=502 unit=1      850.0 W (export)
check-battery: OK
```

Exits non-zero (printing `FAILED: <error>` for the offending device)
if any device cannot be constructed (e.g. an unknown `type=`) or its
read fails, so it composes the same way as `check` in installer
scripts. It reads `--config`/env/CLI exactly like the daemon does, so
running it against your actual config file (or drop-in) exercises the
same values `forecast-mode` would use in production.

## Querying the proxy directly

The `query` subcommand is a small CLI client for this proxy's own
Shelly RPC API (or any real Shelly Pro 3 EM) — handy for ad-hoc
diagnostics without needing a Solakon ONE or other consumer on hand:

```sh
# Discover a device (GET /shelly)
shelly-fritz-proxy query --target http://192.168.1.50 --discover

# Read the current EM.GetStatus payload (the default)
shelly-fritz-proxy query --target http://192.168.1.50

# Read cumulative energy
shelly-fritz-proxy query --target http://192.168.1.50 --method EMData.GetStatus

# Read this proxy's own safeguard/health details
shelly-fritz-proxy query --target http://192.168.1.50 --health
```

Run `shelly-fritz-proxy query --help` for the full flag reference.

## Limitations

- **Single-phase only physically.** See the caveat above.
- **End-to-end latency of ~3–7 s** vs ~1 s for a real Shelly Pro 3 EM.
  The FRITZ!Smart Energy 250 talks to the FRITZ!Box over DECT-ULE
  which updates roughly every 2 s. This is fine for typical Solakon
  ONE / Balkonkraftwerk zero-feed-in regulation but unsuitable for
  fast PID-style control loops. See
  [`docs/architecture.md` — Real-time vs polled data](docs/architecture.md#real-time-vs-polled-data)
  for the full latency budget and what mitigations exist (and why
  most of them are bad ideas).
- **No Modbus emulation.** Shelly clients that talk Modbus instead of
  RPC are not supported (`modbus` interface enabled on the Pro 3 EM is
  optional; most consumers use RPC).
- **No power-factor data.** The FSE 250 does not expose PF; the proxy
  reports `1.0` for non-zero loads, `0` when idle.
- **No grid frequency.** The Smart Home REST API does not expose grid
  frequency; the proxy reports a constant `50.0 Hz`.
- **Energy import / export split is best-effort.** The documented REST
  schema only has one `energy` integer; if your firmware adds
  `energyImported` / `energyExported` to `smartmeterInterface`, the
  proxy will pick them up automatically. Otherwise the whole counter
  goes into `total_act` and `total_act_ret` stays at zero. The
  directionality of *power* (sign of `total_act_power`) is always
  correct because the FSE 250 reports a signed `power` field.
- **Webhooks / outbound websocket are not implemented.** If your client
  insists on them, this proxy is the wrong tool.

## Disclaimer

This project is **not affiliated with Allterco / Shelly or with AVM /
FRITZ!** Use it on your own network, at your own risk. Pretending to be
hardware you do not own is a layer-7 trick and your client *will*
notice if you do something silly, like reporting 50 kA on L3.
