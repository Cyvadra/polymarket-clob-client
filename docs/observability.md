# executiond observability

Everything executiond knows about its own health is on `/metrics`, `/healthz`
or in Postgres. The logs carry the detail behind an error, but no fact is only
in the logs.

The endpoints listen on `EXECUTION_METRICS_ADDR`; give every instance on a
host its own. Alert rules are in [`scripts/alerts.yml`](../scripts/alerts.yml).

## `/healthz`

Returns `200` when every check passes and `503` otherwise. Either way the body
is JSON with one entry per check:

```json
{"ok": false, "checks": {
  "nats": {"ok": true, "detail": "connected"},
  "account_stream": {"ok": false, "detail": "disconnected"},
  "reconcile": {"ok": true, "detail": "last success 2026-11-10T12:00:00Z, 20s ago (allowed 3m0s)"},
  "sweep": {"ok": true, "detail": "no success since start, 40s ago (allowed 3m0s)"}}}
```

`reconcile` and `sweep` fail once three of their intervals pass without a
successful pass. Before the modules start, every request gets a `503`.

## Metrics

| Metric | Labels | What it shows |
|---|---|---|
| `executiond_errors_total` | `component` | Errors, by the component that reported them; the log line has the detail. Every component starts at 0. |
| `executiond_last_success_timestamp_seconds` | `component` | Unix time of the last good `sweep`, `reconcile`, `equity_snapshot`, `keepwarm`, and `account_stream` message. |
| `executiond_nats_connected`, `executiond_account_stream_connected` | | 1 while the link is up. |
| `executiond_equity_usd` | | Equity in the latest recorded snapshot. |
| `executiond_sizing_base_usd` | | Equity that equity-fraction opens are sized from. |
| `executiond_drawdown`, `executiond_drawdown_limit` | `guard`, `unit` | Each guard's drawdown and its limit; `unit` is `fraction` or `usd`, and `guard` is `wallet` or a strategy prefix. |
| `executiond_opens_suspended` | `guard` | 1 while a guard holds back opens. |
| `executiond_sweeps_total` | `outcome` | Settlement sweep passes, `ok` or `failed`. |
| `executiond_sweep_lanes_total` | `result` | Lanes that sweeps checked, lost, redeemed, shrank, wrote off as dust, left to a close, or left settling. |
| `executiond_results_total` | `kind`, `strategy`, `status`, `reason_code` | Open, close and quote results sent to strategies. |
| `executiond_order_events_total` | `strategy`, `state` | Order lifecycle events sent to strategies. |
| `executiond_fills_total` | | Fills newly recorded. |
| `executiond_order_http_seconds`, `executiond_order_http_ttfb_seconds`, `executiond_order_http_cold_connections_total` | `method`, `path`, `outcome` | Order pool HTTP latency and connection reuse. |
| `executiond_order_prepost_seconds` | `kind`, `tif`, `post_only` | From request arrival to the order POST. |
| `executiond_cancel_seconds`, `executiond_cancel_batch_orders` | `batch`, `outcome` | Exchange cancels. |
| `executiond_loop_pass_seconds`, `executiond_loop_pass_overruns_total` | `loop` | Executor background loops. |
| `executiond_keepwarm_rtt_seconds`, `executiond_keepwarm_failures_total` | `pool` | Keep-warm pings. |
| `executiond_quote_levels_total`, `executiond_quote_fill_to_cancel_seconds` | `action`, `reason` | Quote lane level churn and how fast a fill cancels the remaining levels. |

### Queries

```promql
# Which component is failing, and how often
sum by (component) (rate(executiond_errors_total[5m])) > 0

# Seconds since each component last succeeded
time() - executiond_last_success_timestamp_seconds

# Share of opens refused, by reason
sum by (reason_code) (rate(executiond_results_total{kind="open",status="FAILED"}[1h]))
  / ignoring(reason_code) group_left sum(rate(executiond_results_total{kind="open"}[1h]))

# How close each guard is to its drawdown limit
executiond_drawdown / executiond_drawdown_limit

# Order POST p90
histogram_quantile(0.9, sum by (le) (rate(executiond_order_http_seconds_bucket{method="POST"}[10m])))
```

## Tables

Next to the trading tables (`order_intents`, `orders`, `order_events`,
`fills`, `positions`, `equity_snapshots`, `settlement_payouts`,
`quote_lanes`), migration 000013 adds:

| Table | One row per |
|---|---|
| `daemon_runs` | executiond process: start and stop times, build revision, signer, maker, strategies, and non-secret settings in `config` |
| `settlement_sweeps` | sweep pass that changed something or failed |
| `risk_events` | risk or sizing decision. `kind` is `suspend`, `resume`, `sizing_base`, `equity_recorded` or `snapshot_deferred`, and `subject` is the guard (`wallet` or a prefix) |
| `v_intents_daily` (view) | day, strategy, kind and status, with the number of intents |

`settlement_sweeps` and `risk_events` reference `daemon_runs.run_id`, so each
row can be traced to the process and settings that wrote it.

### Queries

```sql
-- Runs, newest first, with what they traded
SELECT run_id, started_at, stopped_at, version, strategies, config->'size_multipliers'
FROM daemon_runs ORDER BY run_id DESC LIMIT 10;

-- When were opens suspended or resumed, and why
SELECT at, kind, subject, reason, details FROM risk_events
WHERE kind IN ('suspend', 'resume') ORDER BY at DESC;

-- Equity history with the sizing base beside it
SELECT at, kind, details->>'equity_usd' AS equity_usd, reason FROM risk_events
WHERE kind IN ('equity_recorded', 'sizing_base') ORDER BY at DESC LIMIT 50;

-- Settlement outcomes per day
SELECT date_trunc('day', swept_at) AS day, sum(lost) AS lost, sum(redeemed) AS redeemed,
       sum(dust) AS dust, count(*) FILTER (WHERE failed) AS failed_passes
FROM settlement_sweeps GROUP BY 1 ORDER BY 1 DESC;

-- Intent outcomes per strategy, last 7 days
SELECT * FROM v_intents_daily WHERE day > now() - interval '7 days' ORDER BY day DESC, strategy;
```
