# Deploying battleship

## Docker Compose

Two containers: `battleship serve` and `postgres:16`. Mount `battleship.toml`
and the Proxmox CA at `/config`, and pass the secrets in the environment:

```yaml
services:
  battleship:
    image: battleship:<tag>          # docker build -t battleship:<tag> .
    command: ["serve", "-log-format", "json"]
    env_file: .env                   # BATTLESHIP_SEAL_KEY, BATTLESHIP_OIDC_ISSUER/CLIENT_ID/SECRET
    environment:
      BATTLESHIP_DATABASE_URL: postgres://battleship:${POSTGRES_PASSWORD}@db:5432/battleship?sslmode=disable
    volumes:
      - ./battleship.toml:/config/battleship.toml:ro
      - ./pve-root-ca.pem:/config/pve-root-ca.pem:ro   # /etc/pve/pve-root-ca.pem from any node
    stop_grace_period: 16m
    depends_on: { db: { condition: service_healthy } }
  db:
    image: postgres:16
    environment: { POSTGRES_DB: battleship, POSTGRES_USER: battleship, POSTGRES_PASSWORD: "${POSTGRES_PASSWORD}" }
    volumes: [ ./postgres-data:/var/lib/postgresql/data ]
    healthcheck: { test: ["CMD-SHELL", "pg_isready -U battleship -d battleship"], interval: 5s }
```

The CLI runs inside it: `docker exec -it battleship battleship jobs list`.

## Kubernetes

`k8s/base` has the Deployment (2 replicas), Service, PDB, Ingress,
NetworkPolicy and ConfigMap; `k8s/postgres` a CloudNativePG cluster;
`k8s/overlays/example` an overlay to copy. Then:

1. Create the Secret (it isn't in kustomize):
   `kubectl -n battleship create secret generic battleship-secrets --from-literal=seal-key="$(openssl rand -base64 48)" --from-literal=oidc-client-secret=...`
2. In your overlay set the image tag, Ingress host, `battleship.toml`
   (`proxmox.urls`, `web.base_url`, `web.trusted_proxies`, `oidc.client_id`)
   and add `pve-root-ca.pem`. Fix the NetworkPolicy CIDRs.
3. Label nodes with their Proxmox host
   (`topology.example.org/proxmox-host=<node>`) so replicas spread.
4. `kubectl apply -k deploy/k8s/overlays/<env>`.

Keep `terminationGracePeriodSeconds` (960) above `web.shutdown_timeout`
(15m), so a stopping replica can finish its jobs' cleanup. Turn ingress
buffering off for the event streams.

## Notes

- **Seal key.** Every process that submits or runs jobs needs the same
  `BATTLESHIP_SEAL_KEY`; a process with another key refuses to start.
- **Connections.** Each process uses up to `database.max_conns` + 4.
- **Migrations** run at startup and only forward: back up the database
  before upgrading.
- **No service token.** `battleship serve` refuses to start with
  `BATTLESHIP_PROXMOX_TOKEN_ID` set; users act with their own Proxmox
  permissions.

| Symptom | Likely cause |
|---|---|
| `can't start` in the logs | the logged reason: config, database URL, or the OIDC issuer unreachable |
| `/readyz` 503 | Postgres unreachable or out of connections |
| "Proxmox sign-in failed" | `<base_url>/auth/proxmox/callback` missing from the Proxmox realm's provider; the node's journal has the reason |
| Previews block everything | the user's Proxmox groups lack the privilege |
| Live updates stall | proxy buffering on the event streams |
