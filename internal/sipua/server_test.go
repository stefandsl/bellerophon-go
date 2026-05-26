package sipua

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"github.com/stefandsl/bellerophon-go/internal/config"
	bellog "github.com/stefandsl/bellerophon-go/internal/log"
)

func TestNewServerRequiresLogger(t *testing.T) {
	_, err := NewServer(config.SIP{Extension: "100"}, Options{})
	if err == nil {
		t.Fatal("expected error when Logger is nil")
	}
}

func TestContactURIIncludesExtension(t *testing.T) {
	s, err := NewServer(config.SIP{Extension: "777"}, Options{
		LocalAddr: "1.2.3.4:5070",
		Logger:    bellog.New("error", "text"),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	got := s.contactURI()
	if !strings.Contains(got, "777") || !strings.Contains(got, "1.2.3.4") || !strings.Contains(got, "5070") {
		t.Errorf("contactURI missing components: %s", got)
	}
}

func TestFirstNonLoopbackIPReturnsAnything(t *testing.T) {
	ip := firstNonLoopbackIP()
	if ip == "" {
		t.Fatal("expected non-empty fallback IP")
	}
}

// TestRegisterAgainstMockRegistrar starts a sipgo-based registrar on an
// ephemeral UDP port that always responds 200 OK, then drives our Server's
// initial REGISTER through it. This exercises the request/response path
// without requiring a live 3CX.
func TestRegisterAgainstMockRegistrar(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}
	// sipgo v1.3.1's ListenAndServe races between its main goroutine and its
	// internal context-cancel watcher when starting and stopping a UDP
	// listener back-to-back. The race is upstream; gate the integration
	// test until a fix or a workaround lands. Run with
	// BELLEROPHON_SIPUA_NET=1 to opt in.
	if os.Getenv("BELLEROPHON_SIPUA_NET") == "" {
		t.Skip("set BELLEROPHON_SIPUA_NET=1 to run network integration test")
	}

	regAddr, registers, stopReg := startMockRegistrar(t)
	defer stopReg()

	srvAddr := pickFreeUDPAddr(t)

	host, port, _ := net.SplitHostPort(regAddr)
	portInt := 0
	for _, c := range port {
		portInt = portInt*10 + int(c-'0')
	}

	cfg := config.SIP{
		Domain:        host,
		Registrar:     host,
		RegistrarPort: portInt,
		Extension:     "alice",
		AuthUsername:  "alice",
		AuthPassword:  "secret",
		Expiry:        60,
	}

	s, err := NewServer(cfg, Options{
		LocalAddr: srvAddr,
		Logger:    bellog.New("error", "text"),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()

	// Wait for a REGISTER to arrive at the mock.
	deadline := time.After(3 * time.Second)
	for atomic.LoadInt32(registers) == 0 {
		select {
		case <-deadline:
			t.Fatal("no REGISTER seen at mock registrar")
		case <-time.After(50 * time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("Run returned: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Server.Run did not return after cancel")
	}
}

// startMockRegistrar starts a sipgo Server that responds 200 OK to every
// REGISTER. It returns the bound address and a counter of REGISTERs seen.
func startMockRegistrar(t *testing.T) (string, *int32, func()) {
	t.Helper()
	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatalf("mock ua: %v", err)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatalf("mock server: %v", err)
	}

	var count int32
	srv.OnRegister(func(req *sip.Request, tx sip.ServerTransaction) {
		atomic.AddInt32(&count, 1)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})

	addr := pickFreeUDPAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.ListenAndServe(ctx, "udp", addr) }()
	// Give the listener time to bind.
	time.Sleep(100 * time.Millisecond)

	stop := func() {
		cancel()
		_ = srv.Close()
	}
	return addr, &count, stop
}

func pickFreeUDPAddr(t *testing.T) string {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := c.LocalAddr().String()
	_ = c.Close()
	return addr
}
