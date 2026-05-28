// Package integration_test is the live multi-provider UAT harness. Every
// test in this package gates on a per-provider env var so unit-test runs
// skip them automatically. See README.md for the gate matrix and how to
// run each scenario.
package integration_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// requireEnv skips the test cleanly unless the named env var is set to a
// truthy value (anything non-empty other than "0" / "false"). The skip
// message names the env var so a CI reviewer can opt in directly.
func requireEnv(t *testing.T, key string) {
	t.Helper()
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "", "0", "false":
		t.Skipf("skipped: set %s=1 to enable (see test/integration/README.md)", key)
	}
}

// requireBinary returns an absolute path to the bellerophon binary built
// for the host. It first tries $BELLEROPHON_BINARY, then `bin/bellerophon`
// at the repo root. If neither exists, the test is failed with a hint to
// run `make build` first.
func requireBinary(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("BELLEROPHON_BINARY"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		t.Fatalf("BELLEROPHON_BINARY=%s does not exist", p)
	}
	repo, err := repoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	candidate := filepath.Join(repo, "bin", "bellerophon")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	t.Fatalf("bellerophon binary not found at %s — run `make build` first "+
		"or set BELLEROPHON_BINARY", candidate)
	return "" // unreachable
}

// repoRoot walks up from this file to find the directory containing go.mod.
func repoRoot() (string, error) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("runtime.Caller failed")
	}
	dir := filepath.Dir(here)
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		dir = filepath.Dir(dir)
	}
	return "", errors.New("repoRoot: walked 8 levels without finding go.mod")
}

// requireSIPP fails the test if `sipp` is not in PATH.
func requireSIPP(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("sipp")
	if err != nil {
		t.Skipf("skipped: sipp not in PATH (install with `apt install sip-tester` "+
			"on Debian/Ubuntu); err=%v", err)
	}
	return path
}

// startBellerophon launches the binary as a subprocess with the given
// config + extra args. Returns the process handle and a function that
// kills it (idempotent) — callers should `defer kill()` from the test.
//
// The subprocess's combined stdout+stderr is captured to a file in
// t.TempDir() so failures show up in the test log.
func startBellerophon(t *testing.T, configPath string, extraArgs ...string) (*exec.Cmd, func()) {
	t.Helper()
	bin := requireBinary(t)

	logPath := filepath.Join(t.TempDir(), "bellerophon.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create subprocess log: %v", err)
	}

	args := append([]string{"--config", configPath}, extraArgs...)
	cmd := exec.Command(bin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("start bellerophon: %v", err)
	}

	killed := false
	kill := func() {
		if killed {
			return
		}
		killed = true
		// Best-effort SIGINT for graceful shutdown, escalating to Kill
		// after 3 s. A clean shutdown is part of what we're testing in
		// some scenarios, but the kill helper here is just for safety.
		if err := cmd.Process.Signal(os.Interrupt); err == nil {
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
		} else {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		_ = logFile.Close()
		// Always show the subprocess log on a failing test for diagnosis.
		if t.Failed() {
			contents, _ := os.ReadFile(logPath)
			t.Logf("subprocess log:\n%s", string(contents))
		}
	}
	return cmd, kill
}

// waitForRegistered polls the subprocess log until it sees a line
// containing the substring "registered as" (which all providers emit on
// successful REGISTER) or ctx times out.
func waitForRegistered(ctx context.Context, logPath string) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(10 * time.Second)
	}
	for time.Now().Before(deadline) {
		contents, err := os.ReadFile(logPath)
		if err == nil && strings.Contains(string(contents), "registered as") {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("waited %v without seeing 'registered as' in subprocess log",
		time.Until(deadline))
}
