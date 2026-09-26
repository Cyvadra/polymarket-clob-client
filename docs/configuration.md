# executiond Configuration

`executiond` reads its configuration from environment variables only. The
repository does not load a YAML configuration file. Use `.env.example` as a
name-only reference; keep real secrets in the deployment secret store or in an
untracked local environment file.

`.env.example` lists only the variables that require a deployment decision.
Everything else on this page has a working default and is documented here for
tuning and troubleshooting.

## Required: the signing key

`executiond` needs a wallet signing key for L1 auth and order signing. Supply it
in exactly one of two ways; setting both is an error.

| Variable | Description |
| --- | --- |
| `POLYMARKET_PRIVATE_KEY_FILE` | Path to the sealed key file (created by `polykey`) holding the key encrypted at rest. **Preferred for deployments.** |
| `POLYMARKET_PRIVATE_KEY_PASSPHRASE_FILE` | Path to the file holding the passphrase for the above. Required with `_FILE`. |
| `POLYMARKET_PRIVATE_KEY_PASSPHRASE` | Inline passphrase, an alternative to `_PASSPHRASE_FILE` for local development and CI. |
| `POLYMARKET_PRIVATE_KEY` | The raw hex key. Convenient for development; avoid on a deployed host, where it sits in plaintext in the env file and in the process environment. |

Both files must be mode `0600`; a group- or world-readable file is rejected at
startup rather than used, the way `ssh` refuses a loose identity file.
Decryption uses the standard (not "light") scrypt parameters, which allocate
roughly 256 MiB for about a second at startup. On a small shared host that
spike can get `executiond` OOM-killed into a restart loop, so either leave
headroom for it or re-encrypt the keystore with lighter parameters. The key
is decrypted once during startup and held only in memory — it is never written
back to disk, put into the environment, or logged. `executiond` logs the
derived signer address at startup so you can confirm which wallet was unlocked.

### Managing the encrypted key

The `polykey` command (`cmd/polykey`, shipped to `/opt/executiond/current/polykey`)
creates and inspects these files. It never takes the key or the passphrase as a
flag, so neither reaches the shell history or the process list.

```bash
# Create the passphrase file, then encrypt the key (read from stdin).
(umask 077; head -c 32 /dev/urandom | base64 > private-key.pass)
polykey encrypt --key-file private-key.json --passphrase-file private-key.pass

# Which wallet does this file hold? (no passphrase needed)
polykey inspect --key-file private-key.json

# Will the daemon be able to unlock it?
polykey verify --key-file private-key.json --passphrase-file private-key.pass

# Rotate the passphrase, keeping the same key. rekey only re-encrypts the
# keystore, so install the new passphrase afterwards or nothing will unlock it.
(umask 077; head -c 32 /dev/urandom | base64 > private-key.pass.new)
polykey rekey --key-file private-key.json \
  --passphrase-file private-key.pass --new-passphrase-file private-key.pass.new
mv private-key.pass.new private-key.pass
polykey verify --key-file private-key.json --passphrase-file private-key.pass

# Convert a plain keystore v3 file written by an older polykey. The passphrase
# is unchanged; executiond refuses the unconverted file.
polykey migrate --key-file private-key.json --passphrase-file private-key.pass
```

With no `--passphrase-file`, `polykey` prompts (twice, with confirmation) when
stdin is a terminal.

The key file is sealed to this program. Inside is a Web3 Secret Storage v3
keystore, but it is encrypted under the passphrase mixed with an app secret
compiled into the binary (HMAC-SHA256), and the whole keystore is then wrapped
in an AES-256-GCM envelope keyed from the same secret. Someone who steals both
the key file and the passphrase file still cannot recover the key with geth,
MetaMask or `eth_account`: they also need our binary and must reverse-engineer
it. This is obfuscation, not a cryptographic boundary. Anyone who has the
binary can extract the secret, so keep releases as private as the key files.
Changing the secret in `internal/keyfile/appsecret.go` makes every existing
key file unreadable.

## L2 API credentials

`executiond` derives its CLOB API credentials (key, secret, passphrase) from
the signing key at startup: it calls `GET /auth/derive-api-key` and,
only if the signer has no key yet, `POST /auth/api-key`. Deriving is
idempotent, so restarts reuse the same credentials and never create duplicates.
Do not configure them.

`POLYMARKET_API_KEY`, `POLYMARKET_API_SECRET`, and `POLYMARKET_API_PASSPHRASE`
are still honoured by `clobclient.ConfigFromEnv` for library users who hold
credentials outside of the signing key; when set, they must be set together and
they take precedence over derivation. `executiond` needs neither.

## Proxy wallet

`executiond` builds its CLOB client directly from the environment and never
calls `Client.WithResolvedProxyWallet`, so it signs as an EOA
(`signature_type=0`) unless told otherwise. If the trading funds sit in a
Polymarket proxy wallet, both variables below are required; setting only one of
them produces orders the exchange rejects.

| Variable | Default | Description |
| --- | --- | --- |
| `POLYMARKET_MAKER_ADDRESS` | signer address | Proxy wallet (Safe/deposit wallet) address that owns the position. |
| `POLYMARKET_SIGNATURE_TYPE` | `0` | `0` EOA, `1` Poly proxy, `2` Gnosis Safe, `3` Poly 1271. |

## Service endpoints and limits

| Variable | Required | Description |
| --- | --- | --- |
| `EXECUTION_NATS_URL` | No | Core NATS server URL; defaults to `nats://127.0.0.1:4222`. |
| `EXECUTION_POSTGRES_URL` | No | PostgreSQL connection URL; defaults to `postgres://user:password@127.0.0.1:5432/execution?sslmode=disable`. The default is a placeholder and must be replaced outside local development. |
| `EXECUTION_MAX_EQUITY_FRACTION` | No | Largest `target_equity_fraction` an open request may carry, in (0, 1]; defaults to `0.25`. Larger fractions are rejected with `INVALID_INTENT`. It catches unit mistakes (`3` meant as 3%), and is not a risk limit. |
| `EXECUTION_MAX_OPEN_BUY_NOTIONAL_USD` | No | Cap on the total notional of active BUY reservations, checked inside the reservation transaction. Unset means no cap; over-cap buys are rejected with `EXPOSURE_LIMIT`. |
| `POLYMARKET_PROXY_URL` | No | Absolute `http`, `https`, or `socks5` URL for outbound CLOB traffic. Unset means a direct connection. |

### Running on a separate host

`executiond` needs nothing from the strategy host but NATS. Point
`EXECUTION_NATS_URL` at the broker's LAN address and give the daemon its own
PostgreSQL — the execution store is this daemon's order and fill state, not a
database shared with the strategy side. The broker itself is the part that has
to be opened up: the strategy host publishes intents and market quotes into it,
and a broker bound to loopback is unreachable from anywhere else. The core NATS
used here carries no credentials and no TLS, so the port belongs on a trusted
LAN behind a firewall rule, never on a public interface.

Outbound internet is still required on whichever host runs the daemon: it calls
the CLOB REST API and holds the account websocket open. The account stream is
wallet-wide, so two daemons on one signing key both see every fill and each
drops the other's as unknown — run only one per wallet.

The proxy covers both the REST client and the websocket account stream, so
orders and fills always leave from the same address. `socks5` applies to REST
only; the websocket dialer supports `http`/`https` proxies, and falls back to
the process proxy environment otherwise.

Polymarket geoblocks *trading* by egress IP while leaving read endpoints open,
so a proxy in a restricted region answers `GET /ok` with 200 and `POST /order`
with 403. To check what the exchange sees:

```sh
curl -s -x "$POLYMARKET_PROXY_URL" https://ipinfo.io/json
curl -s -x "$POLYMARKET_PROXY_URL" -X POST -H 'content-type: application/json' \
  -d '{}' https://clob.polymarket.com/order
```

A 403 from the second command means the exit region is blocked regardless of
credentials.

## Tuning (defaults are production-usable)

These are not in `.env.example`. Set them only when you have a reason to
deviate from the default.

| Variable | Default | Description |
| --- | --- | --- |
| `EXECUTION_POSITION_FEATURE_INTERVAL` | `500ms` | Position snapshot publish frequency. |
| `EXECUTION_RECONCILE_INTERVAL` | `30s` | REST reconciliation frequency. |
| `EXECUTION_SETTLEMENT_SWEEP_INTERVAL` | `1m` | How often lanes in resolved markets are checked and emptied once their shares are worth nothing. |
| `EXECUTION_MISSING_ORDER_GRACE_PERIOD` | `2m` | How long an unresolved submitted order may return REST 404 before it is failed. |
| `EXECUTION_RECONCILE_MAX_TRADE_AGE` | `24h` | How far back trade replay looks on each reconciliation pass. Trades older than this are skipped rather than replayed. |
| `EXECUTION_RESULT_PRICE_WAIT` | `3500ms` | How long a terminal open or close result waits for the fills that price it before publishing. The order message that ends an order can arrive ahead of its trade messages; if the fills still have not landed, the result carries the order's own limit price as `average_price`. `0` does not wait: the result is priced at once from whatever fills are already recorded, with the same fallback. |
| `EXECUTION_BALANCE_CACHE_TTL` | `5s` | How long a USDC balance read from the CLOB is reused by the equity valuation. `0` reads on every query. |
| `EXECUTION_EQUITY_MAX_QUOTE_AGE` | `30s` | A position whose latest PMM quote is older than this is valued at its entry price instead of the best bid, and counted in `unmarked_positions`. `0` accepts any age. |
| `EXECUTION_CONNECT_TIMEOUT` | `10s` | NATS and PostgreSQL connection timeout. |
| `EXECUTION_SHUTDOWN_GRACE_PERIOD` | `10s` | Graceful shutdown deadline. |

Durations accept Go duration strings such as `500ms` and `30s`; positive
integer values are interpreted as milliseconds. All of them must be positive or
startup fails, except `EXECUTION_RESULT_PRICE_WAIT`, `EXECUTION_BALANCE_CACHE_TTL`, and
`EXECUTION_EQUITY_MAX_QUOTE_AGE`, which may also be `0`.

## Advanced client variables

`ConfigFromEnv` also reads the variables below. They are not needed by a
standard `executiond` deployment and are omitted from `.env.example`.

| Variable | Default | Description |
| --- | --- | --- |
| `POLYMARKET_HOST` | `https://clob.polymarket.com` | CLOB REST base URL. |
| `POLYMARKET_CHAIN_ID` | `137` | `137` Polygon mainnet, `80002` Amoy testnet. |
| `POLYMARKET_RPC_ENDPOINT` | `https://polygon-rpc.com` on mainnet | Only used as the on-chain fallback inside `Client.ResolveProxyWallet`, which `executiond` never calls. Relevant to library callers, not to the service. |
| `POLYMARKET_BUILDER_API_KEY` / `_SECRET` / `_PASSPHRASE` | unset | Builder credentials for fee-sharing order headers. Must be set together, or not at all. Only relevant under a builder agreement. |

## Notes

- `cmd/executiontest` is configured through command-line flags, not this file.
  It reads a single environment variable, `EXECUTION_NATS_URL`, as the default
  for `--nats-url`. See [execution-integration-test.md](execution-integration-test.md).
- A `config.yaml` may be present in the working tree. It belongs to an earlier
  version of the system, is gitignored, and is not read by any code in this
  repository. It is kept only for reference and must not be treated as live
  configuration.
