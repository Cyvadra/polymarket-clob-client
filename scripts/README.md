# executiond deployment

`deploy.sh` installs the `executiond` execution daemon on a host that already
runs pmm and has `pm2` installed. It runs `go test`, cross-builds a release,
uploads it plus a private per-host environment file over SSH, installs an
immutable release under `/opt/executiond/releases/<timestamp>`, swaps the
`current` symlink atomically, (re)starts the `executiond` pm2 app, waits for
it to settle, rolls back on any error, and keeps the three newest releases.

## Usage

```bash
scripts/deploy.sh <ssh-host>
# or: EXECUTIOND_DEPLOY_HOST=<ssh-host> scripts/deploy.sh
```

`PMM_DEPLOY_HOST` is also honoured, so the same variable can drive both deploys.
Override the target user with `EXECUTIOND_DEPLOY_TARGET` (default `root`), the
platform with `EXECUTIOND_DEPLOY_ARCH` / `EXECUTIOND_DEPLOY_OS`, and the remote
`pm2` binary with `EXECUTIOND_PM2_BIN` (default
`/root/.nvm/versions/node/v24.21.0/bin/pm2`). Any of these can also be set
per host in `scripts/private/<ssh-host>/deploy.env` (see below) instead of
exporting them by hand.

## Layout on the target

Paths and names below are for the default instance. With
`EXECUTIOND_INSTANCE=<name>` (see below) every `executiond` in them — the
directories, the pm2 app and the system user — becomes `executiond-<name>`.

| Path | Purpose |
|---|---|
| `/opt/executiond/releases/<timestamp>` | Immutable application releases (`executiond`, `executiontest`, `polykey`, `pnlreport`) |
| `/opt/executiond/current` | Active release symlink |
| `/opt/executiond/run.sh` | Wrapper pm2 runs: sources the env file, then drops to `executiond` and execs `current/executiond` |
| `/etc/executiond/executiond.env` | Runtime environment, sourced by `run.sh`. Mode 0600, owned by `root` — `run.sh` sources it as root, so `executiond` must not be able to write it |
| `/etc/executiond/private-key.json` | Signing key, encrypted at rest (sealed keystore, see `docs/configuration.md`). Mode 0600, owned by `executiond` |
| `/etc/executiond/private-key.pass` | Passphrase for the above. Mode 0600, owned by `executiond` |
| `/var/lib/executiond` | Service working directory, owned by the unprivileged `executiond` user |

The `executiond` pm2 app always runs `run.sh` as root — needed to read the
`pm2`/`node` binaries under `/root` and to `runuser` into the app account —
and `run.sh` immediately drops privileges via `runuser -u executiond` before
execing the actual `executiond` binary, so the daemon itself never runs as
root (mirroring the old systemd unit's `User=executiond`). A redeploy only
needs to swap the `current` symlink and restart the pm2 app — the pm2 app
definition itself never changes between releases.

## pmm compatibility

- Separate namespace: user, `/opt`, `/etc`, `/var/lib` paths, and the
  `executiond` pm2 app are all `executiond`-scoped. The script never touches
  any `pmm*` unit, path, or pm2 app.
- Shared NATS: `executiond` connects to `EXECUTION_NATS_URL`
  (`nats://127.0.0.1:4222` by default), which is the `pmm-nats.service` pmm's
  deploy installs (as a systemd unit or the pm2 app `pmm-nats`). The deploy
  only warns (does not fail) when nothing accepts TCP connections at that
  address — any NATS server there is fine. If
  the connection is lost for good the process exits and pm2's own restart
  policy brings it back.
- `executiond` uses its own PostgreSQL database from `EXECUTION_POSTGRES_URL`
  and applies its own schema on startup. The deploy refuses the placeholder
  default URL.
- One `executiond` instance per ssh alias: the deploy deletes and recreates
  that alias's pm2 app on every run. Several hosts may share one NATS server;
  each trades only the strategies in its `EXECUTION_ALLOWED_STRATEGIES`, which
  the deploy requires.
- Several wallets on one machine: add an ssh alias per wallet (e.g. `aws-a`,
  `aws-b` with the same `HostName`) and set `EXECUTIOND_INSTANCE=b` in
  `scripts/private/aws-b/deploy.env`. That instance runs as the pm2 app
  `executiond-b` under `/opt`, `/etc` and `/var/lib/executiond-b`, so its key
  paths in `executiond.env` are `/etc/executiond-b/private-key.{json,pass}`.
  Each instance runs as its own system user (`executiond-b`). Give each its
  own wallet, its own Postgres database and disjoint
  `EXECUTION_ALLOWED_STRATEGIES` (no `*` on more than one): the deploy refuses
  to install an instance whose `POLYMARKET_MAKER_ADDRESS` or
  `EXECUTION_POSTGRES_URL` another instance on the machine already uses. The
  instance name is read only from `deploy.env`, never from your shell.

## Private bundle

Each host has a git-ignored bundle at `scripts/private/<ssh-host>/`:

```
scripts/private/<ssh-host>/executiond.env      # required: app runtime secrets
scripts/private/<ssh-host>/private-key.json    # required: signing key, encrypted at rest
scripts/private/<ssh-host>/private-key.pass    # required: passphrase for the above
scripts/private/<ssh-host>/deploy.env          # optional: deploy.sh overrides
```

Copy `scripts/executiond.env.example` to `executiond.env`, replace every
`<CHANGE_ME>` on an active setting (the preflight ignores commented lines), and
keep the file mode `0600`. `scripts/deploy.sh` uploads it
to `/etc/executiond/executiond.env`, which `run.sh` sources before dropping
privileges and execing the binary under pm2.

### Signing key

The signing key is never stored in plaintext. Create the encrypted keystore and
its passphrase file with `polykey`:

```bash
go build -o /tmp/polykey ./cmd/polykey
(umask 077; head -c 32 /dev/urandom | base64 > scripts/private/<ssh-host>/private-key.pass)
/tmp/polykey encrypt \
  --key-file scripts/private/<ssh-host>/private-key.json \
  --passphrase-file scripts/private/<ssh-host>/private-key.pass
```

`encrypt` reads the hex key from stdin (or prompts without echo on a terminal),
so it never reaches the shell history. Both files must be mode `0600`.

Before uploading anything, `deploy.sh` checks the modes and runs
`polykey verify` locally, so a wrong passphrase fails the deploy instead of the
daemon. It then installs both files as `/etc/executiond/private-key.{json,pass}`,
owned by the `executiond` user, and restores the previous pair on rollback.
`executiond` decrypts the key once at startup and logs only the resulting
address. See `docs/configuration.md` for the `polykey` subcommands.

A bundle whose `executiond.env` still sets the plaintext `POLYMARKET_PRIVATE_KEY`
deploys with a warning; setting both forms is an error.

Copy `scripts/deploy.env.example` to `deploy.env` if a host needs different
deploy-time settings (e.g. a non-default `EXECUTIOND_PM2_BIN`). It holds no
secrets, is only read locally by `deploy.sh`, and is never uploaded.
`scripts/private/` is excluded by `.gitignore`.

## PnL report

Each release also ships `pnlreport`. It rebuilds realized PnL from the
instance's `fills` and `settlement_payouts` and sends a metrics table, the
wallet curve and one curve per strategy (the `unique_tag` prefix before the
first `:`) to Telegram. Set
`PNL_TELEGRAM_TOKEN` and `PNL_TELEGRAM_CHAT_ID` in that instance's
`executiond.env`. It is not scheduled automatically, so add one root cron line
per instance:

```cron
0 */6 * * * /opt/executiond/current/pnlreport --env /etc/executiond/executiond.env
0 */6 * * * /opt/executiond-b/current/pnlreport --env /etc/executiond-b/executiond.env
```

The report covers the last 72 hours by default (`--hours`) and is labelled by
the env file's directory, so each instance reports separately. Use
`--dry-run --out <dir>` to write the PNGs and `report.md` instead of sending
them. Open positions are carried at cost, not marked to market.
