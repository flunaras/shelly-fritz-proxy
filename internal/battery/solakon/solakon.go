// Package solakon implements internal/battery.Reader for the FoxESS
// Solakon ONE (H3/H3 Pro family) battery inverter, read over Modbus
// TCP.
//
// Register map and wire format cross-checked against
// github.com/flunaras/solakon-one-fritz-powerregulator (the sibling
// project's C++ Modbus client, src/solakonapi.cpp/.h):
//
//	Register 39134  ACTIVE_POWER  int32, 2 registers, FoxESS big-endian
//	                word order (high word first), scale: raw == watts
//	                (comment there: "raw / 1000 = kW -> raw value ==
//	                watts"). Sign: +export / -import at the inverter's
//	                grid connection -- i.e. the inverter's current
//	                commanded/actual power contribution, which is
//	                exactly the "current power output" this proxy's
//	                forecast mode needs (see internal/forecast).
//
// Register addresses are used exactly as documented by FoxESS: no
// SunSpec-style base offset (no -30001/+1/-40001 adjustment) is
// applied; they are passed directly as the Modbus PDU address.
package solakon

import (
	"context"
	"fmt"
	"time"

	"github.com/flunaras/shelly-fritz-proxy/internal/battery"
	"github.com/flunaras/shelly-fritz-proxy/internal/modbus"
)

func init() {
	battery.Register("solakon", newReader)
}

const (
	// defaultPort is the standard Modbus TCP port; the Solakon ONE
	// does not offer an alternative.
	defaultPort = 502
	// defaultUnitID matches this project's sibling
	// solakon-one-fritz-powerregulator's config.h default
	// (solakon_slave_id = 1).
	defaultUnitID = 1
	// regActivePower is ACTIVE_POWER: the inverter's current grid
	// export power (+export/-import), int32 spanning 2 holding
	// registers.
	regActivePower = 39134
)

// Reader implements battery.Reader for a Solakon ONE reachable at
// Host:Port via Modbus TCP.
type Reader struct {
	client *modbus.Client
}

// New builds a Solakon ONE Reader directly (bypassing the
// battery.New/Register registry), useful for tests or callers that
// already know they want a Solakon device.
func New(host string, port int, unitID int, timeout time.Duration) *Reader {
	if port <= 0 {
		port = defaultPort
	}
	if unitID <= 0 {
		unitID = defaultUnitID
	}
	addr := fmt.Sprintf("%s:%d", host, port)
	return &Reader{client: modbus.New(addr, byte(unitID), timeout)}
}

func newReader(cfg battery.DeviceConfig) (battery.Reader, error) {
	if cfg.Host == "" {
		return nil, fmt.Errorf("solakon: host is required")
	}
	return New(cfg.Host, cfg.Port, cfg.UnitID, cfg.Timeout), nil
}

// ReadPowerWatts reads register 39134 (ACTIVE_POWER): the Solakon
// ONE's current grid export power, in watts, positive for export and
// negative for import. context deadlines are not (yet) threaded
// through to the underlying Modbus TCP call, which is synchronous and
// bounded by the client's own configured timeout instead; ctx is
// accepted only to satisfy battery.Reader and to allow a future,
// context-aware Modbus implementation.
func (r *Reader) ReadPowerWatts(_ context.Context) (float64, error) {
	data, err := r.client.ReadHoldingRegisters(regActivePower, 2)
	if err != nil {
		return 0, fmt.Errorf("solakon: read active power: %w", err)
	}
	raw, err := modbus.DecodeInt32BigWordFirst(data)
	if err != nil {
		return 0, fmt.Errorf("solakon: decode active power: %w", err)
	}
	return float64(raw), nil
}

// Close is a no-op: internal/modbus.Client opens a fresh TCP
// connection per call and does not hold a persistent one open.
func (r *Reader) Close() error { return nil }
