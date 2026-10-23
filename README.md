# battleship

Deploys, resets, powers, snapshots and tears down VM pods, dev environments and base images on Proxmox.

- **Every operation is a job.** Preview a plan, confirm it, and a worker runs it (re-planning first and refusing to run if the cluster changed).
- **No service token.** Every Proxmox call runs with the signed-in user's own ticket, so Proxmox ACLs decide who may do what.
- **Safe re-runs & live updates.** Steps check VM state first, cancelled/failed jobs clean up half-built VMs, and pages update live via server-sent events.

See [`ARCHITECTURE.md`](ARCHITECTURE.md) for the job lifecycle and package layout.

## Screenshots

### Pod Grid
![Pod grid with live status and multi-cell selection](screenshots/grid.png)

### Live Job Progress
![Running deploy job across 32 pods](screenshots/job.png)

### Environments
![Dev environments with live guest-agent addresses](screenshots/environments.png)

### Base Images
![Base image catalogue grouped by category](screenshots/images.png)

## Running

```sh
cp battleship.example.toml battleship.toml   # set proxmox.urls and site settings
export BATTLESHIP_DATABASE_URL=... BATTLESHIP_SEAL_KEY=... BATTLESHIP_OIDC_CLIENT_SECRET=...
battleship -log-format json
```

See [`battleship.example.toml`](battleship.example.toml) for all settings and [`deploy/README.md`](deploy/README.md) for Docker Compose and Kubernetes manifests.

**Authentik setup:**
1. Create a confidential OAuth2/OpenID provider with redirect URI `<base_url>/auth/callback` and scopes `openid`, `email`, `profile`, `offline_access`.
2. Create an application `battleship` and bind policies for volunteers.
3. Add `<base_url>/auth/proxmox/callback` to the redirect URIs of the provider behind Proxmox's OpenID realm (`proxmox.realm`).
4. Set `oidc.issuer`, `oidc.client_id` and `BATTLESHIP_OIDC_CLIENT_SECRET`.

## Tests

```sh
docker run -d --name battleship-pgtest -e POSTGRES_PASSWORD=test -p 127.0.0.1:55432:5432 postgres:16-alpine
export BATTLESHIP_TEST_DATABASE_URL='postgres://postgres:test@127.0.0.1:55432/postgres?sslmode=disable'
go test -race ./...
```

Set `BATTLESHIP_BROWSER=http://127.0.0.1:9222` (e.g. `chromedp/headless-shell`) to run the headless Chrome tests.
