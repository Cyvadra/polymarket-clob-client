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

| Path | Purpose |
|---|---|
| `/opt/executiond/releases/<timestamp>` | Immutable application releases (`executiond`, `executiontest`) |
| `/opt/executiond/current` | Active release symlink |
| `/opt/executiond/run.sh` | Wrapper pm2 runs: sources the env file, then drops to `executiond` and execs `current/executiond` |
| `/etc/executiond/executiond.env` | Runtime environment, sourced by `run.sh` |
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
  deploy installs. The deploy only warns (does not fail) when
  `pmm-nats.service` is not active — a standalone NATS server is allowed. If
  the connection is lost for good the process exits and pm2's own restart
  policy brings it back.
- `executiond` uses its own PostgreSQL database from `EXECUTION_POSTGRES_URL`
  and applies its own schema on startup. The deploy refuses the placeholder
  default URL.
- One `executiond` instance per wallet: the deploy deletes and recreates the
  pm2 app on every run.

## Private bundle

Each host has a git-ignored bundle at `scripts/private/<ssh-host>/`:

```
scripts/private/<ssh-host>/executiond.env   # required: app runtime secrets
scripts/private/<ssh-host>/deploy.env       # optional: deploy.sh overrides
```

Copy `scripts/executiond.env.example` to `executiond.env`, replace every
`<CHANGE_ME>`, and keep the file mode `0600`. `scripts/deploy.sh` uploads it
to `/etc/executiond/executiond.env`, which `run.sh` sources before dropping
privileges and execing the binary under pm2.

Copy `scripts/deploy.env.example` to `deploy.env` if a host needs different
deploy-time settings (e.g. a non-default `EXECUTIOND_PM2_BIN`). It holds no
secrets, is only read locally by `deploy.sh`, and is never uploaded.
`scripts/private/` is excluded by `.gitignore`.
