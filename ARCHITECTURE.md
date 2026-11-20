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
   claim is unsure). Items are VMs or their disks, but for record items:
   an environment created with no machines (no router, no hosts) is one
   item that writes `[env <domain>]` into its slot pool's Proxmox comment,
   and an add with no blocked host into, or a teardown of, an environment
   whose pool carries that marker ends with an item that takes it out, run
   after the others and only if none of them failed (`pods.Item.Last`); a
   retry narrowed to the VMs that didn't finish has no such item.
6. **Finish.** The worker records the outcome (`store.Finish`). Retrying a
   pod job, or an environment's create or add, previews and submits a new
   job for just the VMs that didn't finish; an environment's teardown is
   planned again whole in its slot. Image jobs and single-host environment
   jobs (`env-edit`, `env-power`, `env-delete-host`) aren't retried with
   `RetryInputs`: run them again from their form, and image jobs resume what
   exists.

Every Proxmox call is made with the credential of the person acting (for a
job, its submitter), or of `practice.principal` for the session interface and
its jobs; battleship holds none of its own (see
[`THREAT_MODEL.md`](THREAT_MODEL.md)). Postgres
LISTEN/NOTIFY wakes idle workers, cancels jobs at once, and pushes changes to
open pages.

## The session interface

The practice app books self-service sessions and has no person at a browser
when one starts or ends, so it can't preview and confirm. With `[practice]`
set, `battleship serve` opens a second listener (`practice.listen`) that
serves `web.Server.PracticeHandler` and none of the pages:

| Call | Does |
|---|---|
| `GET /v1/sets` | The approved sets: their hosts, their built templates with a digest of each one's config, and what one pod takes |
| `GET /v1/pods/{pod}/vms` | One session pod's VMs |
| `POST /v1/jobs` | Submits a deploy of an approved set to pods, carrying the digests `GET /v1/sets` listed for its templates, or a teardown of pods |
| `GET /v1/jobs/{id}`, `POST /v1/jobs/{id}/cancel` | Reads or cancels a job the interface submitted |

Each call carries `practice.key` and a Proxmox ticket of `practice.principal`,
which the practice app gets by logging in to Proxmox itself. Battleship
proves the ticket with Proxmox and acts with it, so it still holds no
credential. Its jobs enter the same queue at step 2, skipping the preview,
and are marked with their origin (`store.OriginSession`). The interface's
own checks stand in for the preview: pods inside `practice.pods`, deploy or
teardown only, an approved set, nothing blocked in a deploy. A session
deploy carries `approved_only`: the planner takes exactly the named set's
masters, only while the set is approved, and clones built templates without
building any (`pods.DeployRequest.ApprovedSet`), and blocks a template whose
digest differs from the one the request carries; the job's fingerprint covers
the digests, and the worker's re-plan
(step 4) checks both again. `internal/web/testdata/practice` holds an example of
each call, for the practice app's client to be tested against.

## Packages

| Package | Role |
|---|---|
| `pods` | The rules and the model: naming, VM config rules, privileges, capacity, and the read-only `Planner` that makes a `Plan` |
| `apply` | The `Executor` that carries out a plan, and the limits on how hard it pushes Proxmox |
| `jobs` | Submitting, fingerprints, retries, the worker, credential renewal |
| `store` | Postgres: jobs, items, events, claims and locks, sessions, previews, queued refresh token revocations |
| `status` | Each viewer's live grid (polls and drift scans) and the notification hub |
| `web` | Server-rendered pages, forms, live updates (server-sent events); the session interface |
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
| **approved set** | A set whose masters all carry `deploy.approved_tag`, a tag Proxmox registers so only `Sys.Modify` on `/` may set it (`pods.ApprovedSets`): one a self-service session may be given. A master without it leaves the whole set unapproved |
| **plan, item, step** | What a job will do: one item per VM, each with ordered steps (clone, network, …, start) |
| **blocked** | An item that can't run (missing privilege, missing snapshot); shown in the preview |
| **fingerprint** | A hash of a plan, to tell whether the cluster changed since the preview |
| **round** | One pass over a job's items; failed items get a retry round, except failures for a missing privilege or a config changed by someone else |
| **baseline** | The `initial` snapshot taken at deploy; a reset rolls back to it |
| **drift** | A pod VM whose config no longer matches the rules a deploy sets: NICs, disk limits, empty notes, pool, baseline, at most one non-empty set tag. No set tag isn't drift: every VM deployed before sets has none |
| **leftover / orphaned disks** | Disks a deleted VM left on the storage |
| **view** | One user's live grid, read with their own ticket |
| **base image, family** | A template tagged `base` and a kind, named `<family>-v<N>`; the versions of one OS build make its family |
| **environment, dev slot** | A dev slot is a numbered pool with its own bridges and VMID block; the environment is the domain-named VMs in it, or, while it has none, the slot pool's comment marker `[env <domain>]` (`pods.EnvMarker`; the rest of the comment is the admins'). An add into a marker-only environment clears the marker once all its hosts succeed; a blocked or failed host, or a narrowed retry, leaves it beside the VMs. A marker naming another domain than the VMs makes the slot mixed, as VMs of two domains do |
