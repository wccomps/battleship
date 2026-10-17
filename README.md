# battleship

Deploys, resets, powers, snapshots and tears down per-team VM pods on Proxmox.
A web app for volunteers (`battleship serve`) and a CLI for admins share one
engine and one Postgres job queue.

## How it works

- **Every operation is a job.** You preview a plan, confirm it, and a worker
  runs it (a CLI run with no database runs itself). The worker plans again
  first and refuses to run if the cluster changed. Jobs that change the same
  teams, templates or VMs run one at a time.
- **Battleship holds no Proxmox credential.** Every call is made as the person
  who asked (their Proxmox ticket in the web app, their API token in the
  CLI), so Proxmox's ACLs decide what each person may do.
- **Re-runs are safe.** Each step checks the VM's state first. A cancelled or
  failed job removes the VMs it half-built; it never deletes what it didn't
  create.
- **Live updates** use server-sent events; pages also work without JavaScript.

`ARCHITECTURE.md` walks through a job's life, the packages and the terms
the code uses.

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
asks for the team range). `-yes` skips the prompt. With a database, the job
runs on a `battleship serve` worker and the command follows it; `-queue`
exits once it is queued. Keep your `battleship.toml` in step with serve's:
if its settings for what a job does to VMs differ, the job ends stale.
`-hosts` matches substrings; `-vms` names exact VMs. A blocked or failed VM
makes the command exit 1.

```sh
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

## Base images and environments

Base images are templates tagged `base` plus `router`, `windows`, `linux` or `other`, named `<family>-v<N>`.
Install and generalize an OS on a VM in the images pool, shut it down, then **Register** it from
the Images page, which lists the pool's VMs not yet registered, suggests the family (an edit copy's own,
else the name less `-build`) and, where it can tell, the kind, or says why it can't be registered yet
(`battleship image register -vm 101 -family win2022 -kind windows`). A new version deprecates the previous one;
**Open for editing** (`image edit -family`) makes a working copy to patch and register as the next version.

**Environments → Create** takes the first free dev slot and clones a router and hosts into it
(`battleship env create -domain kilo.alpha -router gw=opnsense -host dc=win2022:10 -host web=debian-12`).
Windows and Linux hosts get their name, a static address on the slot's LAN (`.2` is the gateway) and DNS through
the QEMU guest agent. **Set DNS** (`env dns -domain kilo.alpha -server dc`) points them at a domain controller once it is promoted.
**Tear down** (`env teardown -domain kilo.alpha`, confirmed by typing the domain) deletes the VMs in its slot's pool,
never VMs from outside it on its networks.
Proxmox privileges decide who may do each step, including `VM.GuestAgent.Unrestricted` for the setup.

## Configuration

`battleship.example.toml` documents every setting. Unknown keys are
rejected. These environment variables override the file:
`BATTLESHIP_DATABASE_URL`, `BATTLESHIP_SEAL_KEY`, `BATTLESHIP_OIDC_ISSUER`,
`BATTLESHIP_OIDC_CLIENT_ID` and `BATTLESHIP_OIDC_CLIENT_SECRET`. Set `proxmox.ca_file` to the cluster's
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
