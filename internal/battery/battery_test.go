package battery

import (
	"context"
	"testing"
)

func TestRegisterAndNew(t *testing.T) {
	const typeName = "test-stub-type"
	Register(typeName, func(cfg DeviceConfig) (Reader, error) {
		return fakeReader{host: cfg.Host}, nil
	})

	r, err := New(DeviceConfig{Type: "Test-Stub-Type", Host: "example.invalid"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fr, ok := r.(fakeReader)
	if !ok {
		t.Fatalf("unexpected reader type %T", r)
	}
	if fr.host != "example.invalid" {
		t.Fatalf("got host %q, want example.invalid", fr.host)
	}
}

func TestNewUnknownType(t *testing.T) {
	_, err := New(DeviceConfig{Type: "does-not-exist", Host: "x"})
	if err == nil {
		t.Fatal("expected error for unknown type")
	}
}

type fakeReader struct{ host string }

func (f fakeReader) ReadPowerWatts(_ context.Context) (float64, error) { return 42, nil }
func (f fakeReader) Close() error                                      { return nil }
