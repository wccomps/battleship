# Threat Model

Battleship turns volunteer requests into queued jobs that run against Proxmox using the submitter's own Proxmox ticket. [`ARCHITECTURE.md`](ARCHITECTURE.md) describes how jobs, locks, and packages fit together; this document records who Battleship defends against, the security boundaries, secret invariants, sharp edges, and accepted risks that code changes must preserve. Citations name the function or type, not a line number.

Update this file in the same change whenever a route, a credential or secret, a guest-agent command, or the build and deploy path changes.

## 1. Attackers & What They Want

| Attacker | Reach | Goal Battleship must deny |
|---|---|---|
| **Internet client** | The public routes in §3 and the OIDC login | Signing in without an Authentik account; acting on a session or CSRF token they don't hold. |
| **Competitor with root on a pod VM** | Everything its guest agent reports: interface names and addresses, host name, command output, exit codes | Using agent replies to inject markup into Battleship pages, steer a job at another VM, or make Battleship run commands they choose. |
| **Red team** | The same guest reach as a competitor, across many pods | As above. |
| **Signed-in volunteer** (trusted, possibly careless or curious) | Every page; their own Proxmox ACLs | Acting in Proxmox as anyone but themselves, or doing more than their own Proxmox ACLs allow. Seeing other volunteers' jobs is allowed (§8). |
| **Anyone who can push to `main`** | The image that runs with every signed-in volunteer's ticket (§5) | Not mitigated by code; only by who holds push rights. |

**Assets, worst loss first:** acting in Proxmox as another person (their ticket or a job credential); the seal key and OIDC client secret, which unlock every stored ticket; Battleship's availability during an event; job history.

## 2. Principals & Credential Delegation

- **Signed-in volunteers (`auth.User` + `proxmox.Credential`):**
  - Authentik OIDC policy bindings gate who may sign in (`login` and `callback` in `internal/auth/login.go`), using Authorization Code + PKCE (`S256`), `state`, `nonce`, and ID token verification in `Service.identify` (`internal/auth/identity.go`) via `s.verifier`: `iss`, `aud` against `oidc.client_id`, signature, expiry, `nonce`, and `at_hash` when present.
  - A second OpenID exchange against Proxmox (`proxmoxStart` and `proxmoxCallback` in `internal/auth/proxmoxlogin.go`) verifies that `sess.Username` is non-empty and binds the returned Proxmox ticket by exact byte-for-byte equality (`cred.User != sess.Username + "@" + s.cfg.Proxmox.Realm`, with no Unicode or case folding).
- **No ambient Proxmox service credential:**
  - `battleship serve` refuses to start if `BATTLESHIP_PROXMOX_TOKEN_ID` / `BATTLESHIP_PROXMOX_TOKEN_SECRET` are set (`noServiceToken` in `cmd/battleship/serve.go`) or if `token_id` / `token_secret` appear in `battleship.toml` (`removedKeys` in `internal/config/config.go`).
  - Every Proxmox API call in previews, confirms, per-user live status views (`status.Views.Open`, keyed by `cred.User`), and background job workers (`Worker.runClaimed` in `internal/jobs/worker.go`) binds a user credential via `proxmox.Client.As` or `AsSource`. An unbound or empty credential fails closed with `proxmox.ErrNoCredential` (enforced in `doServed` in `internal/proxmox/failover.go` and in `RenewTicket`).
- **Job credentials:** confirming a job seals the submitter's ticket into `job_credentials` (`internal/jobs/credential.go`); the worker renews it while the job waits and runs. The schema and `Credentials.Open` also accept `kind = 'token'` (a person's own API token), but nothing in production creates one: sign-in only yields tickets.
- **Retry and confirm act as the clicker:** any signed-in volunteer may retry any job (`retryJob` in `internal/web/jobpages.go`), unlike cancel (§3). Retry and confirm (`opConfirm` in `internal/web/confirm.go`) re-plan and submit with the **acting user's** own ticket, through the usual preview and typed confirmation, never with the original submitter's credential, so Proxmox ACLs decide what the retry may do.

## 3. Trust Boundaries & Entry Points

- **Unauthenticated routes:** `/healthz`; `/readyz`, whose body is only `shutting down` or `database: unreachable`, never an internal error string (`readyz` in `internal/web/health.go`); `/favicon.ico` and `/static/{file}` (`assetRoutes` in `internal/web/assets.go`); the `/` catch-all 404 page (`notFound`); and `GET /auth/{login,callback}` (`Service.Routes` in `internal/auth/service.go`). `POST /auth/logout` checks the session and CSRF token itself (`logout` in `internal/auth/login.go`), and `GET /auth/proxmox{,/callback}` require `s.liveSession`.
- **Reverse proxy boundary (`forwarded` in `internal/web/middleware.go`):** strips incoming `X-Forwarded-For`, `X-Forwarded-Proto`, `X-Forwarded-Host`, `Forwarded`, and `X-Real-Ip`, and only parses `X-Forwarded-For` (right-to-left hop walk) and `X-Forwarded-Proto` when `r.RemoteAddr` is within `web.trusted_proxies`.
- **Browser request & state-change boundary:**
  - `limitBody` (`internal/web/middleware.go`) caps request bodies at 1 MiB before any handler or CSRF form parse runs.
  - Every route registered by `volunteerRoutes` (`internal/web/server.go`) is wrapped in `s.require` → `auth.Service.RequireUser` (`internal/auth/middleware.go`), requiring a live session, periodic OIDC refresh-token revalidation (`web.session_refresh`), and a usable Proxmox ticket.
  - Every method except `GET` and `HEAD` passes `Service.csrfProblem` (`internal/auth/middleware.go`; also called by `logout`): any `Origin` header must match `s.origin`, and the 32-byte per-session `csrf_token` is compared with `subtle.ConstantTimeCompare`.
  - `opConfirm` loads `jobs.Inputs` from the server-stored `store.Preview` row keyed by `(session_id, sha256(nonce))` rather than trusting hidden form fields, re-plans against the cluster, verifies `jobs.Fingerprint(plan) == prev.Fingerprint`, enforces `TypedConfirm()` server-side, and atomically claims the preview (`claimPreview` in `internal/store/previews.go`) inside `store.CreateJob`.
  - `cancelJob` / `mayCancel` (`internal/web/jobpages.go`) allow cancellation only when `j.CreatedBy == actor(u)` or the caller holds `pods.HoldsOffered` on every item's VM (`/vms/<vmid>`) or target pool (`/pool/<pool>`).
- **Redirect & header boundary:**
  - Post-login, error-page, and `<meta http-equiv="refresh">` targets are validated by `safeNext` / `auth.LocalURL` (`internal/auth/next.go`; `view.Refresh` and `AuthPage` in `internal/web/render.go`).
  - `proxmoxStart` checks that the redirect URL Proxmox returns has an `http`/`https` scheme and a non-empty `state` parameter. OIDC logout redirects to `s.endSessionURL`, discovered from `oidc.issuer` and validated by `webURL` (`internal/auth/service.go`).
  - `contentSecurityPolicy` and `s.secure` (`internal/web/middleware.go`) set `Content-Security-Policy: default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; form-action 'self' <issuerOrigin>`, `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin`, and `Strict-Transport-Security` under HTTPS.
- **External control planes (Proxmox API & Authentik IdP):**
  - Proxmox TLS certificates are verified against `proxmox.ca_file` unless `proxmox.insecure_skip_verify` is explicitly set (`ReadCAFile` and `RequireTLSVerify` in `internal/config/config.go`, `proxmox.New`).
  - Proxmox path segments use `url.PathEscape` (`internal/proxmox/vms.go`, `internal/proxmox/tasks.go`), and responses are capped at 32 MiB (`maxAnswer` in `internal/proxmox/client.go`), with tighter caps per agent call (`c.capped` in `internal/proxmox/agent.go`).
- **Untrusted VM data (notes and configs):** `pods.NotesChanges` (`internal/pods/vmconfig.go`) strips `description` from cloned pod VMs so base-image credentials never reach team VMs; `CheckDetails`, `dropFormat`, `ParseDetails`, and `CheckDescription` (`internal/pods/details.go`) strip Unicode `Cf` format characters and bound note sizes.

## 4. Hostile Guests (QEMU Guest Agent)

Competitors and red team hold root in pod VMs, so every guest-agent reply is attacker-controlled. Battleship never SSHes into Proxmox nodes or guests; it reaches guests only through the Proxmox guest agent API (`internal/proxmox/agent.go`), as the user whose job or page caused the call.

- **What Battleship runs in guests is fixed:** the setup script built by `GuestCommand` / `GuestScript` (`internal/pods/guest.go`) from values `CheckSSHKey` and `pods.Guest.Check` validate first (host name, domain, IPv4 address, prefix, MAC via `macRE`, canonical OpenSSH CA key), sent on stdin to `/bin/sh -c <linuxLocked>` or as `powershell.exe ... -EncodedCommand`; `pods.SetupStateCommand`; and the read-only `SSHCheckCommand`, which runs on environment hosts when a volunteer opens an environment page (`checkHostSSH` in `internal/pods/liveaddr.go`). No guest reply is ever turned into a command.
- **What guests send back is bounded and reduced:** agent replies are size-capped (`maxExecStatus`, `maxSSHCheckOutput`, `c.capped`). Addresses are parsed into `netip.Prefix` and filtered (`guestAddr`); `CheckSSHOutput` reduces SSH-check output to one of a fixed set of warnings; `verifyGuest` quotes the reported host name with `%q`; failed guest commands report only their last few lines of output (`lastLines`).
- **Rendering:** guest-supplied text reaches pages only through `html/template`. The only `template.HTML` values are `keepNames`, which escapes its input, and fixed SVG icons; the CSP above forbids inline script.

## 5. Build & Deploy Path

Whoever controls the image controls every signed-in volunteer's Proxmox ticket, so the deploy path is part of the trust boundary.

- A push to `main` runs `.github/workflows/ci.yml`: `test` (with `govulncheck`, informational only), then `publish` builds and pushes the public image with the workflow `GITHUB_TOKEN`, then `deploy` pins the new image in the Argo CD repo using `ARGOCD_REPO_TOKEN`, and Argo CD rolls it out.
- `ARGOCD_REPO_TOKEN` is a secret of the `production` environment, whose deployment-branch policy admits only `main`, so pull-request and other branch runs never see it. The workflow token defaults to read-only; jobs that publish request `packages: write` themselves. Every action is pinned to a commit SHA (enforced by the repository's Actions settings), and Dependabot proposes updates to actions, Go modules, and the base image.
- Workflows run on `push` and `pull_request` only, never `pull_request_target`; fork pull requests from first-time contributors need approval before they run, and never receive secrets.
- **Gap:** the `main` ruleset blocks only deletion and force-pushes. It requires no pull request, review, or passing checks, so any repository admin can push straight to `main` and deploy.

## 6. Crown-Jewel Assets & Secret Invariants

- **Domain-separated AEAD sealing (`internal/seal/seal.go`):**
  - `seal.NewKey(secret, purpose)` derives a 32-byte subkey via `HMAC-SHA256(secret, purpose)` and seals plaintexts as `"v1:" + base64url(nonce || AES-256-GCM(plain, AAD = purpose + "\x00" + binding))` with a 12-byte `crypto/rand` nonce and strict unpadded base64url decoding.
  - `oidc.client_secret` (`BATTLESHIP_OIDC_CLIENT_SECRET`) derives keys for OIDC login state cookies (`"battleship oidc login cookie v1"`, AAD `loginCookie`, `internal/auth/loginstate.go`), session refresh and ID tokens (`"refresh token v1"`, AAD `sessionID` and `"id token " + sessionID`, `internal/auth/refreshtoken.go`), and session Proxmox tickets and CSRF tokens (`"proxmox ticket v1"`, AAD `sessionID + "\x00ticket"` and `sessionID + "\x00csrf"`, `internal/auth/ticket.go`).
  - `database.seal_key` (`BATTLESHIP_SEAL_KEY`) derives the key for `job_credentials` (`credentialPurpose`, AAD `"job:" + jobID`) and the startup key check (`Credentials.CheckKey`, binding `"check"`, plaintext `keyCheckText`), all in `internal/jobs/credential.go`.
- **One-way token hashing at rest:** raw 32-byte session tokens, Proxmox OpenID state tokens, and preview nonces are never stored in PostgreSQL, only their SHA-256 hex digests (`auth.HashToken`; `proxmoxStart` and `proxmoxCallback`; `storePreview` in `internal/web/preview.go`).
- **Credential lifecycle & redaction:**
  - The PostgreSQL trigger `jobs_end_credentials` (`internal/store/migrations/006_delegated_auth.sql`) deletes a job's `job_credentials` row the moment the job leaves `pending`/`running`, and `store.RenewCredential` deletes lapsed credentials.
  - Ending a session queues its OIDC refresh token in `revocations` in the same SQL statement (`EndSession` in `internal/store/sessions.go`) for background revocation at the IdP (`RunRevocations`, `DrainRevocations`, and `revoke` in `internal/auth/login.go`).
  - `seal.Key` and `proxmox.Credential` redact secret material in `String`, `GoString`, and `Format`; `config.OIDC` and `config.Database` redact secrets in `String` and `GoString`.

## 7. Security-Critical Invariants & Sharp Edges

- **UI privilege checks vs. enforcement:** `mayAnywhere`, `mayOn`, `lacks`, and `mayRun` (`internal/web/privileges.go`) only hide or disable UI controls; `pods.BlockUnpermitted` (`internal/pods/access.go`) and Proxmox's own RBAC on the bound user ticket are the authoritative gates.
- **Test-fake fallbacks must stay test-only:** `asUser` (`cmd/battleship/usertoken.go`), `accessFor` (`internal/web/server.go`), and `mayCancel` bypass credential or permission binding only when `pods.API` is not `*proxmox.Client` / `pods.PermissionReader` (unit-test fakes). Production startup (`newProxmoxAPI` in `cmd/battleship/main.go`) must always construct `*proxmox.Client`.
- **Bounded SSE stream lifetime:** event streams (`Server.stream` in `internal/web/sse.go`) end after `streamLifetime`, derived from `web.session_refresh`, so revoked sessions are forced back through `RequireUser` on reconnect.
- **Liveness pool isolation:** `store.Store` keeps `s.liveness` apart from `s.pool` so web query load cannot starve `/readyz` or worker heartbeats (`Worker.heartbeat` in `internal/jobs/worker.go`); `heartbeat` must never issue `s.pool` queries synchronously on its loop.

## 8. Explicit Non-Goals & Accepted Risks

- **Shared audit log & lab VM login notes across signed-in volunteers:** every volunteer Authentik lets sign in can view the job audit trail (`/logs`, `/logs/{id}`) and stored `jobs.inputs` / `jobs.plan`, including `[battleship]` base-image and environment host login notes such as `ImageDetails.Password` written to Proxmox VM descriptions. Those notes hold lab guest credentials for volunteers, not infrastructure secrets.
- **No local user/role table:** Battleship keeps no application-level RBAC table (`internal/store/migrations/006_delegated_auth.sql`), delegating coarse entry control to Authentik and resource authorization to Proxmox ACLs.
- **Compromised control planes or host secrets:** Battleship does not defend against a compromised Proxmox cluster, Authentik IdP, GitHub repository admin, PostgreSQL database together with `BATTLESHIP_SEAL_KEY` / `BATTLESHIP_OIDC_CLIENT_SECRET`, or unauthenticated network floods above PostgreSQL pool limits and `apply.Limits` cluster concurrency slots.
