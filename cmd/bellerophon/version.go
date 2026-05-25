package main

// Set via -ldflags at build time.
// See Makefile target `build` for the canonical flags.
var (
	version   = "0.1.0-alpha"
	commit    = "none"
	buildDate = "unknown"
)
