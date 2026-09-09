# executiond Configuration

`executiond` reads its configuration from environment variables only. The
repository does not load a YAML configuration file. Use `.env.example` as a
name-only reference; keep real secrets in the deployment secret store or in an
untracked local environment file.

`.env.example` lists only the variables that require a deployment decision.
Everything else on this page has a working default and is documented here for
tuning and troubleshooting.

## Required

| Variable | Description |
| --- | --- |
| `POLYMARKET_PRIVATE_KEY` | Wallet signing key. Used for L1 auth and order signing. |

## L2 API credentials

`executiond` derives its CLOB API credentials (key, secret, passphrase) from
`POLYMARKET_PRIVATE_KEY` at startup: it calls `GET /auth/derive-api-key` and,
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
| `EXECUTION_MAX_OPEN_BUY_NOTIONAL_USD` | No | Cap on the total notional of active BUY reservations, checked inside the reservation transaction. Unset means no cap; over-cap buys are rejected with `EXPOSURE_LIMIT`. |
| `POLYMARKET_PROXY_URL` | No | Absolute `http`, `https`, or `socks5` URL for outbound CLOB traffic. Unset means a direct connection. |

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
| `EXECUTION_MISSING_ORDER_GRACE_PERIOD` | `2m` | How long an unresolved submitted order may return REST 404 before it is failed. |
| `EXECUTION_CONNECT_TIMEOUT` | `10s` | NATS and PostgreSQL connection timeout. |
| `EXECUTION_SHUTDOWN_GRACE_PERIOD` | `10s` | Graceful shutdown deadline. |

Durations accept Go duration strings such as `500ms` and `30s`; positive
integer values are interpreted as milliseconds. All five must be positive or
startup fails.

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
