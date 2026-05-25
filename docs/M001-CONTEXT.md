# M001 — Context

Working notes that accumulate across M001 slices. Authoritative for any decision the SPEC leaves implicit.

## 1. Output location decision

The Go rewrite lives in its own repository `github.com/stefandsl/bellerophon-go` (separate from `github.com/stefandsl/bellerophon`).

Rationale: cleaner CI matrix, separate release artifacts, no Node tooling in this tree. The cross-link to voice-app reference behaviour stays via `docs/` summaries written by scout passes — no source-tree coupling.

## 2. Config struct ↔ YAML key ↔ env var ↔ CLI flag mapping (S02)

Single source of truth used by:

- `internal/config/Config` field tags (`yaml`, `env`, `flag`)
- `--help` output
- `Validate()` error messages (must name field / key / env / flag together so any of the four override sources can be located)

Env-var names mirror `voice-app/.env.example` where a 1:1 concept exists. New fields use the `BELLEROPHON_` prefix to avoid collisions.

| Go field                        | YAML key                          | Env var                  | CLI flag                      | Required | Notes                                                                 |
|---------------------------------|-----------------------------------|--------------------------|-------------------------------|----------|-----------------------------------------------------------------------|
| `SIP.Domain`                    | `sip.domain`                      | `SIP_DOMAIN`             | `--sip.domain`                | yes      | 3CX server FQDN.                                                      |
| `SIP.Registrar`                 | `sip.registrar`                   | `SIP_REGISTRAR`          | `--sip.registrar`             | yes      | Registrar host (IP or FQDN).                                          |
| `SIP.RegistrarPort`             | `sip.registrar_port`              | `SIP_REGISTRAR_PORT`     | `--sip.registrar-port`        | no       | Default `5060`.                                                       |
| `SIP.Extension`                 | `sip.extension`                   | `SIP_EXTENSION`          | `--sip.extension`             | yes      | Numeric extension to register as.                                     |
| `SIP.AuthUsername`              | `sip.auth_username`               | `SIP_AUTH_USERNAME`      | `--sip.auth-username`         | no       | Defaults to `Extension` if empty.                                     |
| `SIP.AuthID`                    | `sip.auth_id`                     | `SIP_AUTH_ID`            | `--sip.auth-id`               | no       | 3CX's `AuthID` field; many tenants accept empty.                      |
| `SIP.AuthPassword`              | `sip.auth_password`               | `SIP_PASSWORD`           | `--sip.auth-password`         | yes      | Env name matches voice-app `SIP_PASSWORD`.                            |
| `SIP.Expiry`                    | `sip.expiry`                      | `SIP_EXPIRY`             | `--sip.expiry`                | no       | Seconds; default `300`; must be `> 0`.                                |
| `RTP.ExternalIP`                | `rtp.external_ip`                 | `EXTERNAL_IP`            | `--rtp.external-ip`           | yes      | Public LAN IP advertised in SDP. Voice-app calls it `EXTERNAL_IP`.    |
| `RTP.PortRange`                 | `rtp.port_range`                  | `RTP_PORT_RANGE`         | `--rtp.port-range`            | no       | Format `"min-max"`, default `30000-30100`. Avoids 3CX SBC 20000-20099.|
| `HTTP.Port`                     | `http.port`                       | `HTTP_PORT`              | `--http.port`                 | no       | Default `3000`. `0` disables admin HTTP.                              |
| `HTTP.TLSPort`                  | `http.tls_port`                   | `TLS_PORT`               | `--http.tls-port`             | no       | Default `0` (disabled).                                               |
| `HTTP.TLSCert`                  | `http.tls_cert`                   | `TLS_CERT_FILE`          | `--http.tls-cert`             | no       | Required iff `TLSPort > 0`.                                           |
| `HTTP.TLSKey`                   | `http.tls_key`                    | `TLS_KEY_FILE`           | `--http.tls-key`              | no       | Required iff `TLSPort > 0`.                                           |
| `Logging.Level`                 | `logging.level`                   | `LOG_LEVEL`              | `--logging.level`             | no       | `debug\|info\|warn\|error`, default `info`.                            |
| `Logging.Format`                | `logging.format`                  | `LOG_FORMAT`             | `--logging.format`            | no       | `text\|json`, default `text`.                                          |

Defaults are applied **before** the env/flag overlays. Empty string ⇒ "not set" (overlay does nothing). Required fields are validated **after** all three layers have been merged.

## 3. Override precedence (S02)

Lowest-to-highest. Each layer overlays only the fields it provides; absent fields keep the value from the lower layer.

1. **Built-in defaults** (`config.Defaults()`).
2. **YAML file** (`--config path.yaml`). If omitted, the loader skips this layer entirely (used by `--check-config -` in test code).
3. **Environment variables** (table above).
4. **CLI flags** (table above; `flag.Visit` is used so only explicitly-set flags overlay — zero-valued defaults from `flag.Int` etc. do not stomp lower layers).

## 4. Validation contract (S02)

`Validate() error` returns a single error that lists every problem with one line per problem in the form:

```
sip.extension (env SIP_EXTENSION, flag --sip.extension) is required
```

This gives the operator enough information to fix it from any input source without grepping docs.

Warnings (e.g. "password is the example value") are returned separately from `Validate()` so `--check-config` can report them without failing the boot.

## 5. Reference behaviour from voice-app (scout output cache)

(Populated by scout subagents as later slices need it. Empty for now — S02 only needs env-name parity.)
