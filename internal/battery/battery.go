// Package battery defines the pluggable interface that "forecast mode"
// (see internal/forecast) uses to read a solar battery inverter's
// current power output, together with a small registry of concrete
// implementations selected by a config string. The first (and
// currently only) implementation is "solakon" (internal/battery/solakon),
// for the FoxESS Solakon ONE.
package battery

import (
	"context"
	"fmt"
	"time"
)

// Reader reads the instantaneous power output of one solar battery
// device. Implementations should use the same sign convention as
// fritz.Measurement.ActivePower: positive = exporting/injecting power
// toward the household or grid, negative = drawing/importing. Forecast
// mode sums the outputs of all configured devices, so a consistent
// sign convention across implementations matters.
type Reader interface {
	// ReadPowerWatts returns the device's current power output in
	// watts.
	ReadPowerWatts(ctx context.Context) (float64, error)
	// Close releases any resources (connections, etc.) held by the
	// reader. Implementations that open a fresh connection per call
	// (as internal/modbus does) may make this a no-op.
	Close() error
}

// DeviceConfig is one entry of a configured solar-battery device: its
// implementation type and the address needed to reach it. Additional
// implementations may need more fields than Host/Port/UnitID; keep
// this struct a superset of what every known implementation needs
// rather than introducing per-type config types, so the CLI/INI
// parsing in cmd/shelly-fritz-proxy stays a single flat table.
type DeviceConfig struct {
	// Type selects the implementation, e.g. "solakon". Matched
	// case-insensitively.
	Type string
	// Host is the device's hostname, IP, or mDNS name.
	Host string
	// Port is the device's control-protocol port. 0 means "use the
	// implementation's default" (502 for Modbus TCP based readers).
	Port int
	// UnitID is the Modbus unit/slave ID, where applicable. 0 means
	// "use the implementation's default".
	UnitID int
	// Timeout bounds a single read. 0 means "use the implementation's
	// default".
	Timeout time.Duration
}

// Factory builds a Reader from a DeviceConfig. Registered by each
// implementation's package via Register (typically from an init()).
type Factory func(cfg DeviceConfig) (Reader, error)

var registry = map[string]Factory{}

// Register adds a Factory for the given type name (case-insensitive).
// Intended to be called from the implementing package's init(), e.g.
// internal/battery/solakon.
func Register(typeName string, f Factory) {
	registry[normalizeType(typeName)] = f
}

// New builds a Reader for cfg.Type using the registered Factory.
// Returns an error listing the known types if cfg.Type is not
// registered (most likely because the caller forgot to blank-import
// the implementation package).
func New(cfg DeviceConfig) (Reader, error) {
	f, ok := registry[normalizeType(cfg.Type)]
	if !ok {
		return nil, fmt.Errorf("battery: unknown device type %q (known types: %v)", cfg.Type, knownTypes())
	}
	return f(cfg)
}

func knownTypes() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	return out
}

func normalizeType(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}
