# executiond deployment

`deploy.sh` installs the `executiond` execution daemon on a host that already
runs pmm. It mirrors `pmm/scripts/deploy.sh`: it runs `go test`, cross-builds a
release, uploads it plus a private per-host environment file over SSH, installs
an immutable release under `/opt/executiond/releases/<timestamp>`, swaps the
`current` symlink atomically, writes the systemd unit, waits for the service to
settle, rolls back on any error, and keeps the three newest releases.

## Usage

```bash
scripts/deploy.sh <ssh-host>
# or: EXECUTIOND_DEPLOY_HOST=<ssh-host> scripts/deploy.sh
```

`PMM_DEPLOY_HOST` is also honoured, so the same variable can drive both deploys.
Override the target user with `EXECUTIOND_DEPLOY_TARGET` (default `root`) and the
platform with `EXECUTIOND_DEPLOY_ARCH` / `EXECUTIOND_DEPLOY_OS`.

## Layout on the target

| Path | Purpose |
|---|---|
| `/opt/executiond/releases/<timestamp>` | Immutable application releases (`executiond`, `executiontest`) |
| `/opt/executiond/current` | Active release symlink |
| `/etc/executiond/executiond.env` | Runtime environment, loaded by the unit |
| `/var/lib/executiond` | Service working directory |

The service runs as the unprivileged `executiond` user.

## pmm compatibility

- Separate namespace: user, `/opt`, `/etc`, `/var/lib` paths, and the
  `executiond.service` unit are all `executiond`-scoped. The script never
  touches any `pmm*` unit or path.
- Shared NATS: `executiond` connects to `EXECUTION_NATS_URL`
  (`nats://127.0.0.1:4222` by default), which is the `pmm-nats.service` pmm's
  deploy installs. The unit only `Wants` pmm-nats, so pmm redeploys that restart
  pmm-nats do not stop executiond; it reconnects, and if the connection is lost
  for good the process exits and `Restart=always` brings it back.
- The remote step warns, but does not fail, when `pmm-nats.service` is not
  active — a standalone NATS server is allowed.
- `executiond` uses its own PostgreSQL database from `EXECUTION_POSTGRES_URL`
  and applies its own schema on startup. The deploy refuses the placeholder
  default URL.
- One `executiond` instance per wallet: the deploy stops the old process before
  starting the new one.

## Private bundle

Each host has a git-ignored bundle at `scripts/private/<ssh-host>/`:

```
scripts/private/<ssh-host>/executiond.env
```

Copy `scripts/executiond.env.example` there, replace every `<CHANGE_ME>`, and
keep the file mode `0600`. `scripts/private/` is excluded by `.gitignore`.
