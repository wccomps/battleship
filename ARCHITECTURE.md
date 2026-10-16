# Architecture

Battleship turns "do this to these teams' VMs" into a job that runs against
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
   when one changes something the other changes or reads (teams,
   templates, VMs, slots, domains, families: their lock keys); jobs that
   only read a VM, such as clones of one base image, run together. The
   worker keeps the claim with heartbeats. Once none has landed for the
   claimer's `stale_after`, any worker reaps the job (`interrupted`), which
   frees its locks. From half that, the worker stops the run and sends no
   more changes for it until a heartbeat lands again.
4. **Re-plan.** The worker plans once more; if the cluster changed since the
   preview, the job ends `stale` and nothing runs.
5. **Run.** The executor (`apply.Executor`) runs each item's steps, records
   events, retries transient failures, and on a cancel or failure removes
   what it half-built (and reports what it couldn't, such as when the
   claim is unsure).
6. **Finish.** The worker records the outcome (`store.Finish`). Retrying a
   job's failed VMs previews and submits a new job for just those VMs.

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
| **pod** | A team's set of VMs, cloned from templates |
| **master / template** | A tagged source VM / its `.tpl` copy that teams clone from |
| **plan, item, step** | What a job will do: one item per VM, each with ordered steps (clone, network, …, start) |
| **blocked** | An item that can't run (missing privilege, missing snapshot); shown in the preview |
| **fingerprint** | A hash of a plan, to tell whether the cluster changed since the preview |
| **round** | One pass over a job's items; failed items get a retry round |
| **baseline** | The `initial` snapshot taken at deploy; a reset rolls back to it |
| **drift** | A VM whose config no longer matches the rules |
| **leftover / orphaned disks** | Disks a deleted VM left on the storage |
| **view** | One user's live grid, read with their own ticket |
