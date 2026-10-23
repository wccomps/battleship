# battleship

Deploys, resets, powers, snapshots and tears down VM pods on Proxmox.

## How it works

- **Every operation is a job.** You preview a plan, confirm it, and a worker
  runs it. The worker plans again first and refuses to run if the cluster
  changed. Jobs that change the same pods, templates or VMs run one at a time.
- **Battleship holds no Proxmox credential.** Every call is made as the person
  who asked, using their Proxmox ticket, so Proxmox's ACLs decide what each
  person may do.
- **Re-runs are safe.** Each step checks the VM's state first. A cancelled or
  failed job removes the VMs it half-built; it never deletes what it didn't
  create.
- **Live updates** use server-sent events; pages also work without JavaScript.

`ARCHITECTURE.md` walks through a job's life, the packages and the terms
the code uses.

## Running

```sh
cp battleship.example.toml battleship.toml   # set proxmox.urls and the site settings
export BATTLESHIP_DATABASE_URL=... BATTLESHIP_SEAL_KEY=... BATTLESHIP_OIDC_CLIENT_SECRET=...
battleship -log-format json
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
| Clone | `VM.Clone`; `VM.Allocate`; `Datastore.AllocateSpace` (`Mapping.Use` for a `custom-<model>` CPU; `SDN.Use` on an edit/slot/team source bridge) | template or source; target pool; `/storage/<deploy.storage>` (`/mapping/cpu/<model>`; `/sdn/zones/<zone>/<bridge>`) |
| Build template | `VM.Clone` (`VM.PowerMgmt` if stopping a running master); `VM.Allocate`; `Datastore.AllocateSpace` (`Mapping.Use` for a `custom-<model>` CPU) | master; `<images.pool>` (rebuild: also the old template); `/storage/<deploy.storage>` |
| Network | `VM.Config.Network` (`VM.Config.Cloudinit` for cloud-init); `SDN.Use` | VM or pool; `/sdn/zones/<zone>/<bridge>` |
| Disk limits / CD-ROM / CPU | `VM.Config.Disk` / `VM.Config.CDROM` / `VM.Config.CPU` (`Mapping.Use` on `/mapping/cpu/<model>`) | VM or pool |
| Clear notes (a clone copies its master's, which can hold a login), tag the set | `VM.Config.Options` (tagging needs Proxmox's default `user-tag-access` of `free`; otherwise a new tag needs `Sys.Modify` on `/`) | VM or pool |
| Labels, details and notes (`image-register`, `image-deprecate`, `image-details`, `env-edit`) | `VM.Config.Options` | VM or pool |
| Guest setup (`env-create`, `env-add`, `env-edit`) | `VM.GuestAgent.Audit`, `VM.GuestAgent.Unrestricted` (`VM.PowerMgmt` when rebooting a Windows host) | VM or pool |
| Live addresses and SSH CA check | `VM.GuestAgent.Audit` (`VM.GuestAgent.Unrestricted` for the SSH check when held) | VM |
| Snapshot / roll back | `VM.Snapshot` (or `VM.Snapshot.Rollback`) | VM |
| Power | `VM.PowerMgmt` | VM |
| Delete / convert to template | `VM.Allocate` | VM |
| Free leftover disks | `Datastore.Allocate` | `/storage/<deploy.storage>` |
| Capacity check, others' tasks | `Sys.Audit` | `/nodes/<node>` |

## Base images and environments

Base images are templates tagged `base` plus `router`, `windows`, `linux` or `other`, named `<family>-v<N>`.
Install and generalize an OS on a VM in the images pool, shut it down, then **Register** it from
the Images page, which lists the pool's VMs not yet registered, suggests the family (an edit copy's own,
else the name less `-build`) and, where it can tell, the kind, or says why it can't be registered yet.
A new version deprecates the previous one;
**Open for editing** makes a working copy to patch and register as the next version.

**Environments → Create** clones a router and hosts into a free dev slot, the first unless you choose one.
Windows and Linux hosts get their name, a static address on the slot's LAN (`.2` is the gateway) and DNS through
the QEMU guest agent.
**Tear down** (confirmed by typing the domain) deletes the VMs in its slot's pool,
never VMs from outside it on its networks.
The page shows each host's live address from its guest agent, and its external one, assuming the router image
maps LAN `.N` one to one to `.N` of its WAN /24 (no external address on any other WAN prefix).
Proxmox privileges decide who may do each step, including `VM.GuestAgent.Unrestricted` for the setup
and `VM.GuestAgent.Audit` for live addresses.

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
