# Architecture

Battleship turns "do this to these pods' VMs" into a job that runs against
Proxmox as the person who asked, and shows its progress live.

## A job's life

```
form ──> preview ──> confirm ──> stored job ──> worker claims it ──> re-plan ──> run ──> finished
          (plan)      (plan again,   (pending)     (running)          (same plan?      (items, events,
                       same plan?)                                      else stale)      cleanup)
```

1. **Preview.** The planner (`pods.Planner`) reads the cluster and builds a
   `Plan`: one item per VM with the steps it needs. It only reads. Items the
   user lacks a Proxmox privilege for are marked blocked.
2. **Confirm.** The web app plans again and checks the plan's fingerprint
   matches the preview, then stores the job with the user's sealed Proxmox
   ticket (`jobs.Submit`).
3. **Claim.** A worker (`jobs.Worker`, in every `battleship serve`) claims
   the oldest pending job that clashes with no running or older pending
   job, so overlapping jobs run in the order they were made. Two jobs clash
   when one changes something the other changes or reads (pods,
   templates, VMs, slots, domains, families: their lock keys); jobs that
   only read a VM, such as clones of one base image, run together. The
   worker keeps the claim with heartbeats. Once none has landed for the
   claimer's `stale_after`, any worker reaps the job (`interrupted`), which
   frees its locks. From half that, the worker stops the run and sends no
   more changes for it until a heartbeat lands again. A replica shutting
   down claims nothing new but keeps running, and heartbeating, the jobs it
   has; only when `web.shutdown_timeout` is about to run out does it stop
   them, early enough for their cleanup.
4. **Re-plan.** The worker plans once more; if the cluster changed since the
   preview, the job ends `stale` and nothing runs.
5. **Run.** The executor (`apply.Executor`) runs each item's steps, records
   events, retries transient failures, and on a cancel or failure removes
   what it half-built (and reports what it couldn't, such as when the
   claim is unsure).
6. **Finish.** The worker records the outcome (`store.Finish`). Retrying a
   pod job, or an environment's create or add, previews and submits a new
   job for just the VMs that didn't finish; an environment's teardown is
   planned again whole in its slot. Image jobs and single-host environment
   jobs (`env-edit`, `env-power`, `env-delete-host`) aren't retried with
   `RetryInputs`: run them again from their form, and image jobs resume what
   exists.

Every Proxmox call is made with the submitter's credential; battleship holds
none of its own. Postgres LISTEN/NOTIFY wakes idle workers, cancels jobs at
once, and pushes changes to open pages.

## Packages

| Package | Role |
|---|---|
| `pods` | The rules and the model: naming, VM config rules, privileges, capacity, and the read-only `Planner` that makes a `Plan` |
| `apply` | The `Executor` that carries out a plan, and the limits on how hard it pushes Proxmox |
| `jobs` | Submitting, fingerprints, retries, the worker, credential renewal |
| `store` | Postgres: jobs, items, events, claims and locks, sessions, previews, queued refresh token revocations |
| `status` | Each viewer's live grid (polls and drift scans) and the notification hub |
| `web` | Server-rendered pages, forms, live updates (server-sent events) |
| `auth` | Authentik login, the Proxmox sign-in, sessions (and revoking their refresh tokens) and CSRF |
| `proxmox` | Typed Proxmox API client with node failover and task waiting |
| `config` | The TOML config |
| `seal` | Encrypting credentials at rest |

`pods` never imports `apply`: planning can't change anything.

## Words the code uses

| Word | Meaning |
|---|---|
| **pod** | A team's VMs, cloned from templates; the code and config call it by its team number (`teams`, `{team}`) |
| **set** | Masters sharing what follows the host, lower-cased: `dc.kilo.alpha` and `web.Kilo.Alpha` are set `kilo.alpha` (`pods.Set`). A deploy tags each pod VM with its set's tag, `set-kilo.alpha`; from a master with nothing after the host it removes any set tag. A VM whose one set tag is a set's is of that set; with none, several, or a bare `set-`, its set is unknown (`pods.SetRecord`). A teardown of a set takes that set's VMs, and frees disks only at its templates' clone VMIDs, since disks have no tag; a VM of an unknown set goes only with a teardown that names no set |
| **master / template** | A tagged source VM / its `.tpl` copy that pods are cloned from |
| **plan, item, step** | What a job will do: one item per VM, each with ordered steps (clone, network, …, start) |
| **blocked** | An item that can't run (missing privilege, missing snapshot); shown in the preview |
| **fingerprint** | A hash of a plan, to tell whether the cluster changed since the preview |
| **round** | One pass over a job's items; failed items get a retry round, except failures for a missing privilege or a config changed by someone else |
| **baseline** | The `initial` snapshot taken at deploy; a reset rolls back to it |
| **drift** | A pod VM whose config no longer matches the rules a deploy sets: NICs, disk limits, empty notes, pool, baseline, at most one non-empty set tag. No set tag isn't drift: every VM deployed before sets has none |
| **leftover / orphaned disks** | Disks a deleted VM left on the storage |
| **view** | One user's live grid, read with their own ticket |
| **base image, family** | A template tagged `base` and a kind, named `<family>-v<N>`; the versions of one OS build make its family |
| **environment, dev slot** | A dev slot is a numbered pool with its own bridges and VMID block; the environment is the domain-named VMs in it |

## Threat model

- **Principals & credential delegation.** Volunteers sign in via OIDC (`auth.Service`) and complete a second OpenID exchange against Proxmox (`auth.ProxmoxLogin`), binding the returned Proxmox ticket to `<preferred_username>@<proxmox.realm>` (`internal/auth/proxmoxlogin.go`). Battleship holds no ambient Proxmox service token (`noServiceToken` in `cmd/battleship/serve.go` refuses startup if `BATTLESHIP_PROXMOX_TOKEN_*` is set): every read, preview, status poll (`status.Views` keyed by the Proxmox user), and job execution (`jobs.Worker`) runs with the signed-in user's or job submitter's own Proxmox ticket. Proxmox enforces RBAC, while `pods.BlockUnpermitted` (`internal/pods/access.go`) pre-checks ACL paths before planning or running steps.
- **Trust boundaries & entry points.** Browser requests enter `web.Server` (`internal/web/server.go`). Only `/healthz`, `/readyz`, `/static/`, and `/auth/` are unauthenticated; all other routes wrap `s.require` (`auth.RequireUser`), which requires a live session, a usable Proxmox ticket, and—on every method except `GET` and `HEAD`—a constant-time verified CSRF token (`Service.csrfProblem` in `internal/auth/middleware.go`) and a 1 MiB body cap (`maxBody` in `internal/web/middleware.go`). `opConfirm` (`internal/web/confirm.go`) loads the server-stored `store.Preview` by `(session_id, sha256(nonce))` rather than trusting hidden form inputs, and `cancelJob` / `mayCancel` (`internal/web/jobpages.go`) allows only the job's creator (`j.CreatedBy == actor(u)` in `internal/web/ops.go`) or a user holding `pods.HoldsOffered` (`internal/pods/access.go`) on every item's VM (`/vms/<vmid>`) or target pool (`/pool/<pool>`).
- **Secrets at rest & in logs.** Session cookies carry a 32-byte random token whose SHA-256 hex digest (`auth.HashToken` in `internal/auth/session.go`) is stored as the session ID. Session OIDC refresh tokens (`internal/auth/refreshtoken.go`), session Proxmox tickets (`internal/auth/ticket.go`), and job credentials (`internal/jobs/credential.go`) are sealed with AES-256-GCM under domain-separated subkeys derived via HMAC-SHA256 (`seal.NewKey` in `internal/seal/seal.go`), with the session ID or job ID in the AEAD additional data so ciphertexts cannot be transplanted across rows. The PostgreSQL trigger `jobs_end_credentials` (`internal/store/migrations/006_delegated_auth.sql`) deletes a job's credential row when the job finishes, is reaped, or is cancelled, and `store.RenewCredential` (`internal/store/credentials.go`) deletes lapsed credentials. `seal.Key` and `proxmox.Credential` redact secrets in `String`/`GoString`/`Format`, and `config.OIDC` and `config.Database` redact secrets in `String`/`GoString`.
- **Untrusted cluster data & guest execution.** Proxmox VM descriptions and configs are writable by anyone with Proxmox access to a VM and are treated as untrusted input: `pods.NotesChanges` (`internal/pods/vmconfig.go`) strips `description` from cloned pod VMs so base-image credentials never leak to team VMs; `pods.ParseDetails` (`internal/pods/details.go`) strips Unicode `Cf` format characters and bounds note sizes; `html/template` auto-escapes all rendered views; and `pods.Guest.Check` (`internal/pods/guest.go`) validates every hostname, domain, IPv4 address, prefix, MAC, and OpenSSH CA key before embedding values into guest setup scripts sent via the Proxmox QEMU guest agent (`AgentExec`—Battleship never SSHes into nodes or guests).
- **Non-goals & accepted risks.** Battleship does not defend against a compromised Proxmox cluster, identity provider, PostgreSQL database, or host key (`BATTLESHIP_SEAL_KEY` / `oidc.client_secret`), and does not rate-limit authenticated volunteers beyond PostgreSQL pool limits and `apply.Limits` cluster concurrency slots.
