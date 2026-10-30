# Threat Model

Battleship turns volunteer requests into queued jobs that run against Proxmox as the person who asked; [`ARCHITECTURE.md`](ARCHITECTURE.md) covers how jobs and locks work. Citations name the function or type, not a line number.

Update this file in the same change whenever a route, an authorization rule, a credential or secret, a guest-agent command, or the build and deploy path changes. §5 also describes GitHub settings, which change without a diff here: re-check them with the `gh api` calls it names.

## 1. Attackers & What They Want

| Attacker | Reach | Goal Battleship must deny |
|---|---|---|
| **Internet client** | The public routes in §3 and the OIDC login | Signing in without an Authentik account; acting on a session or CSRF token they don't hold. |
| **Whoever holds root on an environment host** | Everything its guest agent reports: interface names and addresses, host name, command output. Battleship calls the agent only on environment (dev-slot) hosts, never on team pod VMs | Injecting markup into Battleship pages, steering a job at another VM, choosing what Battleship runs, or misleading volunteers about the guest. |
| **Signed-in volunteer** (trusted, possibly careless or curious) | Every page; their own Proxmox ACLs, which may include editing VM notes | Acting in Proxmox as anyone but themselves, or beyond their own ACLs. Seeing other volunteers' jobs and lab logins is allowed (§8). |
| **Anyone who can change what is deployed** | §5: battleship's `main`, the image registry, and the Argo CD repository's battleship manifest | Not stopped by code: whoever controls the image or its config controls every signed-in volunteer's ticket. |

**Assets, worst loss first:** acting in Proxmox as another person (their ticket or a job credential); the seal key and OIDC client secret, which unlock every stored job credential and every captured ticket cookie; the private key of `environments.ops_ca_key`, which Battleship installs as a root-login SSH CA on every environment host it configures; Battleship's availability during an event; job history and the lab guest logins in VM notes.

## 2. Principals & Credential Delegation

- **Sign-in:** Authentik decides who may sign in (`login` and `callback` in `internal/auth/login.go`), with Authorization Code + PKCE (`S256`), `state`, `nonce`, and `at_hash` (when the ID token carries one) checked in `Service.identify`. A second OpenID exchange against Proxmox (`proxmoxStart` / `proxmoxCallback` in `internal/auth/proxmoxlogin.go`) binds the returned ticket only if its user exactly equals `preferred_username@realm`, with no case or Unicode folding. The ticket lives in a `battleship_pve` cookie, sealed with its lifetime and bound to the session (`internal/auth/ticket.go`). Battleship keeps no server-side copy of the session's ticket.
- **No service credential:** `battleship serve` refuses to start with a Proxmox token in the environment (`noServiceToken`) or config (`removedKeys`). Every Proxmox call is bound to a person's credential via `proxmox.Client.As` or `AsSource`, and an unbound call fails closed with `proxmox.ErrNoCredential`, except the login calls that send none or carry their own (`OpenIDAuthURL`, `OpenIDLogin`, `RenewTicket`, through `Client.anonymous`). In `serve`, `cluster.Bind` (`cmd/battleship/cluster.go`) is the only source of a `pods.API`. Previews, confirms, retries and live views use the viewer's own ticket; job workers use the submitter's.
- **Job credentials:** confirming a job seals the submitter's ticket into `job_credentials` (`internal/jobs/credential.go`), and `jobs.Renewer` renews it while the job waits and runs.
- **Retry acts as the clicker:** any signed-in volunteer may retry any job (`retryJob` in `internal/web/jobpages.go`). It re-plans with the clicker's ticket and goes through the usual preview and typed confirmation, so Proxmox ACLs decide what it may do.

## 3. Trust Boundaries & Entry Points

- **Unauthenticated routes:** `/healthz`; `/readyz`, whose body is `ready`, `shutting down` or `database: unreachable`, never an internal error (`readyz` in `internal/web/health.go`); `/favicon.ico` and `/static/{file}` (`assetRoutes`); the `/` catch-all 404 (`notFound`); and `GET /auth/{login,callback}` (`Service.Routes`). `POST /auth/logout` checks the session and CSRF token itself, and `GET /auth/proxmox{,/callback}` require `s.liveSession`.
- **Reverse proxy:** `forwarded` (`internal/web/middleware.go`) strips every forwarding header and trusts `X-Forwarded-For` / `X-Forwarded-Proto` only from `web.trusted_proxies`.
- **Browser requests:**
  - `limitBody` caps request bodies at 1 MiB before any handler or form parse.
  - Every route from `volunteerRoutes` is wrapped in `s.require` → `auth.Service.RequireUser`: a live session, periodic OIDC refresh-token revalidation (`web.session_refresh`), and a usable Proxmox ticket.
  - Every method but `GET` and `HEAD` passes `Service.csrfProblem`: any `Origin` must match `s.origin`, and the per-session `csrf_token` is compared in constant time.
  - `opConfirm` takes `jobs.Inputs` from a server-stored preview that this session made, not from the form; re-plans and compares `jobs.Fingerprint`; enforces `TypedConfirm()`; and claims the preview atomically inside `store.CreateJob`.
  - **Cancel is Battleship's own rule:** cancelling only sets a flag, and the stop and cleanup run with the submitter's ticket, so Proxmox never sees who cancelled. `mayCancel` therefore allows it only to the submitter (`actor(u)`, the OIDC email or subject) or to someone holding `pods.HoldsOffered` on every item's VM or target pool, read from their cached Proxmox permissions.
- **Redirects and headers:** `safeNext` / `auth.LocalURL` (`internal/auth/next.go`) validate post-login, error-page and refresh targets. `proxmoxStart` requires an `http`/`https` redirect with a non-empty `state`; logout goes only to the issuer's discovered end-session URL (`webURL`). `s.secure` sets a CSP with no inline script and `form-action` limited to self and the issuer, `X-Frame-Options: DENY`, `nosniff`, `Referrer-Policy: same-origin`, and HSTS under HTTPS.
- **Proxmox and Authentik:** Proxmox TLS is verified against `proxmox.ca_file` unless `proxmox.insecure_skip_verify` is set (`RequireTLSVerify`). Path segments are escaped (`vmPath` and its siblings in `internal/proxmox`), and responses are capped at 32 MiB (`maxAnswer`), lower for agent calls.
- **VM notes:** anyone with Proxmox rights to edit a VM's description controls its notes. `pods.NotesChanges` drops `description` from cloned pod VMs so base-image logins never reach team VMs; `CheckDetails` rejects non-printable values, `dropFormat` strips Unicode `Cf` characters when parsing, and `CheckDescription` bounds the length.

## 4. Hostile Guests (QEMU Guest Agent)

Anyone with root on an environment host controls its guest agent, so every agent reply is attacker-controlled. Battleship never SSHes into nodes or guests; it reaches guests only through the Proxmox agent API (`internal/proxmox/agent.go`), as the user whose job or page caused the call.

- **Battleship runs only fixed commands:** the setup script from `GuestCommand` / `GuestScript` (`internal/pods/guest.go`), built only from values `CheckSSHKey`, `pods.Guest.Check` and `macRE` validate first; `pods.SetupStateCommand`; and the read-only `SSHCheckCommand`, run on environment hosts when a volunteer opens the environment pages (`checkHostSSH`). A reply never becomes command text, but it can choose among fixed actions on its own VM: `RebootMarker` in the setup output restarts that VM, and a Windows-looking interface name picks the Windows SSH check.
- **Replies are capped and reduced:** every agent call carries a size cap (`Client.agent` refuses one without). Addresses are parsed into `netip.Prefix` (`proxmox.GuestIP`); `CheckSSHOutput` returns one of a fixed set of warnings. Guest output a job error shows (the setup state, a failed command's last lines, the host name and addresses `verifyGuest` saw) goes through `guestText`, which quotes it and caps it at 1 KiB; every volunteer can read job errors. When an agent call fails, Proxmox puts the error text in its status line and the guest can choose it: Battleship shows that unquoted, capped at 4 KiB (`maxMessage`).
- **Job text is always storable:** the store replaces NUL and invalid UTF-8 in every text and JSON value a job records (`internal/store/text.go`), so guest or Proxmox text can't make PostgreSQL refuse a write and lose a job's outcome. Job inputs are stored as given: `Inputs.Validate` refuses control characters before a job exists.
- **Rendering:** guest text reaches pages only through `html/template`. The only `template.HTML` values are `keepNames`, which escapes its input, and fixed SVG icons; the CSP forbids inline script.
- **Addresses are claims:** environment pages show the addresses guests report (`LiveAddrs`). A rooted guest can report any address, so volunteers must not treat one as proof of where an SSH session will land.

## 5. Build & Deploy Path

Whoever controls the deployed image or its config controls every signed-in volunteer's ticket.

- **CI** (`.github/workflows/ci.yml`): a push to `main` runs `test`, then `publish` builds and pushes the public image to GHCR as `sha-<short sha>` with the workflow `GITHUB_TOKEN` and prints its digest in the run summary. `govulncheck` runs as a separate `vulncheck` job and gates nothing. CI holds no credential for the Argo CD repository and deploys nothing.
- **Deploys are manual:** someone with write access to the Argo CD repository sets the image's `newTag` and `digest` from a `main` run's summary. The digest decides what runs, so overwriting a `sha-` tag changes nothing deployed.
- **The Argo CD repository** holds Battleship's manifests: the pinned image; `battleship.toml`, which sets `proxmox.urls`, the Proxmox CA, `web.trusted_proxies` and `environments.ops_ca_key`; the Deployment, which sets the OIDC issuer; the NetworkPolicy; and SOPS-encrypted secrets. Anyone who can write it can deploy any image, point volunteers' Proxmox logins elsewhere, swap the ops CA for root SSH on new environment hosts, or replace the secrets without reading them.
- **Controls that hold:** every action is pinned to a commit SHA (enforced by the repository's Actions settings); no workflow uses `pull_request_target`; fork pull requests from first-time contributors need approval and never get secrets. The `main` ruleset blocks deletion and force-pushes and requires a pull request and a passing `Test`, with no bypass. The image package grants this repository's Actions Write, not inherited admin. Dependabot proposes action, Go module and base-image updates; the base images are pinned by tag, not digest.
- **Gaps:**
  - The Argo CD repository's `main` has no branch protection, and more people can write to it than to this repository. Protecting it needs an organization owner.
  - The `main` ruleset requires no approving review, so anyone with write access can merge their own change; it reaches production only when someone deploys it.
  - A workflow on any branch can request `packages: write` and push or overwrite image tags. The digest pin keeps that out of production, but a deployer who copies a digest from anywhere but a `main` run's summary can be misled.
- **Re-check with:** `gh api repos/<owner>/battleship/rulesets`, `.../actions/secrets`, `.../actions/permissions`, the package's Actions access, and the Argo CD repository's collaborators and branch protection.

## 6. Secret Invariants

- **Sealing (`internal/seal`):** `seal.NewKey(secret, purpose)` derives a per-purpose AES-256-GCM key, and each sealed value is bound to its purpose and to a binding (a session or job ID, or a fixed one for login-state cookies and the key check), so it opens only where it was written. Columns beside a sealed value, such as its user, are not covered; a database writer can move a job credential's `login_at` to keep renewing it past `proxmox.ticket_max_age`.
  - `oidc.client_secret` keys OIDC login-state cookies (`internal/auth/loginstate.go`), session refresh and ID tokens (`internal/auth/refreshtoken.go`; logout sends the ID token to the IdP as `id_token_hint`), and each session's Proxmox ticket: the ticket cookie, with its CSRF-prevention token and lifetime, bound to the session ID (`internal/auth/ticket.go`). Battleship's own session `csrf_token` is stored unsealed.
  - `database.seal_key` keys `job_credentials`, bound to the job ID, and the startup key check (`Credentials.CheckKey`), both in `internal/jobs/credential.go`.
- **Hashed at rest:** session tokens, Proxmox OpenID state tokens and preview nonces are stored only as SHA-256 digests (`auth.HashToken`, `storePreview`).
- **Credential lifecycle:** the `jobs_end_credentials` trigger deletes a job's credential the moment the job leaves `pending`/`running`, and `store.RenewCredential` deletes lapsed ones. When the IdP lists a revocation endpoint, ending a session queues its refresh token for revocation in the same statement (`EndSession` in `internal/store/sessions.go`).
- **Redaction:** `seal.Key` and `proxmox.Credential` redact secrets in `String`, `GoString` and `Format`; `config.OIDC` and `config.Database` in `String` and `GoString`.

## 7. Security-Critical Invariants & Sharp Edges

- **Proxmox RBAC is the only gate**, apart from cancel and preview ownership (§3): `mayAnywhere`, `mayOn`, `lacks` and `mayRun` (`internal/web/privileges.go`) only hide or disable controls, and `pods.BlockUnpermitted` blocks plan items early from a cached copy of the user's permissions. Proxmox refusing the bound ticket is what actually stops an action.
- **Privilege reads fail closed:** every `pods.API` reads privileges (`PermissionReader` in `pods.Reads`), `mayCancel` refuses anyone but the submitter when the viewer's privileges can't be read, and `BlockUnpermitted` errors on a nil `Access`.
- **Bounded SSE stream lifetime:** event streams (`Server.stream`) end after `streamLifetime`, derived from `web.session_refresh`, so revoked sessions go back through `RequireUser` on reconnect.
- **Liveness and ping pools:** `store.Store` gives worker heartbeats (`Worker.heartbeat`) and the public `/readyz` ping (`Store.Ping`) their own pools, so web query load can't starve heartbeats and a `/readyz` flood takes no heartbeat or main-pool connection.

## 8. Explicit Non-Goals & Accepted Risks

- **Shared job log and lab logins:** every volunteer Authentik lets sign in can read the job log (`/logs`, `/logs/{id}`) and stored `jobs.inputs` / `jobs.plan`, including base-image and environment host logins (`ImageDetails.Password`) written to VM notes. Those are lab guest credentials for volunteers, not infrastructure secrets.
- **No local user or role table:** Authentik decides who may sign in, and Proxmox ACLs decide what each person may do.
- **Removal in Authentik is not instant:** sessions end at the next `web.session_refresh`, but the person's queued and running jobs keep renewing their Proxmox ticket until `proxmox.ticket_max_age`. Disable them in Proxmox to stop them at once.
- **Compromised control planes or host secrets:** Battleship does not defend against a compromised Proxmox cluster, Authentik, deploy path (§5), or PostgreSQL or captured ticket cookies together with `BATTLESHIP_SEAL_KEY` / `BATTLESHIP_OIDC_CLIENT_SECRET`, nor against unauthenticated floods above PostgreSQL pool limits and `apply.Limits` concurrency slots.
