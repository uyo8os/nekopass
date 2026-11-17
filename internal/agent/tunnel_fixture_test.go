package agent

import (
	"fmt"
	"math/rand/v2"
	"net"
	"strings"
	"testing"
)

// The transport fixtures switch between TCP, UDP, and H3. Availability must
// be checked separately: Windows, for example, reserves different ranges for
// each protocol. Both sockets must be held while checking a candidate.
func selectTunnelTestPort(candidate func() int, attempts int) (int, error) {
	var lastErr error
	for range attempts {
		port := candidate()
		udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
		if err != nil {
			lastErr = err
			continue
		}
		tcp, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			tcp.Close()
			udp.Close()
			return port, nil
		}
		lastErr = err
		udp.Close()
	}
	return 0, fmt.Errorf("reserve a TCP and UDP tunnel test port: %w", lastErr)
}

func availableTunnelTestPort(t testing.TB) int {
	t.Helper()
	// This range is outside the usual dynamic client-port ranges, reducing
	// competition during a listener restart. Availability is still verified on
	// both protocols; no platform-specific port range is assumed to be free.
	port, err := selectTunnelTestPort(func() int { return 20000 + rand.IntN(10000) }, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// Apply reports listener errors through the next heartbeat, not its return
// value. Inspect them immediately so a bind failure cannot become a timeout.
func assertTunnelTestApplied(t testing.TB, engine *Engine) {
	t.Helper()
	engine.mu.Lock()
	syncError := engine.syncError
	engine.mu.Unlock()
	if syncError != "" {
		t.Fatalf("applying tunnel test configuration: %s", syncError)
	}
}

func TestAvailableTunnelTestPortSupportsTCPAndUDP(t *testing.T) {
	for range 10 {
		port := availableTunnelTestPort(t)
		tcp, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Fatal(err)
		}
		udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
		tcp.Close()
		if err != nil {
			t.Fatal(err)
		}
		udp.Close()
	}
}

func TestSelectTunnelTestPortSkipsProtocolConflict(t *testing.T) {
	busy, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	occupied := busy.Addr().(*net.TCPAddr).Port
	available := availableTunnelTestPort(t)
	attempt := 0
	port, err := selectTunnelTestPort(func() int {
		attempt++
		if attempt == 1 {
			return occupied
		}
		return available
	}, 2)
	if err != nil || attempt != 2 || port != available {
		t.Fatalf("expected to skip the TCP conflict: port=%d attempts=%d err=%v", port, attempt, err)
	}
}

func TestTunnelApplyReportsUDPBindConflict(t *testing.T) {
	engine, _, config, _ := h2Fixture(t, "127.0.0.1:1")
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(config.Node.TunnelListenPort)})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	config.Node.TunnelProtocol = "tls_h3"
	if err := engine.Apply(config); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(engine.syncError, "tunnel:") {
		t.Fatalf("expected tunnel bind error in heartbeat, got %q", engine.syncError)
	}
}
