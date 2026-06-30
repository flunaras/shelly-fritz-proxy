// Package modbus implements the minimal subset of Modbus TCP (MBAP
// framing over a plain TCP socket) needed by internal/battery/solakon:
// Read Holding Registers (function code 0x03) and Write Multiple
// Registers (function code 0x10). It intentionally does not implement
// the rest of the Modbus function code space; this proxy only ever
// reads (and, for now, never writes) inverter registers.
//
// Every call opens a short-lived TCP connection, issues one request,
// and closes the connection. This proxy polls at most once every few
// seconds, so the overhead of a fresh TCP handshake per call is
// negligible and avoids having to reason about a long-lived
// connection's staleness or partial-write recovery, at the cost of a
// little extra latency per poll.
package modbus

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync/atomic"
	"time"
)

// Client is a synchronous Modbus TCP client for a single unit/slave ID
// at Addr (host:port). It is safe for concurrent use; each call opens
// its own connection.
type Client struct {
	Addr    string // "host:port"
	UnitID  byte
	Timeout time.Duration

	txID atomic.Uint32
}

// New builds a Client. If timeout is 0, a 5s default is used.
func New(addr string, unitID byte, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Client{Addr: addr, UnitID: unitID, Timeout: timeout}
}

// ReadHoldingRegisters issues function code 0x03 and returns the raw
// register bytes (2 bytes per register, big-endian per register, in
// wire order). quantity is the number of 16-bit registers requested
// (1-125 per the Modbus spec; this client does not enforce the limit
// beyond what a single TCP frame can carry).
func (c *Client) ReadHoldingRegisters(addr uint16, quantity uint16) ([]byte, error) {
	conn, err := net.DialTimeout("tcp", c.Addr, c.Timeout)
	if err != nil {
		return nil, fmt.Errorf("modbus: dial %s: %w", c.Addr, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(c.Timeout)); err != nil {
		return nil, fmt.Errorf("modbus: set deadline: %w", err)
	}

	txID := uint16(c.txID.Add(1))
	pdu := []byte{
		0x03, // function code
		byte(addr >> 8), byte(addr),
		byte(quantity >> 8), byte(quantity),
	}
	frame := buildFrame(txID, c.UnitID, pdu)
	if _, err := conn.Write(frame); err != nil {
		return nil, fmt.Errorf("modbus: write request: %w", err)
	}

	header := make([]byte, 7)
	if _, err := readFull(conn, header); err != nil {
		return nil, fmt.Errorf("modbus: read MBAP header: %w", err)
	}
	respTxID := binary.BigEndian.Uint16(header[0:2])
	length := binary.BigEndian.Uint16(header[4:6])
	if respTxID != txID {
		return nil, fmt.Errorf("modbus: transaction id mismatch: sent %d got %d", txID, respTxID)
	}
	if length < 2 || length > 253 {
		return nil, fmt.Errorf("modbus: implausible response length %d", length)
	}
	body := make([]byte, length-1) // length counts unit id + PDU; unit id already read as header[6]
	if _, err := readFull(conn, body); err != nil {
		return nil, fmt.Errorf("modbus: read response body: %w", err)
	}

	fc := body[0]
	if fc&0x80 != 0 {
		exCode := byte(0)
		if len(body) > 1 {
			exCode = body[1]
		}
		return nil, fmt.Errorf("modbus: exception response, function 0x%02x code %d", fc&0x7f, exCode)
	}
	if fc != 0x03 {
		return nil, fmt.Errorf("modbus: unexpected function code 0x%02x in response", fc)
	}
	if len(body) < 2 {
		return nil, fmt.Errorf("modbus: response too short")
	}
	byteCount := int(body[1])
	if len(body) < 2+byteCount {
		return nil, fmt.Errorf("modbus: response truncated: want %d data bytes, got %d", byteCount, len(body)-2)
	}
	if byteCount != int(quantity)*2 {
		return nil, fmt.Errorf("modbus: unexpected byte count %d for quantity %d", byteCount, quantity)
	}
	return body[2 : 2+byteCount], nil
}

// buildFrame wraps pdu in an MBAP header: transaction id, protocol id
// (always 0 for Modbus), length (unit id + pdu), and unit id.
func buildFrame(txID uint16, unitID byte, pdu []byte) []byte {
	frame := make([]byte, 7+len(pdu))
	binary.BigEndian.PutUint16(frame[0:2], txID)
	binary.BigEndian.PutUint16(frame[2:4], 0) // protocol id
	binary.BigEndian.PutUint16(frame[4:6], uint16(len(pdu)+1))
	frame[6] = unitID
	copy(frame[7:], pdu)
	return frame
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := conn.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// DecodeInt32BigWordFirst decodes a 32-bit signed integer spanning two
// consecutive 16-bit registers with FoxESS/Solakon big-endian *word*
// order (high word first; each individual register is itself
// big-endian, per the Modbus wire format). data must be exactly 4
// bytes, as returned by ReadHoldingRegisters(addr, 2).
func DecodeInt32BigWordFirst(data []byte) (int32, error) {
	if len(data) != 4 {
		return 0, fmt.Errorf("modbus: expected 4 bytes for int32, got %d", len(data))
	}
	hi := binary.BigEndian.Uint16(data[0:2])
	lo := binary.BigEndian.Uint16(data[2:4])
	raw := uint32(hi)<<16 | uint32(lo)
	return int32(raw), nil
}

// DecodeUint16 decodes a single 16-bit register. data must be exactly
// 2 bytes, as returned by ReadHoldingRegisters(addr, 1).
func DecodeUint16(data []byte) (uint16, error) {
	if len(data) != 2 {
		return 0, fmt.Errorf("modbus: expected 2 bytes for uint16, got %d", len(data))
	}
	return binary.BigEndian.Uint16(data), nil
}
