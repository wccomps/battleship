# Threat Model

Battleship turns volunteer requests into queued jobs that run against Proxmox using the submitter's own Proxmox ticket. [`ARCHITECTURE.md`](ARCHITECTURE.md) describes how jobs, locks, and packages fit together; this document records the security boundaries, secret invariants, sharp edges, and accepted risks that code changes must preserve.

## 1. Principals & Credential Delegation

- **Unauthenticated clients:** May reach only `/healthz`, `/readyz` (which returns static `"database: unreachable"`, never internal error strings: `internal/web/health.go:27-55`), `/static/*`, `/favicon.ico`, and `/auth/{login,callback}` (`internal/web/server.go:204-239`, `internal/auth/service.go:118-126`).
- **Signed-in volunteers (`auth.User` + `proxmox.Credential`):**
  - Authentik OIDC policy bindings gate who may sign in (`internal/auth/login.go:52-131`), using Authorization Code + PKCE (`S256`), `state`, `nonce`, and verified `aud`/`azp` claims.
  - A second OpenID exchange against Proxmox (`internal/auth/proxmoxlogin.go:35-152`) binds the returned Proxmox ticket to the exact case-sensitive username `<preferred_username>@<proxmox.realm>` (`internal/auth/proxmoxlogin.go:132`), after restricting `preferred_username` to printable ASCII without `@` or whitespace (`internal/auth/identity.go:136-152`).
- **No ambient Proxmox service credential:**
  - `battleship` refuses to start if `BATTLESHIP_PROXMOX_TOKEN_ID` / `BATTLESHIP_PROXMOX_TOKEN_SECRET` are set in the environment (`noServiceToken` in `cmd/battleship/serve.go:286-294`) or in `battleship.toml` (`internal/config/config.go:352-364`).
  - Every Proxmox API call in previews, confirms, per-user live status views (`internal/status/views.go:19-106`, keyed by `cred.User`), and background job workers (`internal/jobs/worker.go:199-210`) binds a user credential via `proxmox.Client.As` or `AsSource` (`internal/proxmox/client.go:48-55`, `internal/proxmox/failover.go:46-54`). An unbound or empty credential fails closed with `proxmox.ErrNoCredential` (`internal/proxmox/client.go:297-302`).
  - Job retry (`internal/web/jobpages.go:1120-1158`) and confirm (`internal/web/confirm.go:29-146`) re-plan and submit using the **acting user's** own Proxmox ticket, never a previous submitter's credential.

## 2. Trust Boundaries & Entry Points

- **Reverse proxy boundary (`internal/web/middleware.go:84-133`):** `forwarded` unconditionally strips incoming `X-Forwarded-For`, `X-Forwarded-Proto`, `X-Forwarded-Host`, `Forwarded`, and `X-Real-Ip` headers and only parses `X-Forwarded-For` (right-to-left hop walk) and `X-Forwarded-Proto` when `r.RemoteAddr` is within `web.trusted_proxies`.
- **Browser request & state-change boundary:**
  - `limitBody` (`internal/web/middleware.go:27-51`) caps request bodies at 1 MiB (`maxBody = 1 << 20`) before any handler or CSRF form parse runs.
  - Every route outside `/healthz`, `/readyz`, `/static/`, and `/auth/` wraps `s.require` (`internal/web/privileges.go:79-85` → `auth.Service.RequireUser` in `internal/auth/middleware.go:56-139`), requiring a live session, periodic OIDC refresh-token revalidation (`web.session_refresh`), and a usable Proxmox ticket.
  - Every method except `GET` and `HEAD` enforces `Service.csrfProblem` (`internal/auth/middleware.go:257-276`; also applied to `POST /auth/logout` via `RequireCSRF` at `internal/auth/service.go:123`), requiring both a same-origin `Origin`/`Referer` match against `web.base_url` (when present) and a `subtle.ConstantTimeCompare` match of the 32-byte per-session CSRF token.
  - `opConfirm` (`internal/web/confirm.go:29-146`) loads `jobs.Inputs` from the server-stored `store.Preview` row keyed by `(session_id, sha256(nonce))` rather than trusting hidden form fields, re-plans against the cluster, verifies `jobs.Fingerprint(plan) == prev.Fingerprint`, enforces `TypedConfirm()` server-side, and atomically claims the preview in `store.CreateJob` (`internal/store/previews.go:53-68`).
  - `cancelJob` / `mayCancel` (`internal/web/jobpages.go:1055-1104`) authorizes cancellation only when `j.CreatedBy == actor(u)` (`internal/web/ops.go:425`) or the caller holds `pods.HoldsOffered` (`internal/pods/access.go:190-202`) on every item's VM (`/vms/<vmid>`) or target pool (`/pool/<pool>`).
- **Redirect & header boundary:**
  - Post-login, error-page, and `<meta http-equiv="refresh">` targets are validated by `auth.LocalURL` (`internal/auth/next.go:13-33`, `internal/web/render.go:140,259-272`).
  - IdP redirects during Proxmox OpenID login and OIDC logout verify that the target URL's scheme and host match `oidc.issuer` (`checkIdPRedirect` in `internal/auth/proxmoxlogin.go:254-269`, `logoutRedirect` in `internal/auth/login.go:156-180`).
  - Security headers (`internal/web/middleware.go:15-25`) set `Content-Security-Policy: default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; form-action 'self' <issuerOrigin>`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin`, and `Strict-Transport-Security` under HTTPS.
- **External control planes (Proxmox API & Authentik IdP):**
  - Proxmox TLS certificates are verified against `proxmox.ca_file` unless `proxmox.insecure_skip_verify` is explicitly set (`internal/config/config.go:669-676`, `internal/proxmox/client.go:64-97`).
  - Proxmox path segments use `url.PathEscape` (`internal/proxmox/vms.go`, `internal/proxmox/tasks.go`), and responses are capped at 16 MiB (`maxResponseBytes` in `internal/proxmox/client.go:28`).
- **Untrusted cluster data & guest execution (Proxmox QEMU Guest Agent):**
  - Battleship never SSHes into Proxmox nodes or guest VMs; guest setup and live checks run via the Proxmox QEMU Guest Agent API (`AgentExec` in `internal/proxmox/agent.go`).
  - VM `description` notes and configs are treated as untrusted input: `pods.NotesChanges` (`internal/pods/vmconfig.go:192-198`) strips `description` from cloned pod VMs so base-image credentials never leak to team VMs; `pods.ParseDetails` (`internal/pods/details.go:122-177`) strips Unicode `Cf` format characters and bounds note sizes.
  - `pods.Guest.Check` and `CheckGuestSetup` (`internal/pods/guest.go:30-108`) validate every hostname, domain, IPv4 address, prefix, MAC (`macRE`), and canonical OpenSSH CA public key before passing positional arguments to Linux `sh -ceu` (`internal/pods/guest.go:141-219`) or building Windows PowerShell setup scripts (`internal/pods/guest.go:278-357`).

## 3. Crown-Jewel Assets & Secret Invariants

- **Domain-separated AEAD sealing (`internal/seal/seal.go`):**
  - `seal.NewKey(secret, purpose)` derives a 32-byte subkey via `HMAC-SHA256(secret, purpose)` and seals plaintexts as `"v1:" + base64url(nonce || AES-256-GCM(plain, AAD = purpose + "\x00" + binding))` with a 12-byte `crypto/rand` nonce and strict unpadded base64url decoding (`internal/seal/seal.go:33-82`).
  - `oidc.client_secret` (`BATTLESHIP_OIDC_CLIENT_SECRET`) derives keys for OIDC login state cookies (`"battleship oidc login cookie v1"`, AAD `"battleship_oidc"`, `internal/auth/loginstate.go:28-58`), session refresh/ID tokens (`"refresh token v1"`, AAD `sessionID` and `"id token " + sessionID`, `internal/auth/refreshtoken.go:9-50`), and session Proxmox tickets/CSRF tokens (`"proxmox ticket v1"`, AAD `sessionID + "\x00ticket"` and `sessionID + "\x00csrf"`, `internal/auth/ticket.go:15-58`).
  - `database.seal_key` (`BATTLESHIP_SEAL_KEY`) derives the key for `job_credentials` (`"battleship job credential v1"`, AAD `"job:" + jobID`, `internal/jobs/credential.go:35-89`) and the startup key check (`"battleship seal check v1"`, AAD `"seal_check:1"`, `internal/jobs/credential.go:135-144`).
- **One-way token hashing at rest:** Raw 32-byte session tokens, Proxmox OpenID state tokens, and preview nonces are never stored in PostgreSQL—only their SHA-256 hex digests (`auth.HashToken` in `internal/auth/session.go:34-37`, `internal/auth/proxmoxlogin.go:57`, `internal/web/preview.go:256`).
- **Credential lifecycle & redaction:**
  - The PostgreSQL trigger `jobs_end_credentials` (`internal/store/migrations/006_delegated_auth.sql:40-47`) deletes a job's `job_credentials` row the moment the job leaves `pending`/`running`, and `store.RenewCredential` (`internal/store/credentials.go:139-140`) deletes lapsed credentials.
  - Ending a session queues its OIDC refresh token in `revocations` in the same SQL statement (`internal/store/sessions.go:127-195`) for background revocation at the IdP (`internal/auth/revoker.go`).
  - `seal.Key` and `proxmox.Credential` redact secret material in `String`, `GoString`, and `Format`; `config.OIDC` and `config.Database` redact secrets in `String` and `GoString`.

## 4. Security-Critical Invariants & Sharp Edges

- **UI privilege checks vs. enforcement:** `mayAnywhere`, `mayOn`, and `lacks` (`internal/web/privileges.go:15-74`) only hide or disable UI controls; `pods.BlockUnpermitted` (`internal/pods/access.go`) and Proxmox's own RBAC on the bound user ticket are the authoritative gates.
- **Test-fake fallbacks must stay test-only:** `asUser` (`cmd/battleship/usertoken.go:10-15`), `accessFor` (`internal/web/server.go:336-340`), and `mayCancel` (`internal/web/jobpages.go:1093-1096`) bypass credential/permission binding only when `pods.API` is not `*proxmox.Client` / `pods.PermissionReader` (used by unit-test fakes). Production startup (`newProxmoxAPI` in `cmd/battleship/main.go:50-54`) must always construct `*proxmox.Client`.
- **Bounded SSE stream lifetime:** SSE handlers (`internal/web/sse.go:184-198`, `internal/web/server.go:300-320`) close after `streamLifetime` (`web.session_refresh`) so revoked sessions are forced back through `RequireUser` on reconnect.
- **Liveness pool isolation:** `store.Store` (`internal/store/store.go:26-32`) isolates `s.liveness` from `s.pool` so web query load cannot starve `/readyz` or worker heartbeats (`internal/jobs/worker.go:267-309`); `heartbeat` must never issue `s.pool` queries synchronously on its loop.

## 5. Explicit Non-Goals & Accepted Risks

- **Shared audit log & lab VM login notes across signed-in volunteers:** All volunteers permitted by Authentik to sign into Battleship can view the job audit trail (`/logs`, `/logs/{id}`) and stored `jobs.inputs` / `jobs.plan` (including `[battleship]` base-image and environment host login notes such as `ImageDetails.Password` written to Proxmox VM descriptions). Those notes hold lab guest credentials for volunteers, not infrastructure secrets.
- **No local user/role table:** Battleship maintains no application-level RBAC table (`internal/store/migrations/006_delegated_auth.sql:18-20`), delegating coarse entry control to Authentik and resource authorization to Proxmox ACLs.
- **Compromised control planes or host secrets:** Battleship does not defend against a compromised Proxmox cluster, Authentik IdP, PostgreSQL database + `BATTLESHIP_SEAL_KEY` / `BATTLESHIP_OIDC_CLIENT_SECRET`, or unauthenticated network floods above PostgreSQL pool limits and `apply.Limits` cluster concurrency slots.
