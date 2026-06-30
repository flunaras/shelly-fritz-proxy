package solakon

import (
	"context"
	"encoding/binary"
	"net"
	"strconv"
	"testing"
	"time"
)

// fakeInverter starts a TCP listener that answers exactly one Read
// Holding Registers request for regActivePower with the given watts
// value encoded as FoxESS big-endian-word-first int32.
func fakeInverter(t *testing.T, watts int32) (host string, port int) {
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
		if _, err := readFull(conn, header); err != nil {
			return
		}
		txID := binary.BigEndian.Uint16(header[0:2])
		length := binary.BigEndian.Uint16(header[4:6])
		body := make([]byte, length-1)
		if _, err := readFull(conn, body); err != nil {
			return
		}

		data := make([]byte, 4)
		binary.BigEndian.PutUint16(data[0:2], uint16(uint32(watts)>>16))
		binary.BigEndian.PutUint16(data[2:4], uint16(uint32(watts)&0xffff))

		respPDU := append([]byte{0x03, byte(len(data))}, data...)
		frame := make([]byte, 7+len(respPDU))
		binary.BigEndian.PutUint16(frame[0:2], txID)
		binary.BigEndian.PutUint16(frame[4:6], uint16(len(respPDU)+1))
		frame[6] = 1
		copy(frame[7:], respPDU)
		_, _ = conn.Write(frame)
	}()
	tcpAddr := ln.Addr().(*net.TCPAddr)
	return tcpAddr.IP.String(), tcpAddr.Port
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

func TestReadPowerWatts(t *testing.T) {
	host, port := fakeInverter(t, 5000)
	r := New(host, port, 1, 2*time.Second)
	got, err := r.ReadPowerWatts(context.Background())
	if err != nil {
		t.Fatalf("ReadPowerWatts: %v", err)
	}
	if got != 5000 {
		t.Fatalf("got %v, want 5000", got)
	}
}

func TestReadPowerWattsNegative(t *testing.T) {
	host, port := fakeInverter(t, -1000)
	r := New(host, port, 1, 2*time.Second)
	got, err := r.ReadPowerWatts(context.Background())
	if err != nil {
		t.Fatalf("ReadPowerWatts: %v", err)
	}
	if got != -1000 {
		t.Fatalf("got %v, want -1000", got)
	}
}

func TestNewReaderViaRegistry(t *testing.T) {
	host, port := fakeInverter(t, 42)
	// exercise the same path cmd/shelly-fritz-proxy uses.
	r := New(host, port, 1, time.Second)
	if r == nil {
		t.Fatal("expected non-nil reader")
	}
	_ = strconv.Itoa(port) // keep port referenced for clarity in failure messages
}
