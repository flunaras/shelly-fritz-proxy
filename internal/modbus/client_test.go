package modbus

import (
	"encoding/binary"
	"net"
	"testing"
	"time"
)

func TestDecodeInt32BigWordFirst(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want int32
	}{
		{"positive", []byte{0x00, 0x00, 0x13, 0x88}, 5000},
		{"negative", []byte{0xff, 0xff, 0xfc, 0x18}, -1000},
		{"zero", []byte{0x00, 0x00, 0x00, 0x00}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeInt32BigWordFirst(tt.data)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDecodeInt32BigWordFirstWrongLength(t *testing.T) {
	if _, err := DecodeInt32BigWordFirst([]byte{0x00, 0x01}); err == nil {
		t.Fatal("expected error for wrong length")
	}
}

func TestDecodeUint16(t *testing.T) {
	got, err := DecodeUint16([]byte{0x00, 0x5a})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 90 {
		t.Fatalf("got %d, want 90", got)
	}
	if _, err := DecodeUint16([]byte{0x00}); err == nil {
		t.Fatal("expected error for wrong length")
	}
}

// fakeModbusServer starts a TCP listener that answers exactly one Read
// Holding Registers (0x03) request with the given register bytes, then
// closes. Used to test Client.ReadHoldingRegisters end-to-end without a
// real device.
func fakeModbusServer(t *testing.T, unitID byte, registerBytes []byte) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		defer ln.Close()

		header := make([]byte, 7)
		if _, err := readFullTest(conn, header); err != nil {
			return
		}
		txID := binary.BigEndian.Uint16(header[0:2])
		length := binary.BigEndian.Uint16(header[4:6])
		body := make([]byte, length-1)
		if _, err := readFullTest(conn, body); err != nil {
			return
		}
		// body: function code(1) + addr(2) + qty(2)

		respPDU := append([]byte{0x03, byte(len(registerBytes))}, registerBytes...)
		respFrame := buildFrame(txID, unitID, respPDU)
		_, _ = conn.Write(respFrame)
	}()
	return ln.Addr().String()
}

func readFullTest(conn net.Conn, buf []byte) (int, error) {
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

func TestClientReadHoldingRegisters(t *testing.T) {
	addr := fakeModbusServer(t, 1, []byte{0x00, 0x00, 0x13, 0x88}) // 5000
	c := New(addr, 1, 2*time.Second)
	data, err := c.ReadHoldingRegisters(39134, 2)
	if err != nil {
		t.Fatalf("ReadHoldingRegisters: %v", err)
	}
	got, err := DecodeInt32BigWordFirst(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != 5000 {
		t.Fatalf("got %d, want 5000", got)
	}
}
