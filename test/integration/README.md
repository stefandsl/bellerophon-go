# test/integration — live multi-provider tests

The tests in this directory exercise the full SIP/RTP path against **real**
registrars: a dockerized Asterisk, MessageNet (Italian ITSP), and 3CX. They
are intentionally gated by environment variables so unit-test runs
(`go test ./...`) skip them with a clear message and finish quickly.

## Gates

| File | Env var to enable |
|---|---|
| `live_generic_test.go` | `BELLEROPHON_LIVE_GENERIC=1` |
| `live_messagenet_test.go` | `BELLEROPHON_LIVE_MESSAGENET=1` |
| `live_3cx_test.go` | `BELLEROPHON_LIVE_3CX=1` |

A missing gate → `t.Skip()`. The gate variable name is also echoed in the
skip message so a CI reviewer can see exactly what to set.

## Tooling required when enabled

- **`sipp`** in PATH (`apt install sip-tester` on Debian/Ubuntu).
- For `live_generic_test.go`: Docker + the bundled `docker-compose.asterisk.yml`
  (or any Asterisk instance reachable on the host network — pass its
  address via `ASTERISK_HOST` / `ASTERISK_PORT`).
- For the ITSP tests: a sip account with at least one DID you can call.

See `docs/m001-uat.md` for the matching **manual** UAT script.

## Running

```sh
# Just the always-skipped sanity (verifies the scaffolding compiles):
go test -v ./test/integration/...

# Full run against the dockerized Asterisk:
docker compose -f test/integration/docker-compose.asterisk.yml up -d
BELLEROPHON_LIVE_GENERIC=1 go test -v ./test/integration/...

# Full run including MessageNet (requires real account credentials):
MESSAGENET_USER=... MESSAGENET_PASS=... MESSAGENET_DID=... \
  BELLEROPHON_LIVE_MESSAGENET=1 go test -v ./test/integration/...

# Same for 3CX:
THREECX_DOMAIN=... THREECX_EXTENSION=... THREECX_PASSWORD=... \
  BELLEROPHON_LIVE_3CX=1 go test -v ./test/integration/...
```

## What each test asserts

All three follow the same shape (REGISTER → INVITE → RTP echo → BYE) so
a provider quirk that breaks one and not the other points straight at
`internal/sipprov/<provider>.go`:

1. Boot a `bellerophon --echo-mode` subprocess pointed at the provider.
2. Wait for the `[sip] registered ...` log line within 10 s.
3. Use `sipp` (the canonical SIP load tester) to place an INVITE with
   a 5-second G.711 sine payload.
4. Verify `sipp` reports the call as answered, the 200 OK has a
   negotiable SDP, and the echoed RTP arrives within ±50 ms of
   500 ms latency.
5. Send `Ctrl-C` to the subprocess and verify it unregisters and exits
   within 2 s.

Failures are diagnosed by dumping the subprocess log and the `sipp`
PCAP into `t.TempDir()` so the next reviewer can replay them.
