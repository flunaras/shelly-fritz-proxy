// Command shelly-fritz-proxy emulates a Shelly Pro 3 EM on the local
// network and feeds it with live measurements from a FRITZ!Smart Energy 250
// connected to a FRITZ!Box. Use it where a consumer (such as a Solakon ONE
// inverter) speaks the Shelly Pro 3 EM API but you only own a FRITZ meter.
//
// Subcommands:
//
//	(default)   run the proxy
//	check       verify connectivity, credentials, and that the configured
//	            unit is on an allow-listed product (e.g. FRITZ!Smart Energy
//	            250). Exits non-zero on the first failing check. Useful as
//	            a pre-flight in installer scripts and ansible.
//	query       query the Shelly Gen2 RPC API of a running instance of this
//	            proxy (or a real Shelly Pro 3 EM). Useful for ad-hoc
//	            diagnostics without needing a Solakon ONE or other consumer
//	            on hand.
//	check-battery  read the current power output of every solar-battery
//	            device configured via --battery-devices (forecast mode),
//	            printing one line per device and exiting non-zero if any
//	            device fails to connect or read. Does not touch FRITZ!
//	            credentials or start the daemon; a debugging aid for
//	            forecast-mode setup.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/flunaras/shelly-fritz-proxy/internal/battery"
	_ "github.com/flunaras/shelly-fritz-proxy/internal/battery/solakon" // registers the "solakon" battery type
	"github.com/flunaras/shelly-fritz-proxy/internal/fritz"
	"github.com/flunaras/shelly-fritz-proxy/internal/iniconf"
	"github.com/flunaras/shelly-fritz-proxy/internal/mdns"
	"github.com/flunaras/shelly-fritz-proxy/internal/meter"
	"github.com/flunaras/shelly-fritz-proxy/internal/safeguard"
	"github.com/flunaras/shelly-fritz-proxy/internal/shelly"
	"github.com/flunaras/shelly-fritz-proxy/internal/shellyclient"
)

// defaultConfigPath is the built-in default --config path, used when
// neither --config nor $CONFIG is given. It is a var (not a const) for
// two reasons:
//
//   - Packaging can override it at build time, e.g.
//     `go build -ldflags="-X main.defaultConfigPath=/other/path"`, for
//     a distro whose packaging conventions mandate a different /etc
//     layout without touching source.
//   - Tests can override it per-test (save the original, defer
//     restoring it) so they exercise the "no file configured" code
//     path deterministically, regardless of whether the machine
//     running the tests happens to have a real
//     /etc/shelly-fritz-proxy/shelly-fritz-proxy.conf on it (e.g. from
//     an actual installed instance).
var defaultConfigPath = "/etc/shelly-fritz-proxy/shelly-fritz-proxy.conf"

// Built-in defaults. The config file, environment, and command line all
// override these in increasing order of priority.
const (
	defaultFritzURL   = "https://fritz.box"
	defaultListenAddr = ":80"
	defaultMAC        = "AA:BB:CC:11:22:33"
	defaultPhaseMode  = "split"
	defaultLogLevel   = "info"
	// Default allowlist mirrors solakon-one-fritz-powerregulator: only
	// the FRITZ!Smart Energy 250 is bidirectional and therefore safe to
	// feed into a closed-loop power regulator.
	defaultProductAllowlist = "FRITZ!Smart Energy 250"
	defaultStalePolicy      = "error"
)

const (
	defaultPollPeriod      = 2 * time.Second
	defaultMaxAge          = 30 * time.Second
	defaultStuckCycles     = 5
	defaultRecoverySamples = 3
	defaultMinVoltage      = 180.0
	defaultMaxVoltage      = 280.0
	defaultMaxAbsPower     = 30000.0
	defaultMaxStep         = 15000.0
	defaultSafeImportWatts = 5000.0
)

const defaultBatteryTimeout = 5 * time.Second

func main() {
	if len(os.Args) > 1 && os.Args[1] == "query" {
		if err := runQuery(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "fatal:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

type config struct {
	ConfigPath string

	// Subcommand. "" means run as a daemon.
	Subcommand string

	FritzURL      string
	FritzBasePath string
	FritzUser     string
	FritzPassword string
	FritzUnitUID  string

	ListenAddr  string
	AdvertiseIP string
	MAC         string

	PhaseMode  string
	PollPeriod time.Duration
	NoMDNS     bool
	LogLevel   string

	// Safeguard knobs.
	MaxAge            time.Duration
	StuckCycles       int
	StuckMatchVoltage bool
	RecoverySamples   int
	MinVoltage        float64
	MaxVoltage        float64
	MaxAbsPower       float64
	MaxStep           float64
	RequireConnected  bool

	// Fail-safe policy when the safeguard is unhealthy.
	StalePolicy     string
	SafeImportWatts float64

	// Product-name allowlist. Comma-separated list; empty disables.
	ProductAllowlist string
	// Disable the product allowlist entirely (an explicit opt-out
	// rather than relying on "empty string" sentinels).
	ProductAllowlistDisabled bool

	// Forecast mode (see internal/forecast): compensates for consumers
	// polling faster than FRITZ!Box refreshes its cache by also
	// reading back the configured solar-battery devices' own output.
	ForecastMode   bool
	BatteryDevices string // structured type=/host=/... entries; see parseBatteryDevices
	BatteryTimeout time.Duration
}

func run() error {
	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		return err
	}
	logger := newLogger(cfg.LogLevel)

	// check-battery is dispatched here, before the FRITZ! credential
	// checks below, since it never talks to a FRITZ!Box at all -- it
	// only needs battery-devices/battery-timeout.
	if cfg.Subcommand == "check-battery" {
		return runCheckBattery(cfg)
	}

	// Accept the AIN either exactly as printed on the device sticker
	// (with its internal space, e.g. "11657 0123456") or as a single run
	// of digits with the space omitted ("116570123456"); both are
	// canonicalized to the same value here so every log line,
	// comparison, and REST call downstream sees one consistent UID
	// regardless of how the operator typed fritz-unit.
	cfg.FritzUnitUID = fritz.NormalizeAIN(cfg.FritzUnitUID)

	if cfg.FritzUnitUID == "" {
		return errors.New("fritz-unit is required (set via --fritz-unit, FRITZ_UNIT, or the config file); this is the FSE 250's unit UID, usually equal to the AIN printed on the device")
	}
	if cfg.FritzPassword == "" {
		return errors.New("fritz-password is required (set via --fritz-password, FRITZ_PASSWORD, or the config file)")
	}

	fclient := fritz.NewClient(cfg.FritzURL, cfg.FritzUser, cfg.FritzPassword)
	if cfg.FritzBasePath != "" {
		fclient.SetBasePath(cfg.FritzBasePath)
	}

	switch cfg.Subcommand {
	case "":
		return runDaemon(cfg, fclient, logger)
	case "check":
		return runCheck(cfg, fclient, logger)
	default:
		return fmt.Errorf("unknown subcommand %q (try 'check', 'query', 'check-battery', or omit for daemon mode)", cfg.Subcommand)
	}
}

// runCheck performs a one-shot suitability check: log in, verify the
// configured unit exists, that the device is connected and reachable,
// and that its product name matches the allowlist. Exits the process
// via the returned error.
func runCheck(cfg *config, fclient *fritz.Client, logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	logger.Info("preflight check starting",
		"fritz_url", cfg.FritzURL, "unit", cfg.FritzUnitUID)

	// GetDevice wants the physical device's UID, which for a unit UID
	// with a "-<n>" suffix (e.g. an FSE 250 whose firmware splits
	// metering into "<AIN>-1"/"<AIN>-2") differs from the fritz-unit
	// value used below for GetMeasurement. See fritz.ParentDeviceUID.
	deviceUID := fritz.ParentDeviceUID(cfg.FritzUnitUID)
	dev, err := fclient.GetDevice(ctx, deviceUID)
	if err != nil {
		return fmt.Errorf("check failed: cannot fetch device %q: %w", deviceUID, err)
	}
	fmt.Printf("device      : %s (%s)\n", dev.Name, dev.AIN)
	fmt.Printf("product     : %s\n", dev.ProductName)
	fmt.Printf("manufacturer: %s\n", dev.Manufacturer)
	fmt.Printf("firmware    : %s\n", dev.FirmwareVersion)
	fmt.Printf("isConnected : %v\n", dev.IsConnected)

	if !cfg.ProductAllowlistDisabled {
		allowed := splitAllowlist(cfg.ProductAllowlist)
		if !productAllowed(dev.ProductName, allowed) {
			return fmt.Errorf("check failed: product %q is not in allowlist %v; "+
				"FSE 250 is the only currently-known bidirectional FRITZ! meter, "+
				"using a unidirectional model would feed a wrong sign to your inverter. "+
				"Pass --product-allowlist-disabled if you know what you are doing.",
				dev.ProductName, allowed)
		}
		fmt.Println("allowlist   : ok")
	} else {
		fmt.Println("allowlist   : disabled")
	}
	if !dev.IsConnected {
		return errors.New("check failed: device is offline at the FRITZ!Box right now")
	}

	// ResolveUnitUID handles the common case (configured value already
	// has a multimeterInterface) with a single extra REST call, and
	// falls back to discovering the right sub-unit for devices like this
	// FSE 250 whose firmware splits metering across "<AIN>-1"/"<AIN>-2".
	// See fritz.ResolveUnitUID.
	unitUID, err := fclient.ResolveUnitUID(ctx, cfg.FritzUnitUID)
	if err != nil {
		return fmt.Errorf("check failed: cannot resolve a usable measurement unit: %w", err)
	}
	if unitUID != strings.TrimSpace(cfg.FritzUnitUID) {
		fmt.Printf("unit        : %s (auto-resolved; configured fritz-unit %q has no measurement of its own -- "+
			"consider updating fritz-unit to this value)\n", unitUID, cfg.FritzUnitUID)
	}

	m, err := fclient.GetMeasurement(ctx, unitUID)
	if err != nil {
		return fmt.Errorf("check failed: cannot read measurement: %w", err)
	}
	fmt.Printf("state       : %s\n", m.State)
	fmt.Printf("voltage     : %.1f V\n", m.Voltage)
	fmt.Printf("current     : %.3f A\n", m.Current)
	fmt.Printf("power       : %.1f W (%s)\n", m.ActivePower,
		map[bool]string{true: "import", false: "export"}[m.ActivePower >= 0])
	fmt.Printf("energy      : %.0f Wh cumulative\n", m.Energy)
	fmt.Println("check       : OK")
	return nil
}

// runCheckBattery implements the "check-battery" subcommand: a debug
// aid for forecast-mode setup (see internal/forecast and the README's
// "Forecast mode" section). It parses --battery-devices exactly as
// runDaemon would, builds a battery.Reader for each configured device,
// and reads its current power output once, printing one line per
// device. It never touches FRITZ! credentials and never starts the
// daemon, so it is safe to run against a freshly edited config file
// before enabling forecast-mode for real.
//
// Exits with an error (non-zero) if battery-devices is empty, if any
// device fails to construct (e.g. unknown type or missing required
// key), or if any device's read fails -- mirroring runCheck's
// "first failing check wins" behaviour so it composes the same way in
// installer scripts.
func runCheckBattery(cfg *config) error {
	deviceCfgs, err := parseBatteryDevices(cfg.BatteryDevices)
	if err != nil {
		return fmt.Errorf("check-battery: %w", err)
	}
	if len(deviceCfgs) == 0 {
		return errors.New("check-battery: battery-devices is empty; configure at least one solar-battery device (see --battery-devices) before running this check")
	}

	anyFailed := false
	for i, dc := range deviceCfgs {
		timeout := dc.Timeout
		if timeout <= 0 {
			timeout = cfg.BatteryTimeout
		}
		label := batteryDeviceLabel(dc)

		r, err := battery.New(dc)
		if err != nil {
			fmt.Printf("[%d] %-40s FAILED to construct reader: %v\n", i, label, err)
			anyFailed = true
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		w, err := r.ReadPowerWatts(ctx)
		cancel()
		_ = r.Close()

		if err != nil {
			fmt.Printf("[%d] %-40s FAILED: %v\n", i, label, err)
			anyFailed = true
			continue
		}
		fmt.Printf("[%d] %-40s %9.1f W (%s)\n", i, label, w, map[bool]string{true: "export", false: "import"}[w >= 0])
	}

	if anyFailed {
		return errors.New("check-battery: one or more devices failed; see above")
	}
	fmt.Println("check-battery: OK")
	return nil
}

// batteryDeviceLabel renders a battery.DeviceConfig as a short
// human-readable identifier for check-battery's output, e.g.
// "solakon@192.168.1.60 port=502 unit=1". port/unit are only shown
// when explicitly configured, since their actual defaults are up to
// each implementation (this function makes no assumption about them).
func batteryDeviceLabel(dc battery.DeviceConfig) string {
	label := fmt.Sprintf("%s@%s", dc.Type, dc.Host)
	if dc.Port > 0 {
		label += fmt.Sprintf(" port=%d", dc.Port)
	}
	if dc.UnitID > 0 {
		label += fmt.Sprintf(" unit=%d", dc.UnitID)
	}
	return label
}

// runQuery implements the "query" subcommand: a thin CLI wrapper around
// internal/shellyclient for talking to a running instance of this proxy
// (or a real Shelly Pro 3 EM) over its Shelly Gen2 RPC API. It has its
// own flag set, deliberately independent of parseConfig/config, since it
// needs none of the FRITZ! credentials or safeguard knobs and should
// remain usable even when no config file or FRITZ!Box is reachable at
// all (e.g. querying a proxy running on another host).
func runQuery(args []string) error {
	fs := flag.NewFlagSet("shelly-fritz-proxy query", flag.ExitOnError)
	fs.SetOutput(os.Stderr)
	target := fs.String("target", "http://127.0.0.1", "Base URL of the Shelly device/proxy to query, e.g. http://192.168.1.50")
	method := fs.String("method", "EM.GetStatus", "RPC method to call, e.g. Shelly.GetDeviceInfo, EM.GetStatus, EMData.GetStatus, Sys.GetStatus")
	id := fs.Int("id", 0, "Component id parameter, sent as {\"id\": <id>} (used by EM.* and EMData.* methods)")
	discover := fs.Bool("discover", false, "Fetch GET /shelly instead of issuing an RPC call")
	health := fs.Bool("health", false, "Fetch GET /healthz/details instead of issuing an RPC call")
	timeout := fs.Duration("timeout", 10*time.Second, "Request timeout")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: shelly-fritz-proxy query [flags]")
		fmt.Fprintln(fs.Output(), "\nQueries the Shelly Gen2 RPC API served by a running shelly-fritz-proxy")
		fmt.Fprintln(fs.Output(), "(or any real Shelly Pro 3 EM) for ad-hoc diagnostics.")
		fmt.Fprintln(fs.Output())
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	client := shellyclient.New(*target, nil)

	var (
		out any
		err error
	)
	switch {
	case *discover:
		out, err = client.Discover(ctx)
	case *health:
		out, err = client.GetHealthDetails(ctx)
	default:
		switch *method {
		case "Shelly.GetDeviceInfo":
			out, err = client.GetDeviceInfo(ctx)
		case "EM.GetStatus":
			out, err = client.GetEMStatus(ctx, *id)
		case "EMData.GetStatus":
			out, err = client.GetEMDataStatus(ctx, *id)
		case "Sys.GetStatus":
			out, err = client.GetSysStatus(ctx)
		default:
			out, err = client.CallRaw(ctx, *method, url.Values{"id": {strconv.Itoa(*id)}})
		}
	}
	if err != nil {
		return fmt.Errorf("query %s: %w", *target, err)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func runDaemon(cfg *config, fclient *fritz.Client, logger *slog.Logger) error {
	mode, err := meter.ParsePhaseMode(cfg.PhaseMode)
	if err != nil {
		return err
	}
	stalePolicy, err := shelly.ParseStalePolicy(cfg.StalePolicy)
	if err != nil {
		return err
	}

	// Build the safeguard from the resolved config.
	sgCfg := safeguard.DefaultConfig()
	sgCfg.MaxAge = cfg.MaxAge
	sgCfg.StuckCycles = cfg.StuckCycles
	sgCfg.StuckMatchVoltage = cfg.StuckMatchVoltage
	sgCfg.RecoverySamples = cfg.RecoverySamples
	sgCfg.MinVoltage = cfg.MinVoltage
	sgCfg.MaxVoltage = cfg.MaxVoltage
	sgCfg.MaxAbsPower = cfg.MaxAbsPower
	sgCfg.MaxStep = cfg.MaxStep
	sgCfg.RequireConnected = cfg.RequireConnected
	guard := safeguard.NewGuard(sgCfg)

	// Resolve which unit UID to actually poll for measurements. This is
	// best-effort, matching the allowlist check below: if the FRITZ!Box
	// is briefly unreachable at startup, fall back to the raw configured
	// value and let the poller's own retry/health-reporting take over
	// (see AGENTS.md gotcha "the startup product-allowlist check is
	// best-effort, not blocking") rather than refusing to start over
	// what may be a transient issue.
	unitUID := cfg.FritzUnitUID
	{
		rctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		resolved, err := fclient.ResolveUnitUID(rctx, cfg.FritzUnitUID)
		cancel()
		if err != nil {
			logger.Warn("could not auto-resolve measurement unit at startup; polling configured fritz-unit as-is",
				"fritz_unit", cfg.FritzUnitUID, "err", err)
		} else {
			unitUID = resolved
			if unitUID != strings.TrimSpace(cfg.FritzUnitUID) {
				logger.Info("auto-resolved measurement unit",
					"configured", cfg.FritzUnitUID, "resolved", unitUID)
			}
		}
	}

	store := meter.NewStore(fclient, unitUID, mode, cfg.PollPeriod, guard, logger.With("component", "poller"))

	if cfg.ForecastMode {
		deviceCfgs, err := parseBatteryDevices(cfg.BatteryDevices)
		if err != nil {
			return fmt.Errorf("forecast-mode: %w", err)
		}
		if len(deviceCfgs) == 0 {
			return errors.New("forecast-mode is enabled but battery-devices is empty; configure at least one solar-battery device or disable forecast-mode")
		}
		readers := make([]battery.Reader, 0, len(deviceCfgs))
		for _, dc := range deviceCfgs {
			if dc.Timeout <= 0 {
				dc.Timeout = cfg.BatteryTimeout
			}
			r, err := battery.New(dc)
			if err != nil {
				return fmt.Errorf("forecast-mode: %w", err)
			}
			readers = append(readers, r)
		}
		store.EnableForecastMode(readers, cfg.BatteryTimeout)
		logger.Info("forecast mode enabled", "devices", len(readers))
	}

	// Run the suitability check synchronously so we refuse to start if
	// it fails. Operators that need to skip it (e.g. during initial
	// network bring-up) can pass --product-allowlist-disabled.
	if !cfg.ProductAllowlistDisabled {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		dev, err := fclient.GetDevice(ctx, fritz.ParentDeviceUID(unitUID))
		cancel()
		if err != nil {
			logger.Warn("startup device-info fetch failed; allowlist check deferred",
				"err", err)
		} else {
			allowed := splitAllowlist(cfg.ProductAllowlist)
			if !productAllowed(dev.ProductName, allowed) {
				return fmt.Errorf("refusing to start: configured unit is on product %q which is not in the allowlist %v. "+
					"Only the FRITZ!Smart Energy 250 is currently bidirectional; using a unidirectional model would feed wrong-sign data into your power regulator. "+
					"To override, pass --product-allowlist-disabled or set product-allowlist-disabled=true in the config.",
					dev.ProductName, allowed)
			}
			store.SetProductOk(true)
			logger.Info("startup allowlist check ok",
				"product", dev.ProductName, "isConnected", dev.IsConnected)
		}
	} else {
		store.SetProductOk(true)
		logger.Warn("product allowlist disabled; the proxy will accept any FRITZ! Smart Home device. " +
			"Make sure your meter is bidirectional or the inverter will see wrong-sign power.")
	}

	mac := normalizeMAC(cfg.MAC)
	srv := shelly.New(store, mac, shelly.StaleConfig{
		Policy:          stalePolicy,
		SafeImportWatts: cfg.SafeImportWatts,
	}, logger.With("component", "shelly"))

	mux := http.NewServeMux()
	srv.Register(mux)
	mux.HandleFunc("/healthz", makeHealthHandler(store))
	mux.HandleFunc("/healthz/details", makeHealthDetailsHandler(store, srv, guard))

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Start polling.
	go store.Run(ctx)

	logger.Info("starting Shelly Pro 3 EM emulator",
		"listen", cfg.ListenAddr,
		"phase_mode", cfg.PhaseMode,
		"poll_period", cfg.PollPeriod,
		"stale_policy", cfg.StalePolicy,
		"max_age", cfg.MaxAge,
		"stuck_cycles", cfg.StuckCycles,
		"device_id", srv.DeviceID,
		"config_file", cfg.ConfigPath,
	)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			cancel()
		}
	}()

	if !cfg.NoMDNS {
		ip := net.ParseIP(cfg.AdvertiseIP)
		if ip == nil {
			detected, err := detectOutboundIP()
			if err != nil {
				logger.Warn("could not detect advertise IP; mDNS disabled", "err", err)
			} else {
				ip = detected
			}
		}
		if ip != nil {
			_, portStr, _ := net.SplitHostPort(cfg.ListenAddr)
			port := uint16(80)
			if portStr != "" {
				if p, err := net.LookupPort("tcp", portStr); err == nil {
					port = uint16(p)
				}
			}
			resp := mdns.NewResponder(mdns.Service{
				InstanceName: srv.DeviceID,
				HostName:     srv.DeviceID + ".local",
				IP:           ip,
				HTTPPort:     port,
				TXT: []string{
					"gen=2",
					"app=Pro3EM",
					"id=" + srv.DeviceID,
					"ver=1.4.4",
					"arch=esp32",
				},
				Logger: logger.With("component", "mdns"),
			})
			go func() {
				if err := resp.Run(ctx); err != nil {
					logger.Warn("mdns responder stopped", "err", err)
				}
			}()
		}
	}

	<-ctx.Done()
	logger.Info("shutting down")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutCancel()
	_ = httpSrv.Shutdown(shutCtx)
	return nil
}

// makeHealthHandler returns 200 only when the safeguard is in
// HealthOk. Degraded (last sample rejected but freshness still good)
// returns 200 too, because consumers should still trust the last
// reading. Stale (no fresh data, stuck data, etc) returns 503.
// The body is plain text so trivial probes (k8s, docker, monit) just
// check the status code.
func makeHealthHandler(store *meter.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, h, ok := store.Snapshot()
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("init\n"))
			return
		}
		switch h.Health {
		case safeguard.HealthOk, safeguard.HealthDegraded:
			_, _ = fmt.Fprintf(w, "%s\n", h.Health)
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintf(w, "%s reason=%s\n", h.Health, h.Reason)
		}
	}
}

// makeHealthDetailsHandler returns a JSON document with the full health
// snapshot. Intended for human diagnostics and dashboards; not part of
// the Shelly RPC surface.
func makeHealthDetailsHandler(store *meter.Store, srv *shelly.Server, guard *safeguard.Guard) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, h, ok := store.Snapshot()
		out := map[string]any{
			"device_id":                 srv.DeviceID,
			"health":                    h.Health.String(),
			"reason":                    string(h.Reason),
			"last_accepted_age_seconds": h.LastAcceptedAge.Seconds(),
			"stuck_run_length":          h.StuckRunLength,
			"consecutive_rejects":       h.ConsecutiveRejects,
			"has_ever_had_data":         h.HasEverHadData,
			"product_ok":                h.ProductOk,
			"safeguard_config": map[string]any{
				"max_age":             guard.Config().MaxAge.String(),
				"stuck_cycles":        guard.Config().StuckCycles,
				"stuck_match_voltage": guard.Config().StuckMatchVoltage,
				"recovery_samples":    guard.Config().RecoverySamples,
				"min_voltage":         guard.Config().MinVoltage,
				"max_voltage":         guard.Config().MaxVoltage,
				"max_abs_power":       guard.Config().MaxAbsPower,
				"max_step":            guard.Config().MaxStep,
				"require_connected":   guard.Config().RequireConnected,
			},
		}
		status := http.StatusOK
		if !ok || (h.Health != safeguard.HealthOk && h.Health != safeguard.HealthDegraded) {
			status = http.StatusServiceUnavailable
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(out)
	}
}

// productAllowed returns true when product matches any entry in the
// allowlist (case-insensitive substring). Empty allowlist accepts
// everything; the empty-allowlist case should be guarded by
// ProductAllowlistDisabled at the caller.
func productAllowed(product string, allowlist []string) bool {
	if len(allowlist) == 0 {
		return true
	}
	p := strings.ToLower(product)
	for _, allow := range allowlist {
		if strings.Contains(p, strings.ToLower(strings.TrimSpace(allow))) {
			return true
		}
	}
	return false
}

// parseBatteryDevices parses the --battery-devices flag/config value
// into battery.DeviceConfig entries. The format is deliberately a
// single flat string (rather than repeated flags, which this project's
// flag/INI parsing does not support) of semicolon-separated entries,
// each a comma-separated key=value list:
//
//	type=solakon,host=192.168.1.60;type=solakon,host=192.168.1.61,port=502,unit=1
//
// Recognized keys: type (required), host (required), port, unit,
// timeout (a Go duration string, e.g. "5s"). Unknown keys are
// rejected outright rather than silently ignored, since a typo here
// would otherwise silently disable forecast-mode compensation for a
// device without any indication why.
func parseBatteryDevices(raw string) ([]battery.DeviceConfig, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []battery.DeviceConfig
	for _, entry := range strings.Split(raw, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		var cfg battery.DeviceConfig
		for _, kv := range strings.Split(entry, ",") {
			kv = strings.TrimSpace(kv)
			if kv == "" {
				continue
			}
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) != 2 {
				return nil, fmt.Errorf("battery-devices: invalid key=value pair %q in entry %q", kv, entry)
			}
			key := strings.ToLower(strings.TrimSpace(parts[0]))
			val := strings.TrimSpace(parts[1])
			switch key {
			case "type":
				cfg.Type = val
			case "host":
				cfg.Host = val
			case "port":
				p, err := strconv.Atoi(val)
				if err != nil {
					return nil, fmt.Errorf("battery-devices: invalid port %q in entry %q: %w", val, entry, err)
				}
				cfg.Port = p
			case "unit":
				u, err := strconv.Atoi(val)
				if err != nil {
					return nil, fmt.Errorf("battery-devices: invalid unit %q in entry %q: %w", val, entry, err)
				}
				cfg.UnitID = u
			case "timeout":
				d, err := time.ParseDuration(val)
				if err != nil {
					return nil, fmt.Errorf("battery-devices: invalid timeout %q in entry %q: %w", val, entry, err)
				}
				cfg.Timeout = d
			default:
				return nil, fmt.Errorf("battery-devices: unknown key %q in entry %q (recognized: type, host, port, unit, timeout)", key, entry)
			}
		}
		if cfg.Type == "" {
			return nil, fmt.Errorf("battery-devices: entry %q is missing required key 'type'", entry)
		}
		if cfg.Host == "" {
			return nil, fmt.Errorf("battery-devices: entry %q is missing required key 'host'", entry)
		}
		out = append(out, cfg)
	}
	return out, nil
}

func splitAllowlist(raw string) []string {
	var out []string
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// parseConfig resolves the effective configuration with this precedence:
//
//	command line  >  environment variable  >  config file  >  builtin default
//
// Two passes are needed: the first discovers --config (or $CONFIG) so the
// file can be loaded before the real flag set is registered. Once loaded,
// the file values become the *defaults* for the real flag registration,
// which in turn use env values as their own defaults.
//
// The first positional argument is treated as a subcommand.
func parseConfig(args []string) (*config, error) {
	// ── Subcommand extraction ─────────────────────────────────────────────
	var sub string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		args = args[1:]
	}

	// ── Pass 1: find --config ─────────────────────────────────────────────
	configPath, configRequired := resolveConfigPath(args)

	fileVals, fileErr := iniconf.ParseFile(configPath)
	if fileErr != nil {
		if !os.IsNotExist(fileErr) {
			return nil, fmt.Errorf("config: %w", fileErr)
		}
		if configRequired {
			return nil, fmt.Errorf("config: %w", fileErr)
		}
		fileVals = map[string]string{}
	}

	cfg := &config{ConfigPath: configPath, Subcommand: sub}

	// ── Pass 2: register the real flag set with computed defaults ─────────
	fs := flag.NewFlagSet("shelly-fritz-proxy", flag.ExitOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&cfg.ConfigPath, "config", configPath,
		"Path to the INI-style configuration file (env CONFIG)")

	// ── FRITZ!Box upstream ───────────────────────────────────────────────
	fs.StringVar(&cfg.FritzURL, "fritz-url",
		resolve(fileVals, "fritz-url", "FRITZ_URL", defaultFritzURL),
		"FRITZ!Box base URL, or a fritzhome-cache URL (env FRITZ_URL)")
	fs.StringVar(&cfg.FritzBasePath, "fritz-base-path",
		resolve(fileVals, "fritz-base-path", "FRITZ_BASE_PATH", ""),
		"REST API base path; defaults to '/api/v0' (env FRITZ_BASE_PATH)")
	fs.StringVar(&cfg.FritzUser, "fritz-user",
		resolve(fileVals, "fritz-user", "FRITZ_USER", ""),
		"FRITZ!Box user with SmartHome permission (env FRITZ_USER)")
	fs.StringVar(&cfg.FritzPassword, "fritz-password",
		resolve(fileVals, "fritz-password", "FRITZ_PASSWORD", ""),
		"FRITZ!Box password (env FRITZ_PASSWORD)")
	fs.StringVar(&cfg.FritzUnitUID, "fritz-unit",
		resolve(fileVals, "fritz-unit", "FRITZ_UNIT", ""),
		"FRITZ! Smart Home unit UID; for an FSE 250 this is the 12-digit AIN printed on the device, with or without the space (env FRITZ_UNIT)")

	// ── Shelly listener / mDNS ────────────────────────────────────────────
	fs.StringVar(&cfg.ListenAddr, "listen",
		resolve(fileVals, "listen", "LISTEN_ADDR", defaultListenAddr),
		"HTTP listen address (env LISTEN_ADDR)")
	fs.StringVar(&cfg.AdvertiseIP, "advertise-ip",
		resolve(fileVals, "advertise-ip", "ADVERTISE_IP", ""),
		"IP to publish in mDNS (auto if empty)")
	fs.StringVar(&cfg.MAC, "mac",
		resolve(fileVals, "mac", "DEVICE_MAC", defaultMAC),
		"Virtual MAC the fake Shelly should report")

	// ── Phase mapping / polling ──────────────────────────────────────────
	fs.StringVar(&cfg.PhaseMode, "phase-mode",
		resolve(fileVals, "phase-mode", "PHASE_MODE", defaultPhaseMode),
		"How to map the single FSE 250 channel to L1/L2/L3: 'split' (default) or 'a'")
	poll, err := parseDurationCandidate(resolve(fileVals, "poll", "POLL_PERIOD", ""), defaultPollPeriod)
	if err != nil {
		return nil, fmt.Errorf("invalid poll duration: %w", err)
	}
	fs.DurationVar(&cfg.PollPeriod, "poll", poll, "FRITZ!Box polling interval")
	noMDNS, err := parseBoolCandidate(resolve(fileVals, "no-mdns", "NO_MDNS", ""), false)
	if err != nil {
		return nil, fmt.Errorf("invalid no-mdns value: %w", err)
	}
	fs.BoolVar(&cfg.NoMDNS, "no-mdns", noMDNS, "Disable mDNS/Bonjour announcement")

	fs.StringVar(&cfg.LogLevel, "log-level",
		resolve(fileVals, "log-level", "LOG_LEVEL", defaultLogLevel),
		"debug|info|warn|error")

	// ── Safeguards ────────────────────────────────────────────────────────
	maxAge, err := parseDurationCandidate(resolve(fileVals, "max-age", "MAX_AGE", ""), defaultMaxAge)
	if err != nil {
		return nil, fmt.Errorf("invalid max-age: %w", err)
	}
	fs.DurationVar(&cfg.MaxAge, "max-age", maxAge,
		"Maximum age of the last accepted FRITZ sample before declaring stale. Default 30s.")
	stuckCycles, err := parseIntCandidate(resolve(fileVals, "stuck-cycles", "STUCK_CYCLES", ""), defaultStuckCycles)
	if err != nil {
		return nil, fmt.Errorf("invalid stuck-cycles: %w", err)
	}
	fs.IntVar(&cfg.StuckCycles, "stuck-cycles", stuckCycles,
		"Consecutive byte-equal samples that mark the FRITZ! cache as stuck. 0 disables.")
	stuckMatchVoltage, err := parseBoolCandidate(resolve(fileVals, "stuck-match-voltage", "STUCK_MATCH_VOLTAGE", ""), true)
	if err != nil {
		return nil, fmt.Errorf("invalid stuck-match-voltage: %w", err)
	}
	fs.BoolVar(&cfg.StuckMatchVoltage, "stuck-match-voltage", stuckMatchVoltage,
		"Require byte-equal voltage too before declaring data stuck (recommended).")
	recovery, err := parseIntCandidate(resolve(fileVals, "recovery-samples", "RECOVERY_SAMPLES", ""), defaultRecoverySamples)
	if err != nil {
		return nil, fmt.Errorf("invalid recovery-samples: %w", err)
	}
	fs.IntVar(&cfg.RecoverySamples, "recovery-samples", recovery,
		"Consecutive Accept verdicts needed to clear a stale/degraded state.")
	minV, err := parseFloatCandidate(resolve(fileVals, "min-voltage", "MIN_VOLTAGE", ""), defaultMinVoltage)
	if err != nil {
		return nil, fmt.Errorf("invalid min-voltage: %w", err)
	}
	fs.Float64Var(&cfg.MinVoltage, "min-voltage", minV, "Plausibility floor for voltage (V).")
	maxV, err := parseFloatCandidate(resolve(fileVals, "max-voltage", "MAX_VOLTAGE", ""), defaultMaxVoltage)
	if err != nil {
		return nil, fmt.Errorf("invalid max-voltage: %w", err)
	}
	fs.Float64Var(&cfg.MaxVoltage, "max-voltage", maxV, "Plausibility ceiling for voltage (V).")
	maxAP, err := parseFloatCandidate(resolve(fileVals, "max-abs-power", "MAX_ABS_POWER", ""), defaultMaxAbsPower)
	if err != nil {
		return nil, fmt.Errorf("invalid max-abs-power: %w", err)
	}
	fs.Float64Var(&cfg.MaxAbsPower, "max-abs-power", maxAP, "Plausibility ceiling for |power| (W). 0 disables.")
	maxStep, err := parseFloatCandidate(resolve(fileVals, "max-step", "MAX_STEP", ""), defaultMaxStep)
	if err != nil {
		return nil, fmt.Errorf("invalid max-step: %w", err)
	}
	fs.Float64Var(&cfg.MaxStep, "max-step", maxStep, "Maximum |Δpower| between two samples (W). 0 disables.")
	requireConnected, err := parseBoolCandidate(resolve(fileVals, "require-connected", "REQUIRE_CONNECTED", ""), true)
	if err != nil {
		return nil, fmt.Errorf("invalid require-connected: %w", err)
	}
	fs.BoolVar(&cfg.RequireConnected, "require-connected", requireConnected,
		"Reject samples while the FRITZ!Box reports the device offline.")

	// ── Stale policy ──────────────────────────────────────────────────────
	fs.StringVar(&cfg.StalePolicy, "stale-policy",
		resolve(fileVals, "stale-policy", "STALE_POLICY", defaultStalePolicy),
		"What to report when data is stale: error|freeze|safe_import|zero")
	safeWatts, err := parseFloatCandidate(resolve(fileVals, "safe-import-watts", "SAFE_IMPORT_WATTS", ""), defaultSafeImportWatts)
	if err != nil {
		return nil, fmt.Errorf("invalid safe-import-watts: %w", err)
	}
	fs.Float64Var(&cfg.SafeImportWatts, "safe-import-watts", safeWatts,
		"Watts to report as grid-import when stale-policy=safe_import (forces the inverter to throttle).")

	// ── Product allowlist ─────────────────────────────────────────────────
	fs.StringVar(&cfg.ProductAllowlist, "product-allowlist",
		resolve(fileVals, "product-allowlist", "PRODUCT_ALLOWLIST", defaultProductAllowlist),
		"Comma-separated list of FRITZ! product names (substring match) to accept.")
	disabled, err := parseBoolCandidate(resolve(fileVals, "product-allowlist-disabled", "PRODUCT_ALLOWLIST_DISABLED", ""), false)
	if err != nil {
		return nil, fmt.Errorf("invalid product-allowlist-disabled: %w", err)
	}
	fs.BoolVar(&cfg.ProductAllowlistDisabled, "product-allowlist-disabled", disabled,
		"Disable the product allowlist entirely. ONLY do this if you have manually verified that your meter is bidirectional.")

	// ── Forecast mode ──────────────────────────────────────────────────────
	forecastMode, err := parseBoolCandidate(resolve(fileVals, "forecast-mode", "FORECAST_MODE", ""), false)
	if err != nil {
		return nil, fmt.Errorf("invalid forecast-mode: %w", err)
	}
	fs.BoolVar(&cfg.ForecastMode, "forecast-mode", forecastMode,
		"Enable forecast-mode compensation for consumers that poll faster than FRITZ!Box refreshes its cache (see docs/architecture.md). Requires battery-devices to be set.")
	fs.StringVar(&cfg.BatteryDevices, "battery-devices",
		resolve(fileVals, "battery-devices", "BATTERY_DEVICES", ""),
		"Semicolon-separated list of solar-battery devices to read back for forecast mode, each a comma-separated key=value list, e.g. "+
			"'type=solakon,host=192.168.1.60;type=solakon,host=192.168.1.61,port=502,unit=1'. Recognized keys: type (required), host (required), port, unit, timeout.")
	batteryTimeout, err := parseDurationCandidate(resolve(fileVals, "battery-timeout", "BATTERY_TIMEOUT", ""), defaultBatteryTimeout)
	if err != nil {
		return nil, fmt.Errorf("invalid battery-timeout: %w", err)
	}
	fs.DurationVar(&cfg.BatteryTimeout, "battery-timeout", batteryTimeout,
		"Per-device timeout for forecast-mode battery reads, unless overridden per-device via 'timeout=' in battery-devices.")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return cfg, nil
}

func resolve(file map[string]string, fileKey, envKey, def string) string {
	if v, ok := os.LookupEnv(envKey); ok && v != "" {
		return v
	}
	if v, ok := file[fileKey]; ok {
		return v
	}
	return def
}

func resolveConfigPath(args []string) (path string, required bool) {
	if p, ok := flagValue(args, "config"); ok {
		return p, true
	}
	if p := os.Getenv("CONFIG"); p != "" {
		return p, true
	}
	return defaultConfigPath, false
}

func flagValue(args []string, name string) (string, bool) {
	long := "--" + name
	short := "-" + name
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == long || a == short:
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", true
		case strings.HasPrefix(a, long+"="):
			return a[len(long)+1:], true
		case strings.HasPrefix(a, short+"="):
			return a[len(short)+1:], true
		}
	}
	return "", false
}

func parseDurationCandidate(raw string, def time.Duration) (time.Duration, error) {
	if raw == "" {
		return def, nil
	}
	return time.ParseDuration(raw)
}

func parseBoolCandidate(raw string, def bool) (bool, error) {
	if raw == "" {
		return def, nil
	}
	return strconv.ParseBool(raw)
}

func parseIntCandidate(raw string, def int) (int, error) {
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseInt(raw, 10, 0)
	if err != nil {
		return 0, err
	}
	return int(v), nil
}

func parseFloatCandidate(raw string, def float64) (float64, error) {
	if raw == "" {
		return def, nil
	}
	return strconv.ParseFloat(raw, 64)
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}

func normalizeMAC(in string) string {
	s := strings.ReplaceAll(in, ":", "")
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ToUpper(s)
	if len(s) != 12 {
		// fall back to a stable default rather than crashing
		return "AABBCC112233"
	}
	return s
}

// detectOutboundIP returns the local IP that the kernel would use to reach
// a public address. It does not actually send any traffic; it only triggers
// route table lookup. Works on Linux/macOS/Windows.
func detectOutboundIP() (net.IP, error) {
	c, err := net.Dial("udp4", "1.1.1.1:80")
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP, nil
}
