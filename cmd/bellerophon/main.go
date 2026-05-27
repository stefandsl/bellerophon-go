package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/stefandsl/bellerophon-go/internal/config"
	bellog "github.com/stefandsl/bellerophon-go/internal/log"
	"github.com/stefandsl/bellerophon-go/internal/sipua"
)

func main() {
	fs := flag.NewFlagSet("bellerophon", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to YAML config file")
	showVersion := fs.Bool("version", false, "print version and exit")
	checkConfig := fs.Bool("check-config", false, "validate config and exit")
	sipListen := fs.String("sip.listen", "0.0.0.0:5060", "local SIP UA bind address (host:port)")
	echoMode := fs.Bool("echo-mode", false, "answer inbound calls with an RTP echo loop (M001/S04 demo)")
	config.RegisterFlags(fs)

	if err := fs.Parse(os.Args[1:]); err != nil {
		if err == flag.ErrHelp {
			return
		}
		os.Exit(2)
	}

	if *showVersion {
		fmt.Printf("bellerophon %s (commit %s, built %s)\n", version, commit, buildDate)
		return
	}

	cfg, err := config.Load(config.LoadOptions{
		Path:    *configPath,
		FlagSet: fs,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	warnings, verr := cfg.Validate()

	if *checkConfig {
		for _, w := range warnings {
			fmt.Fprintf(os.Stderr, "warn: %s\n", w)
		}
		if verr != nil {
			fmt.Fprintln(os.Stderr, verr)
			os.Exit(1)
		}
		fmt.Println("config OK")
		printResolved(cfg)
		return
	}

	if verr != nil {
		fmt.Fprintln(os.Stderr, verr)
		os.Exit(1)
	}

	logger := bellog.New(cfg.Logging.Level, cfg.Logging.Format)
	for _, w := range warnings {
		logger.Warn("config", "issue", w)
	}
	logger.Info("config loaded",
		"sip_domain", cfg.SIP.Domain,
		"sip_extension", cfg.SIP.Extension,
		"rtp_external_ip", cfg.RTP.ExternalIP,
		"rtp_port_range", cfg.RTP.PortRange,
	)
	srv, err := sipua.NewServer(cfg.SIP, sipua.Options{
		LocalAddr: *sipListen,
		Logger:    logger,
	})
	if err != nil {
		logger.Error("sip ua init failed", "error", err)
		os.Exit(1)
	}
	if *echoMode {
		logger.Info("echo mode enabled — inbound calls will be answered with an RTP echo loop")
		srv.OnInvite(echoHandler(cfg, logger))
	} else {
		srv.OnInvite(func(c *sipua.Call) {
			// S03 stub: accept the call with no media. The echo handler
			// (above, under --echo-mode) wires real SDP/RTP per S04.
			logger.Info("invite accepted (S03 stub: no media)", "call_id", c.CallID)
			c.OnBye(func() {
				logger.Info("call ended", "call_id", c.CallID)
			})
			if err := c.Reply(200, "OK", stubSDP(cfg.RTP.ExternalIP)); err != nil {
				logger.Error("reply 200 failed", "error", err)
			}
		})
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logger.Info("starting sip ua", "listen", *sipListen)
	if err := srv.Run(ctx); err != nil {
		logger.Error("sip ua exited with error", "error", err)
		os.Exit(1)
	}
	logger.Info("bellerophon shutdown complete")
}

// stubSDP returns a minimal session description so the 200 OK has a body.
// S04 replaces this with a real PCMU/PCMA offer/answer tied to an RTP socket.
func stubSDP(externalIP string) []byte {
	ip := externalIP
	if ip == "" {
		ip = "127.0.0.1"
	}
	return []byte("v=0\r\n" +
		"o=bellerophon 0 0 IN IP4 " + ip + "\r\n" +
		"s=bellerophon\r\n" +
		"c=IN IP4 " + ip + "\r\n" +
		"t=0 0\r\n" +
		"m=audio 30000 RTP/AVP 0 8\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n" +
		"a=rtpmap:8 PCMA/8000\r\n" +
		"a=ptime:20\r\n" +
		"a=sendrecv\r\n")
}

func printResolved(c config.Config) {
	fmt.Println("resolved config:")
	fmt.Printf("  sip.domain         = %q\n", c.SIP.Domain)
	fmt.Printf("  sip.registrar      = %q (port %d)\n", c.SIP.Registrar, c.SIP.RegistrarPort)
	fmt.Printf("  sip.extension      = %q\n", c.SIP.Extension)
	fmt.Printf("  sip.auth_username  = %q\n", c.SIP.AuthUsernameOrExtension())
	fmt.Printf("  sip.expiry         = %d\n", c.SIP.Expiry)
	fmt.Printf("  rtp.external_ip    = %q\n", c.RTP.ExternalIP)
	fmt.Printf("  rtp.port_range     = %q\n", c.RTP.PortRange)
	fmt.Printf("  http.port          = %d\n", c.HTTP.Port)
	fmt.Printf("  http.tls_port      = %d\n", c.HTTP.TLSPort)
	fmt.Printf("  logging.level      = %q\n", c.Logging.Level)
	fmt.Printf("  logging.format     = %q\n", c.Logging.Format)
}
