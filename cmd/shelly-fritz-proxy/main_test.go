package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flunaras/shelly-fritz-proxy/internal/battery"
)

// writeConf is a small helper that writes a file in the test's temp dir
// and returns its absolute path.
func writeConf(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.conf")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// scrub strips env vars that parseConfig consults so individual tests
// start from a clean slate. We do this at the start of every test that
// touches the env to avoid order-dependent flakiness.
//
// It also points defaultConfigPath at a path inside the test's temp
// dir that is guaranteed not to exist, restoring the original value on
// cleanup. Without this, a test that expects "no config file" behavior
// would silently pick up a *real*
// /etc/shelly-fritz-proxy/shelly-fritz-proxy.conf if one happens to
// exist on the machine running the tests (e.g. an actual installed
// instance) -- defaultConfigPath is a var specifically so tests can
// override it like this instead of depending on the filesystem state
// of whatever host happens to run them.
func scrub(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"CONFIG", "FRITZ_URL", "FRITZ_BASE_PATH", "FRITZ_USER", "FRITZ_PASSWORD",
		"FRITZ_UNIT", "LISTEN_ADDR", "ADVERTISE_IP", "DEVICE_MAC", "PHASE_MODE",
		"POLL_PERIOD", "NO_MDNS", "LOG_LEVEL",
	} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}

	orig := defaultConfigPath
	defaultConfigPath = filepath.Join(t.TempDir(), "does-not-exist.conf")
	t.Cleanup(func() { defaultConfigPath = orig })
}

func TestParseConfigDefaults(t *testing.T) {
	scrub(t)
	// With neither --config nor $CONFIG set, the loader falls back to
	// builtins since scrub() points defaultConfigPath at a path that is
	// guaranteed not to exist.
	cfg, err := parseConfig([]string{
		"--fritz-unit", "123",
		"--fritz-password", "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FritzURL != defaultFritzURL {
		t.Errorf("FritzURL: got %q, want %q", cfg.FritzURL, defaultFritzURL)
	}
	if cfg.ListenAddr != defaultListenAddr {
		t.Errorf("ListenAddr: got %q, want %q", cfg.ListenAddr, defaultListenAddr)
	}
	if cfg.PollPeriod != defaultPollPeriod {
		t.Errorf("PollPeriod: got %v, want %v", cfg.PollPeriod, defaultPollPeriod)
	}
	if cfg.NoMDNS {
		t.Errorf("NoMDNS should default to false")
	}
}

func TestParseConfigFile(t *testing.T) {
	scrub(t)
	conf := writeConf(t, `
# sample
fritz-url   = https://my-fritz.lan
fritz-user  = u
fritz-password = p
fritz-unit  = 116570123456
listen      = 127.0.0.1:8080
poll        = 5s
no-mdns     = true
log-level   = debug
phase-mode  = a
`)
	cfg, err := parseConfig([]string{"--config", conf})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.FritzURL != "https://my-fritz.lan" {
		t.Errorf("FritzURL: got %q", cfg.FritzURL)
	}
	if cfg.FritzUnitUID != "116570123456" {
		t.Errorf("UnitUID: got %q", cfg.FritzUnitUID)
	}
	if cfg.ListenAddr != "127.0.0.1:8080" {
		t.Errorf("ListenAddr: got %q", cfg.ListenAddr)
	}
	if cfg.PollPeriod != 5*time.Second {
		t.Errorf("PollPeriod: got %v", cfg.PollPeriod)
	}
	if !cfg.NoMDNS {
		t.Errorf("NoMDNS: should be true")
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel: got %q", cfg.LogLevel)
	}
	if cfg.PhaseMode != "a" {
		t.Errorf("PhaseMode: got %q", cfg.PhaseMode)
	}
}

func TestParseConfigPrecedence(t *testing.T) {
	scrub(t)
	conf := writeConf(t, `
fritz-url   = https://from-file.lan
fritz-password = file-pw
fritz-unit  = 1
`)
	t.Setenv("FRITZ_URL", "https://from-env.lan") // env beats file
	t.Setenv("FRITZ_PASSWORD", "env-pw")          // env beats file
	// CLI beats env
	cfg, err := parseConfig([]string{
		"--config", conf,
		"--fritz-url", "https://from-cli.lan",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FritzURL != "https://from-cli.lan" {
		t.Errorf("CLI should win: got %q", cfg.FritzURL)
	}
	if cfg.FritzPassword != "env-pw" {
		t.Errorf("env should beat file: got %q", cfg.FritzPassword)
	}
	if cfg.FritzUnitUID != "1" {
		t.Errorf("file should provide value missing from env/CLI: got %q", cfg.FritzUnitUID)
	}
}

func TestParseConfigMissingExplicitFileIsError(t *testing.T) {
	scrub(t)
	_, err := parseConfig([]string{
		"--config", filepath.Join(t.TempDir(), "nope.conf"),
		"--fritz-unit", "1", "--fritz-password", "p",
	})
	if err == nil {
		t.Fatal("expected error for explicitly-specified missing config")
	}
}

func TestParseConfigInvalidPoll(t *testing.T) {
	scrub(t)
	conf := writeConf(t, "poll = forever\n")
	_, err := parseConfig([]string{"--config", conf})
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

func TestFlagValue(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		key  string
		want string
		ok   bool
	}{
		{"separate", []string{"--config", "/etc/x.conf"}, "config", "/etc/x.conf", true},
		{"equals-long", []string{"--config=/etc/x.conf"}, "config", "/etc/x.conf", true},
		{"equals-short", []string{"-config=/etc/x.conf"}, "config", "/etc/x.conf", true},
		{"short-separate", []string{"-config", "/etc/x.conf"}, "config", "/etc/x.conf", true},
		{"absent", []string{"--other", "x"}, "config", "", false},
		{"only-name", []string{"--config"}, "config", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := flagValue(tc.args, tc.key)
			if ok != tc.ok {
				t.Fatalf("ok: got %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("value: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseBatteryDevices(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		got, err := parseBatteryDevices("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Fatalf("got %+v, want nil", got)
		}
	})

	t.Run("single device", func(t *testing.T) {
		got, err := parseBatteryDevices("type=solakon,host=192.168.1.60")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d devices, want 1", len(got))
		}
		if got[0].Type != "solakon" || got[0].Host != "192.168.1.60" {
			t.Fatalf("got %+v", got[0])
		}
	})

	t.Run("multiple devices with optional fields", func(t *testing.T) {
		got, err := parseBatteryDevices("type=solakon,host=192.168.1.60,port=502,unit=1,timeout=3s;type=solakon,host=192.168.1.61")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d devices, want 2", len(got))
		}
		if got[0].Port != 502 || got[0].UnitID != 1 || got[0].Timeout != 3*time.Second {
			t.Fatalf("got %+v", got[0])
		}
		if got[1].Host != "192.168.1.61" || got[1].Port != 0 {
			t.Fatalf("got %+v", got[1])
		}
	})

	t.Run("missing type", func(t *testing.T) {
		if _, err := parseBatteryDevices("host=192.168.1.60"); err == nil {
			t.Fatal("expected error for missing type")
		}
	})

	t.Run("missing host", func(t *testing.T) {
		if _, err := parseBatteryDevices("type=solakon"); err == nil {
			t.Fatal("expected error for missing host")
		}
	})

	t.Run("unknown key", func(t *testing.T) {
		if _, err := parseBatteryDevices("type=solakon,host=x,bogus=1"); err == nil {
			t.Fatal("expected error for unknown key")
		}
	})

	t.Run("invalid port", func(t *testing.T) {
		if _, err := parseBatteryDevices("type=solakon,host=x,port=notanumber"); err == nil {
			t.Fatal("expected error for invalid port")
		}
	})
}

func TestBatteryDeviceLabel(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  battery.DeviceConfig
		want string
	}{
		{"bare", battery.DeviceConfig{Type: "solakon", Host: "192.168.1.60"}, "solakon@192.168.1.60"},
		{"with port", battery.DeviceConfig{Type: "solakon", Host: "192.168.1.60", Port: 502}, "solakon@192.168.1.60 port=502"},
		{"with unit", battery.DeviceConfig{Type: "solakon", Host: "192.168.1.60", UnitID: 1}, "solakon@192.168.1.60 unit=1"},
		{"with port and unit", battery.DeviceConfig{Type: "solakon", Host: "192.168.1.60", Port: 502, UnitID: 1}, "solakon@192.168.1.60 port=502 unit=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := batteryDeviceLabel(tc.cfg); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunCheckBatteryEmptyDevices(t *testing.T) {
	err := runCheckBattery(&config{BatteryDevices: ""})
	if err == nil {
		t.Fatal("expected error for empty battery-devices")
	}
}

func TestRunCheckBatteryUnknownType(t *testing.T) {
	err := runCheckBattery(&config{BatteryDevices: "type=bogus,host=127.0.0.1"})
	if err == nil {
		t.Fatal("expected error for unknown battery type")
	}
}
