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
`/root/.nvm/versions/node/v24.21.0/bin/pm2`).

## Layout on the target

| Path | Purpose |
|---|---|
| `/opt/executiond/releases/<timestamp>` | Immutable application releases (`executiond`, `executiontest`) |
| `/opt/executiond/current` | Active release symlink |
| `/opt/executiond/run.sh` | Wrapper pm2 runs: sources the env file, then execs `current/executiond` |
| `/etc/executiond/executiond.env` | Runtime environment, sourced by `run.sh` |
| `/var/lib/executiond` | Service working directory |

The `executiond` pm2 app always runs `run.sh`, so a redeploy only needs to
swap the `current` symlink and restart the pm2 app — the pm2 app definition
itself never changes between releases.

## pmm compatibility

- Separate namespace: `/opt`, `/etc`, `/var/lib` paths and the `executiond`
  pm2 app are all `executiond`-scoped. The script never touches any `pmm*`
  unit, path, or pm2 app.
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
scripts/private/<ssh-host>/executiond.env
```

Copy `scripts/executiond.env.example` there, replace every `<CHANGE_ME>`, and
keep the file mode `0600`. `scripts/private/` is excluded by `.gitignore`.
`scripts/deploy.sh` uploads it to `/etc/executiond/executiond.env`, which
`run.sh` sources before execing the binary under pm2.
