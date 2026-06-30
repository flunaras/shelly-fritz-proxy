// Package mdns publishes a minimal multicast-DNS / DNS-SD service record
// so Shelly-aware clients (including the Solakon ONE) discover this proxy
// as a real Shelly device on the local network.
//
// The implementation is intentionally minimal: only A and PTR/SRV/TXT
// records for the well-known Shelly service types are advertised, and we
// only answer queries (no unsolicited announcements). For most home LANs
// that is enough; if you need full conformance, drop in a battle-tested
// library like github.com/grandcat/zeroconf and remove this file.
package mdns

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	mdnsAddr    = "224.0.0.251:5353"
	mdnsPort    = 5353
	defaultTTL  = 120 // seconds
)

// Service describes what we want to announce.
type Service struct {
	// InstanceName is the human-visible Bonjour name (e.g. "shellypro3em-AABBCCDDEEFF").
	InstanceName string
	// HostName is the FQDN we claim on the .local domain, without the
	// trailing dot (e.g. "shellypro3em-AABBCCDDEEFF.local").
	HostName string
	// IP is the IPv4 address that resolves to HostName.
	IP net.IP
	// HTTPPort is the port the Shelly RPC server is listening on (usually 80).
	HTTPPort uint16
	// TXT holds the DNS-SD TXT key/value pairs (e.g. "gen=2", "id=...").
	TXT []string
	// Logger receives operational messages.
	Logger *slog.Logger
}

// Responder listens on the mDNS multicast group and answers queries for
// the configured service. Run blocks until ctx is cancelled.
type Responder struct {
	svc Service
}

func NewResponder(svc Service) *Responder { return &Responder{svc: svc} }

// Run binds to the multicast group and answers queries forever.
func (r *Responder) Run(ctx context.Context) error {
	if r.svc.IP.To4() == nil {
		return errors.New("mdns: only IPv4 is supported")
	}
	addr, err := net.ResolveUDPAddr("udp4", mdnsAddr)
	if err != nil {
		return err
	}
	iface, err := pickInterface(r.svc.IP)
	if err != nil {
		return err
	}
	conn, err := net.ListenMulticastUDP("udp4", iface, addr)
	if err != nil {
		return fmt.Errorf("mdns: listen: %w", err)
	}
	defer conn.Close()
	_ = conn.SetReadBuffer(64 * 1024)

	r.svc.Logger.Info("mdns responder up",
		"iface", iface.Name,
		"ip", r.svc.IP,
		"host", r.svc.HostName,
		"instance", r.svc.InstanceName,
	)
	// Send a gratuitous announcement so clients learn about us quickly.
	if reply, err := r.buildAnnouncement(); err == nil {
		_, _ = conn.WriteToUDP(reply, addr)
	}

	buf := make([]byte, 65535)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			r.svc.Logger.Warn("mdns: read failed", "err", err)
			continue
		}
		r.handle(conn, src, buf[:n])
	}
}

func pickInterface(ip net.IP) (*net.Interface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for i := range ifaces {
		iface := &ifaces[i]
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && ipn.IP.Equal(ip) {
				return iface, nil
			}
		}
	}
	return nil, fmt.Errorf("no interface owns %s", ip)
}

func (r *Responder) handle(conn *net.UDPConn, src *net.UDPAddr, raw []byte) {
	var msg dnsmessage.Message
	if err := msg.Unpack(raw); err != nil {
		return
	}
	if msg.Header.Response {
		return
	}
	matched := false
	for _, q := range msg.Questions {
		name := strings.ToLower(q.Name.String())
		switch {
		case name == "_http._tcp.local." ||
			name == "_shelly._tcp.local.":
			matched = true
		case name == strings.ToLower(r.svc.HostName)+".":
			matched = true
		case name == strings.ToLower(r.svc.InstanceName)+"._http._tcp.local." ||
			name == strings.ToLower(r.svc.InstanceName)+"._shelly._tcp.local.":
			matched = true
		}
	}
	if !matched {
		return
	}
	reply, err := r.buildAnnouncement()
	if err != nil {
		r.svc.Logger.Warn("mdns: build reply", "err", err)
		return
	}
	// Reply to the multicast group (unicast replies are an option but
	// some Shelly clients only re-listen on multicast).
	if _, err := conn.WriteToUDP(reply, &net.UDPAddr{IP: net.ParseIP("224.0.0.251"), Port: mdnsPort}); err != nil {
		r.svc.Logger.Warn("mdns: write reply", "err", err)
	}
}

func (r *Responder) buildAnnouncement() ([]byte, error) {
	msg := dnsmessage.Message{
		Header: dnsmessage.Header{Response: true, Authoritative: true},
	}
	host, _ := dnsmessage.NewName(r.svc.HostName + ".")
	addRR := func(name string, ttl uint32, body dnsmessage.ResourceBody) {
		n, err := dnsmessage.NewName(name)
		if err != nil {
			return
		}
		msg.Answers = append(msg.Answers, dnsmessage.Resource{
			Header: dnsmessage.ResourceHeader{
				Name:  n,
				Class: dnsmessage.ClassINET,
				TTL:   ttl,
			},
			Body: body,
		})
	}
	// A record
	var ip4 [4]byte
	copy(ip4[:], r.svc.IP.To4())
	addRR(r.svc.HostName+".", defaultTTL, &dnsmessage.AResource{A: ip4})

	for _, svcType := range []string{"_http._tcp.local.", "_shelly._tcp.local."} {
		instance := r.svc.InstanceName + "." + svcType
		addRR(svcType, defaultTTL, &dnsmessage.PTRResource{PTR: mustName(instance)})
		addRR(instance, defaultTTL, &dnsmessage.SRVResource{
			Priority: 0, Weight: 0, Port: r.svc.HTTPPort, Target: host,
		})
		// TXT
		var txt dnsmessage.TXTResource
		for _, kv := range r.svc.TXT {
			txt.TXT = append(txt.TXT, kv)
		}
		if len(txt.TXT) == 0 {
			txt.TXT = []string{""}
		}
		addRR(instance, defaultTTL, &txt)
	}
	return msg.Pack()
}

func mustName(s string) dnsmessage.Name {
	n, _ := dnsmessage.NewName(s)
	return n
}
