# executiond Configuration

`executiond` reads its configuration from environment variables only. The
repository does not load a YAML configuration file. Use `.env.example` as a
name-only reference; keep real secrets in the deployment secret store or in an
untracked local environment file.

| Variable | Required | Description |
| --- | --- | --- |
| `POLYMARKET_PRIVATE_KEY` | Yes | Wallet signing key. |
| `POLYMARKET_API_KEY` | Yes | CLOB API key. |
| `POLYMARKET_API_SECRET` | Yes | CLOB API secret. |
| `POLYMARKET_API_PASSPHRASE` | Yes | CLOB API passphrase. |
| `EXECUTION_NATS_URL` | Yes | Core NATS server URL. |
| `EXECUTION_POSTGRES_URL` | Yes | PostgreSQL connection URL. |
| `EXECUTION_POSITION_FEATURE_INTERVAL` | No | Position snapshot frequency; default `500ms`. |
| `EXECUTION_RECONCILE_INTERVAL` | No | REST reconciliation frequency; default `30s`. |
| `EXECUTION_CONNECT_TIMEOUT` | No | NATS and PostgreSQL connection timeout; default `10s`. |
| `EXECUTION_SHUTDOWN_GRACE_PERIOD` | No | Graceful shutdown deadline; default `10s`. |

Durations accept Go duration strings such as `500ms` and `30s`; positive
integer values are interpreted as milliseconds.