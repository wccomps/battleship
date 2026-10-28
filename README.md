# battleship

Web app for deploying, resetting, snapshotting, and tearing down competition VM pods, dev environments, and base images on Proxmox.

Every change previews a plan first and runs as a queued job using the signed-in user's own Proxmox ticket; Battleship holds no Proxmox credentials of its own. Failed or cancelled jobs clean up any VMs they half-built, so re-running a job is safe. [`ARCHITECTURE.md`](ARCHITECTURE.md) covers how jobs, locks, and packages fit together, and [`THREAT_MODEL.md`](THREAT_MODEL.md) covers principals, trust boundaries, and secret invariants.

![Pod grid](screenshots/grid.png)

![Running deploy job](screenshots/job.png)

![Environments](screenshots/environments.png)

![Base images](screenshots/images.png)

## Running

```sh
cp battleship.example.toml battleship.toml   # set proxmox.urls and site settings
export BATTLESHIP_DATABASE_URL=... BATTLESHIP_SEAL_KEY=... BATTLESHIP_OIDC_CLIENT_SECRET=...
battleship -log-format json
```

See [`battleship.example.toml`](battleship.example.toml) for all settings and [`deploy/README.md`](deploy/README.md) for Docker Compose and Kubernetes setup.

For Authentik:
1. Create a confidential OAuth2/OpenID provider with redirect URI `<base_url>/auth/callback` and scopes `openid`, `email`, `profile`, `offline_access`.
2. Create an application `battleship` and bind policies for volunteers.
3. Add `<base_url>/auth/proxmox/callback` to the redirect URIs of the provider behind Proxmox's OpenID realm (`proxmox.realm`).
4. Set `oidc.issuer`, `oidc.client_id`, and `BATTLESHIP_OIDC_CLIENT_SECRET`.

## Tests

```sh
docker run -d --name battleship-pgtest -e POSTGRES_PASSWORD=test -p 127.0.0.1:55432:5432 postgres:16-alpine
export BATTLESHIP_TEST_DATABASE_URL='postgres://postgres:test@127.0.0.1:55432/postgres?sslmode=disable'
go test -race ./...
```

Set `BATTLESHIP_BROWSER=http://127.0.0.1:9222` (for example `chromedp/headless-shell`) to include the browser tests.
