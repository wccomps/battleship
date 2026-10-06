# battleship

Deploys, resets, powers, snapshots and tears down per-team VM pods on Proxmox.
A web app for volunteers (`battleship serve`) and a CLI for admins share one
engine and one Postgres job queue.

## How it works

- **Every operation is a job.** You preview a plan, confirm it, and a worker
  runs it. The worker plans again first and refuses to run if the cluster
  changed. Jobs on the same teams or templates run one at a time.
- **Battleship holds no Proxmox credential.** Every call is made as the person
  who asked (their Proxmox ticket in the web app, their API token in the
  CLI), so Proxmox's ACLs decide what each person may do.
- **Re-runs are safe.** Each step checks the VM's state first. A cancelled or
  failed job removes the VMs it half-built; it never deletes what it didn't
  create.
- **Live updates** use server-sent events; pages also work without JavaScript.

## CLI

```sh
cp battleship.example.toml battleship.toml   # set proxmox.urls and the site settings
export BATTLESHIP_PROXMOX_TOKEN_ID='jdoe@auth.example.org!cli' BATTLESHIP_PROXMOX_TOKEN_SECRET=...

battleship deploy   -templates '*.kilo.alpha' -teams 1-32
battleship reset    -teams 7 -hosts dc
battleship power    -teams 1-32 -action start
battleship snapshot -teams 7 -hosts dc -name before-scoring
battleship teardown -teams all
battleship nodes                              # prints a urls = [...] line
```

Every command prints its plan and asks you to confirm (`yes`; a teardown
asks for the team range). `-yes` skips the prompt. `-queue` stores the job
for a worker instead of running it here. `-hosts` matches substrings; `-vms`
names exact VMs. A blocked or failed VM makes the command exit 1.

```sh
battleship worker            # runs queued jobs
battleship jobs list | show <id> | cancel <id>
```

## Web app

```sh
export BATTLESHIP_DATABASE_URL=... BATTLESHIP_SEAL_KEY=... BATTLESHIP_OIDC_CLIENT_SECRET=...
battleship serve -log-format json
```

It needs `[proxmox]`, `database.url`, `database.seal_key`, `web.base_url` and
`[oidc]`. It migrates the database at startup, runs `web.workers` job workers,
and serves `/readyz` (database reachable) and `/healthz`.

**Authentik setup:**
1. Create an OAuth2/OpenID provider (confidential client) with redirect URI
   `<base_url>/auth/callback`, a signing key, and the scopes `openid`,
   `email`, `profile` and `offline_access`.
2. Create an application `battleship` for it, and bind policies so only
   volunteers can open it.
3. Add `<base_url>/auth/proxmox/callback` to the redirect URIs of the
   provider behind Proxmox's OpenID realm (`proxmox.realm`). After logging
   in, battleship signs the user in to Proxmox through that realm.
4. Set `oidc.issuer`, `oidc.client_id` and the client secret.

## Who may do what

Proxmox decides. Previews mark each VM the user can't act on as blocked,
naming the missing privilege.

| Step | Privilege | Path |
|---|---|---|
| Clone | `VM.Clone`; `VM.Allocate`; `Datastore.AllocateSpace` | template; team pool; `/storage/<deploy.storage>` |
| Network | `VM.Config.Network`, `VM.Config.Cloudinit`; `SDN.Use` | VM; `/sdn/zones/<proxmox.sdn_zone>/<bridge>` |
| Disk limits / CD-ROM | `VM.Config.Disk` / `VM.Config.CDROM` | VM |
| Snapshot / roll back | `VM.Snapshot` (or `VM.Snapshot.Rollback`) | VM |
| Power | `VM.PowerMgmt` | VM |
| Delete | `VM.Allocate` | VM |
| Free leftover disks | `Datastore.Allocate` | `/storage/<deploy.storage>` |
| Capacity check, others' tasks | `Sys.Audit` | `/nodes/<node>` |

## Configuration

`battleship.example.toml` documents every setting. Unknown keys are
rejected. Secrets can come from `BATTLESHIP_DATABASE_URL`,
`BATTLESHIP_SEAL_KEY`, `BATTLESHIP_OIDC_CLIENT_ID` and
`BATTLESHIP_OIDC_CLIENT_SECRET`. Set `proxmox.ca_file` to the cluster's
`/etc/pve/pve-root-ca.pem`; servers refuse to start without it (or an explicit
`insecure_skip_verify = true`).

Deployment (Docker or Kubernetes): see `deploy/README.md`.

## Tests

```sh
docker run -d --name battleship-pgtest -e POSTGRES_PASSWORD=test -p 127.0.0.1:55432:5432 postgres:16-alpine
export BATTLESHIP_TEST_DATABASE_URL='postgres://postgres:test@127.0.0.1:55432/postgres?sslmode=disable'
go test -race ./...
```

Browser tests run with `BATTLESHIP_BROWSER=http://127.0.0.1:9222` (a Chrome
DevTools endpoint, e.g. the `chromedp/headless-shell` image).
