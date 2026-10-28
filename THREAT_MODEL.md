# Threat Model

Battleship turns volunteer requests into queued jobs that run against Proxmox as the person who asked; [`ARCHITECTURE.md`](ARCHITECTURE.md) covers how jobs and locks work. Citations name the function or type, not a line number.

Update this file in the same change whenever a route, an authorization rule, a credential or secret, a guest-agent command, or the build and deploy path changes. §5 also describes GitHub settings, which change without a diff here: re-check them with the `gh api` calls it names.

## 1. Attackers & What They Want

| Attacker | Reach | Goal Battleship must deny |
|---|---|---|
| **Internet client** | The public routes in §3 and the OIDC login | Signing in without an Authentik account; acting on a session or CSRF token they don't hold. |
| **Competitor or red team with root on a pod VM** | Everything its guest agent reports: interface names and addresses, host name, command output (red team: across many pods) | Injecting markup into Battleship pages, steering a job at another VM, choosing what Battleship runs, or misleading volunteers about the guest. |
| **Signed-in volunteer** (trusted, possibly careless or curious) | Every page; their own Proxmox ACLs, which may include editing VM notes | Acting in Proxmox as anyone but themselves, or beyond their own ACLs. Seeing other volunteers' jobs and lab logins is allowed (§8). |
| **Anyone who can change what is deployed** | §5: battleship's `main` and `v*` tags, the image registry, and the Argo CD repository's battleship manifest | Not stopped by code: whoever controls the image or its config controls every signed-in volunteer's ticket. |

**Assets, worst loss first:** acting in Proxmox as another person (their ticket or a job credential); the seal key and OIDC client secret, which unlock every stored ticket; Battleship's availability during an event; job history and the lab guest logins in VM notes.

## 2. Principals & Credential Delegation

- **Sign-in:** Authentik decides who may sign in (`login` and `callback` in `internal/auth/login.go`), with Authorization Code + PKCE (`S256`), `state`, `nonce`, and `at_hash` checked in `Service.identify`. A second OpenID exchange against Proxmox (`proxmoxStart` / `proxmoxCallback` in `internal/auth/proxmoxlogin.go`) binds the returned ticket only if its user exactly equals `preferred_username@realm`, with no case or Unicode folding.
- **No service credential:** `battleship serve` refuses to start with a Proxmox token in the environment (`noServiceToken`) or config (`removedKeys`). Every Proxmox call is bound to a person's credential via `proxmox.Client.As` or `AsSource`; an unbound call fails closed with `proxmox.ErrNoCredential`. Previews, confirms, retries and live views use the viewer's own ticket; job workers use the submitter's.
- **Job credentials:** confirming a job seals the submitter's ticket into `job_credentials` (`internal/jobs/credential.go`), and `jobs.Renewer` renews it while the job waits and runs. The schema and `Credentials.Open` also accept `kind = 'token'` (a person's own API token), but nothing in production creates one: sign-in only yields tickets.
- **Retry acts as the clicker:** any signed-in volunteer may retry any job (`retryJob` in `internal/web/jobpages.go`). It re-plans with the clicker's ticket and goes through the usual preview and typed confirmation, so Proxmox ACLs decide what it may do.

## 3. Trust Boundaries & Entry Points

- **Unauthenticated routes:** `/healthz`; `/readyz`, whose body is `ready`, `shutting down` or `database: unreachable`, never an internal error (`readyz` in `internal/web/health.go`); `/favicon.ico` and `/static/{file}` (`assetRoutes`); the `/` catch-all 404 (`notFound`); and `GET /auth/{login,callback}` (`Service.Routes`). `POST /auth/logout` checks the session and CSRF token itself, and `GET /auth/proxmox{,/callback}` require `s.liveSession`.
- **Reverse proxy:** `forwarded` (`internal/web/middleware.go`) strips every forwarding header and trusts `X-Forwarded-For` / `X-Forwarded-Proto` only from `web.trusted_proxies`.
- **Browser requests:**
  - `limitBody` caps request bodies at 1 MiB before any handler or form parse.
  - Every route from `volunteerRoutes` is wrapped in `s.require` → `auth.Service.RequireUser`: a live session, periodic OIDC refresh-token revalidation (`web.session_refresh`), and a usable Proxmox ticket.
  - Every method but `GET` and `HEAD` passes `Service.csrfProblem`: any `Origin` must match `s.origin`, and the per-session `csrf_token` is compared in constant time.
  - `opConfirm` takes `jobs.Inputs` from the server-stored preview, not the form; re-plans and compares `jobs.Fingerprint`; enforces `TypedConfirm()`; and claims the preview atomically inside `store.CreateJob`.
  - **Cancel is Battleship's own rule:** cancelling only sets a flag, and the stop and cleanup run with the submitter's ticket, so Proxmox never sees who cancelled. `mayCancel` therefore allows it only to the submitter (`actor(u)`, the OIDC email or subject) or to someone holding `pods.HoldsOffered` on every item's VM or target pool, read from their cached Proxmox permissions.
- **Redirects and headers:** `safeNext` / `auth.LocalURL` (`internal/auth/next.go`) validate post-login, error-page and refresh targets. `proxmoxStart` requires an `http`/`https` redirect with a non-empty `state`; logout goes only to the issuer's discovered end-session URL (`webURL`). `s.secure` sets a CSP with no inline script and `form-action` limited to self and the issuer, `X-Frame-Options: DENY`, `nosniff`, `Referrer-Policy: same-origin`, and HSTS under HTTPS.
- **Proxmox and Authentik:** Proxmox TLS is verified against `proxmox.ca_file` unless `proxmox.insecure_skip_verify` is set (`RequireTLSVerify`). Path segments are escaped (`vmPath` and its siblings in `internal/proxmox`), and responses are capped at 32 MiB (`maxAnswer`), lower for agent calls.
- **VM notes:** anyone with Proxmox rights to edit a VM's description controls its notes. `pods.NotesChanges` drops `description` from cloned pod VMs so base-image logins never reach team VMs; `CheckDetails` rejects non-printable values, `dropFormat` strips Unicode `Cf` characters when parsing, and `CheckDescription` bounds the length.

## 4. Hostile Guests (QEMU Guest Agent)

Competitors and red team hold root in pod VMs, so every guest-agent reply is attacker-controlled. Battleship never SSHes into nodes or guests; it reaches guests only through the Proxmox agent API (`internal/proxmox/agent.go`), as the user whose job or page caused the call.

- **Battleship runs only fixed commands:** the setup script from `GuestCommand` / `GuestScript` (`internal/pods/guest.go`), built only from values `CheckSSHKey`, `pods.Guest.Check` and `macRE` validate first; `pods.SetupStateCommand`; and the read-only `SSHCheckCommand`, run on environment hosts when a volunteer opens an environment page (`checkHostSSH`). A reply never becomes command text, but it can choose among fixed actions on its own VM: `RebootMarker` in the setup output restarts that VM, and a Windows-looking interface name picks the Windows SSH check.
- **Replies are capped and reduced:** every agent call carries a size cap (`Client.agent` refuses one without). Addresses are parsed into `netip.Prefix` (`guestAddr`); `CheckSSHOutput` returns one of a fixed set of warnings; `verifyGuest` quotes the reported host name with `%q`. Failed guest commands put their output into the job error, which every volunteer can read.
- **Rendering:** guest text reaches pages only through `html/template`. The only `template.HTML` values are `keepNames`, which escapes its input, and fixed SVG icons; the CSP forbids inline script.
- **Addresses are claims:** environment pages show the addresses guests report (`LiveAddrs`). A rooted guest can report any address, so volunteers must not treat one as proof of where an SSH session will land.

## 5. Build & Deploy Path

Whoever controls the deployed image or its config controls every signed-in volunteer's ticket.

- **CI** (`.github/workflows/ci.yml`): a push to `main` runs `test`, then `publish` builds and pushes the public image to GHCR with the workflow `GITHUB_TOKEN`, then `deploy` (in the `production` environment, `main` only) pins `sha-<short sha>` in the Argo CD repository with `ARGOCD_REPO_TOKEN`. `govulncheck` runs as a separate `vulncheck` job and gates nothing. A `v*` tag push also runs `publish`, so a tag on any commit publishes images, including a `sha-` tag.
- **The Argo CD repository** holds Battleship's manifest: the pinned image tag (a tag, not a digest) and the generated `battleship.toml`, which sets `proxmox.urls`, the Proxmox CA and the OIDC issuer. Anyone who can write it can deploy any image or point volunteers' Proxmox logins elsewhere.
- **Controls that hold:** every action is pinned to a commit SHA (enforced by the repository's Actions settings); the default workflow token is read-only and `publish` asks for `packages: write` itself; no workflow uses `pull_request_target`; fork pull requests from first-time contributors need approval and never get secrets. Dependabot proposes action, Go module and base-image updates; the base images are pinned by tag, not digest.
- **Gaps:**
  - `ARGOCD_REPO_TOKEN` is a repository secret, not a `production` environment secret, so any workflow run on any branch in the repository can read it; only the `deploy` job's environment is limited to `main`.
  - The `main` ruleset blocks only deletion and force-pushes: no pull request, review or passing checks are required, and nothing guards `v*` tags. Anyone with write access can deploy by pushing.
  - The Argo CD repository's `main` has no branch protection, and more people can write to it than to this repository.
- **Re-check with:** `gh api repos/<owner>/battleship/rulesets`, `.../actions/secrets`, `.../environments/production/secrets`, `.../actions/permissions`, and the Argo CD repository's collaborators and branch protection.

## 6. Secret Invariants

- **Sealing (`internal/seal`):** `seal.NewKey(secret, purpose)` derives a per-purpose AES-256-GCM key, and each sealed value is bound to its purpose and to a binding (a session or job ID), so it opens only where it was written.
  - `oidc.client_secret` keys OIDC login-state cookies (`internal/auth/loginstate.go`), session refresh and ID tokens (`internal/auth/refreshtoken.go`), and each session's Proxmox ticket and Proxmox CSRF-prevention token (`internal/auth/ticket.go`). Battleship's own session `csrf_token` is stored unsealed.
  - `database.seal_key` keys `job_credentials`, bound to the job ID, and the startup key check (`Credentials.CheckKey`), both in `internal/jobs/credential.go`.
- **Hashed at rest:** session tokens, Proxmox OpenID state tokens and preview nonces are stored only as SHA-256 digests (`auth.HashToken`, `storePreview`).
- **Credential lifecycle:** the `jobs_end_credentials` trigger deletes a job's credential the moment the job leaves `pending`/`running`, and `store.RenewCredential` deletes lapsed ones. When the IdP lists a revocation endpoint, ending a session queues its refresh token for revocation in the same statement (`EndSession` in `internal/store/sessions.go`).
- **Redaction:** `seal.Key` and `proxmox.Credential` redact secrets in `String`, `GoString` and `Format`; `config.OIDC` and `config.Database` in `String` and `GoString`.

## 7. Security-Critical Invariants & Sharp Edges

- **Proxmox RBAC is the only gate:** `mayAnywhere`, `mayOn`, `lacks` and `mayRun` (`internal/web/privileges.go`) only hide or disable controls, and `pods.BlockUnpermitted` blocks plan items early from a cached copy of the user's permissions. Proxmox refusing the bound ticket is what actually stops an action.
- **Test-fake fallbacks must stay test-only:** when `pods.API` is not a `*proxmox.Client` / `pods.PermissionReader` (unit-test fakes), `asUser` returns it unbound, `accessFor` returns nil, `mayAnywhere`, `mayOn` and `mayCancel` then allow everything, the web `planner` leaves `Access` nil so `BuildPlan` skips `BlockUnpermitted`, and task following is skipped. Production startup (`newProxmoxAPI` in `cmd/battleship/main.go`) must always build a `*proxmox.Client`.
- **Bounded SSE stream lifetime:** event streams (`Server.stream`) end after `streamLifetime`, derived from `web.session_refresh`, so revoked sessions go back through `RequireUser` on reconnect.
- **Liveness pool:** `store.Store` keeps `s.liveness` apart from `s.pool` so web query load cannot starve worker heartbeats (`Worker.heartbeat`). `/readyz` pings on the liveness pool too, and it is public, so it shares that pool with heartbeats.

## 8. Explicit Non-Goals & Accepted Risks

- **Shared job log and lab logins:** every volunteer Authentik lets sign in can read the job log (`/logs`, `/logs/{id}`) and stored `jobs.inputs` / `jobs.plan`, including base-image and environment host logins (`ImageDetails.Password`) written to VM notes. Those are lab guest credentials for volunteers, not infrastructure secrets.
- **No local user or role table:** Authentik decides who may sign in, and Proxmox ACLs decide what each person may do.
- **Compromised control planes or host secrets:** Battleship does not defend against a compromised Proxmox cluster, Authentik, deploy path (§5), or PostgreSQL together with `BATTLESHIP_SEAL_KEY` / `BATTLESHIP_OIDC_CLIENT_SECRET`, nor against unauthenticated floods above PostgreSQL pool limits and `apply.Limits` concurrency slots.
