package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/stefandsl/bellerophon-go/internal/config"
	bellog "github.com/stefandsl/bellerophon-go/internal/log"
)

func main() {
	fs := flag.NewFlagSet("bellerophon", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to YAML config file")
	showVersion := fs.Bool("version", false, "print version and exit")
	checkConfig := fs.Bool("check-config", false, "validate config and exit")
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
	logger.Info("bellerophon skeleton ready; runtime pipeline not yet implemented")
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
