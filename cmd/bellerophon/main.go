package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/stefandsl/bellerophon-go/internal/config"
	bellog "github.com/stefandsl/bellerophon-go/internal/log"
)

func main() {
	var (
		configPath  = flag.String("config", "", "path to YAML config file")
		showVersion = flag.Bool("version", false, "print version and exit")
		checkConfig = flag.Bool("check-config", false, "validate config and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("bellerophon %s (commit %s, built %s)\n", version, commit, buildDate)
		return
	}

	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "error: --config is required (or pass --version)")
		flag.Usage()
		os.Exit(2)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	errs, warns := cfg.Validate()

	if *checkConfig {
		for _, w := range warns {
			fmt.Fprintf(os.Stderr, "warn: %s\n", w)
		}
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "error: %s\n", e)
		}
		if len(errs) > 0 {
			os.Exit(1)
		}
		fmt.Println("config OK")
		return
	}

	if len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "error: %s\n", e)
		}
		os.Exit(1)
	}

	logger := bellog.New(cfg.Debug.LogLevel)
	for _, w := range warns {
		logger.Warn("config", "issue", w)
	}
	logger.Info("config loaded",
		"sip_server", cfg.SIP.Server,
		"sip_user", cfg.SIP.Username,
		"rtp_codec", cfg.RTP.PreferredCodec,
	)

	// Slices S03+ wire the SIP / RTP / voice pipeline here.
	logger.Info("bellerophon skeleton ready; runtime pipeline not yet implemented")
}
