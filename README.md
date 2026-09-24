# teleport-machine-identity-k8s

A pod with no secret in it that reaches PostgreSQL, MySQL, MongoDB, Redis, ClickHouse,
Cassandra and Oracle through the Teleport proxy. A `tbot` sidecar holds the pod's Machine
ID and opens one authenticated tunnel per database on localhost; a small Go page connects
to those local ports with ordinary drivers and shows what each server says about who
connected. The page itself is opened through Teleport Application Access.

Verified against Teleport 18.11.1 (Enterprise) on k3s.

## Why this matters

- **The application has no Teleport code and no credential.** It connects to `127.0.0.1`
  with stock drivers, no TLS and no password. The sidecar turns each local port into a
  mutual-TLS session to the proxy using a one-hour certificate it renews itself.
- **The database account is the bot's own identity.** Where the engine supports it,
  Teleport creates the account `bot-db-status` on first connect and grants it a role.
  Nobody wrote a password anywhere. The page shows the name as the server reports it.
- **Every query is a recorded session attributed to the bot**, in the same audit log as
  your people. `tctl lock --user=bot-db-status` turns every row on the page red within
  one reconnect.
- **Machine ID is the way in; Workload Identity is the way out.** A SPIFFE identity issued
  by Teleport Workload Identity proves who a pod is to AWS or to a peer, but it cannot open
  a session through the proxy: the proxy admits only User CA certificates, which is what
  humans and bots hold. This repo shows the way in.

## The components

| Component | Where it runs | What it does | File |
|---|---|---|---|
| Bot and join token | Teleport | Bot `db-status`, joined with the pod's own projected ServiceAccount token (`kubernetes` join). The token holds the cluster's public signing keys, so nothing is distributed to the pod. | `scripts/make-token.sh` |
| Bot role | Teleport | Connect to databases labelled `env: demo` as `bot-db-status` (auto-provisioned where supported), or as the fixed Redis account. Nothing else in Teleport. | `teleport/role-db-status.yaml` |
| `tbot` sidecar | the pod | Joins, keeps a one-hour certificate in memory, and runs one `database-tunnel` per database: a plain TCP listener on `127.0.0.1:<port>` that becomes a mutual-TLS session to the proxy. | `k8s/configmap.yaml` (`tbot.yaml`) |
| The page | the pod | Connects to each local port with the engine's normal driver, asks "who am I" and "what version are you", shows the result and the sidecar's own health. | `main.go`, `probe.go`, `web.go`, `index.html`; `k8s/configmap.yaml` (`databases.yaml`) |
| App registration | Teleport | Publishes the page through Application Access, so it opens in a browser behind Teleport login and every visit is an app session. Served by an App Service inside the cluster. | `teleport/app.yaml` |
| Database accounts | the databases | PostgreSQL, MySQL and MongoDB: created by Teleport on first connect. Redis: one fixed account. ClickHouse and Oracle: a certificate-mapped account created once by an administrator. Cassandra: no accounts; the identity is the one Teleport checked. | your database |

**Machine ID** is Teleport's identity for software: a *bot* is the identity, `tbot` is the
agent that holds it, and a *join method* is how the agent proves it deserves it. Here the
proof is the Kubernetes ServiceAccount token the kubelet mounts into the pod.

## How it works

```
 browser ──App Access──► Teleport proxy ──► App Service in the cluster ──► db-status:8080
                                                                              │
 pod db-status ───────────────────────────────────────────────────────────────┤
 │  app          connects to 127.0.0.1:15432 / 13306 / 27018 / 16379 / 19000 / 19042 / 11521
 │  tbot         one database-tunnel per port ──mTLS, bot certificate──► proxy ──► Database Service ──► database
 └  projected ServiceAccount token (audience = Teleport cluster) ──► kubernetes join
```

1. **Join.** tbot reads the projected token, sends it to the Teleport Auth Service, which
   verifies it against the signing keys pinned in the join token and checks the
   ServiceAccount name. It issues the bot a one-hour certificate, held in memory.
2. **Tunnel.** For each database, tbot listens on a localhost port. When the app connects,
   tbot opens a TLS connection to the proxy with the bot certificate and a route saying
   which database, which user and which database name. The proxy hands it to the Database
   Service that fronts that database.
3. **Connect.** The Database Service checks the bot's role (labels, user, name), opens the
   backend connection with its own credentials, creates the account if the engine supports
   provisioning, and relays the protocol. The app sees a plain, unauthenticated local
   connection; the database sees a session for `bot-db-status`.
4. **Audit.** Each connection is a `db.session.start` event with `user: bot-db-status`,
   the database, and the database user. Queries are recorded per engine capability.

Every page probe is a fresh session, so the audit log gets one row per database per
refresh. The first connection costs a TLS handshake through the proxy, measured at one to
two seconds per database on this setup; results are cached for 15 seconds.

## Install

You need Teleport Enterprise 18.x with `tsh` and `tctl` logged in as `editor`, one or more
databases enrolled with the label `env: demo`, and `kubectl` on the target cluster.
Replace `example.teleport.sh` with your proxy.

```bash
tsh login --proxy=example.teleport.sh:443

# Teleport side, once: role, join token (built from your current kubectl context), bot
tctl create -f teleport/role-db-status.yaml
scripts/make-token.sh | tctl create -f -
tctl bots add db-status --roles=db-status --token=db-status

# Edit k8s/configmap.yaml: the `service:` of each tunnel is your Teleport database name;
# delete the tunnels (and the matching databases.yaml lines) you do not have.

# The pod
PROXY_ADDR=example.teleport.sh:443 scripts/render.sh | kubectl apply -f -
kubectl -n db-status rollout status deploy/db-status     # Ready = joined and every tunnel holds a certificate

# The page, through Teleport
tctl create -f teleport/app.yaml                          # set its labels to your in-cluster App Service's selector
```

Without an App Service in the cluster, `kubectl -n db-status port-forward svc/db-status 8080`
and `http://localhost:8080` show the same page.

`render.sh` asks the proxy which tbot version to use; set `TBOT_VERSION=18.11.1` if your
machine cannot reach it.

## Try it

Open the page. One row per database:

```
engine      Teleport database   via                      connected as (server's view)   server              status
cassandra   cassandra           127.0.0.1:19042          bot-db-status (checked by Teleport)  Cassandra 5.0.8   ✓ ok
clickhouse  clickhouse          127.0.0.1:19000          bot-db-status                  ClickHouse 24.8.14  ✓ ok
mongodb     mongodb             127.0.0.1:27018 / demo   CN=bot-db-status@$external     MongoDB 7.0.37      ✓ ok
mysql       mysql               127.0.0.1:13306 / demo   bot-db-status@%                8.4.10              ✓ ok
oracle      oracle              127.0.0.1:11521 / FREEPDB1  BOT-DB-STATUS               Oracle 26ai Free    ✓ ok
postgres    postgres            127.0.0.1:15432 / demo   bot-db-status                  PostgreSQL 16.14    ✓ ok
redis       redis               127.0.0.1:16379          demo                           Redis 7.4.9         ✓ ok
```

The "connected as" column is each server's own answer (`current_user`, `CURRENT_USER()`,
`connectionStatus`, `ACL WHOAMI`, `currentUser()`, `SYS_CONTEXT('USERENV','SESSION_USER')`).
MongoDB's answer is the certificate subject Teleport used to authenticate on the bot's
behalf; Oracle's is the certificate-mapped account, upper-cased as Oracle does. Below the table, the sidecar's
`/readyz` lists every tunnel and its health.

Then the audit log and the kill switch:

```bash
tctl audit query exec 'SELECT event_time, db_service, db_user, db_name FROM db_session_start WHERE "user" = '"'"'bot-db-status'"'"' ORDER BY event_time DESC LIMIT 10'

tctl lock --user=bot-db-status --ttl=2m       # reload: every row fails within seconds
tctl rm lock/<id>                              # or wait two minutes; the rows recover without a restart
```

The sidecar's `/readyz` stays healthy while locked: its certificate is still valid, and the
proxy refuses each new connection because the lock names the bot. Measured: all seven rows
failed 15 seconds after the lock, and all seven recovered 10 seconds after its removal.

`kubectl -n db-status get secrets` prints `No resources found`: there is no Secret, no
password and no kubeconfig anywhere in the namespace.

## 5-minute demo script

1. `kubectl -n db-status get secrets` → "No resources found. The app is a stock database
   client pointed at localhost."
2. Open the page → "Seven databases, seven engines, one identity. Each server names the
   bot as the connected user."
3. `tctl bots instances ls` → "The pod is a Teleport identity, joined with its own
   ServiceAccount token. Nothing was copied into it."
4. The audit query above → "Every one of those connections is a recorded session for
   `bot-db-status`, same log as the humans."
5. `tctl lock --user=bot-db-status --ttl=2m`, reload → "One lock, every database gone."
6. `tctl get role/db-status` → "The whole policy: these labels, this user, this database.
   Change it here and every tunnel follows on its next connection."

## Troubleshooting

- tbot exits with `validating service[N]: database: should not be empty`: every
  `database-tunnel` needs a `database:`, even for engines where Teleport does not enforce
  it. Use any value for Redis, ClickHouse, Cassandra.
- MongoDB row: `access to db denied ... Confirm database user and name` and the audit log
  shows the failure on database `admin`: drivers open every connection with a handshake on
  `admin`, and Teleport checks each message's database. Add `admin` to the role's
  `db_names`, as the shipped role does.
- Oracle row: `ORA-01017: invalid credential`: the client sent a username and password.
  Oracle behind Teleport uses an external logon (no user, no password); Teleport's
  certificate names the account. The shipped probe does this already.
- Redis row shows `default` instead of your account: the client did not send `AUTH`. Redis
  only authenticates a client that sends a password, so the probe sends a placeholder.
- tbot `CrashLoopBackOff` with a JWT verification error after a cluster rebuild: the
  cluster's signing keys changed. Run `scripts/make-token.sh | tctl create --force -f -`.
- A row shows `connection refused`: the tunnel for that port is not listening. Check
  `kubectl -n db-status logs deploy/db-status -c tbot` for the service named
  `database-tunnel-N` (N counts from 1 in `tbot.yaml` order).

## Day two

- **Add a database**: one tunnel in `tbot.yaml`, one line in `databases.yaml`, and the
  label or name in the role if it does not already match.
- **Rotate**: nothing to do. The sidecar renews its certificate at half-life; a restarted
  pod re-joins with a fresh ServiceAccount token.
- **Revoke**: `tctl lock --user=bot-db-status` stops every tunnel within one reconnect.
- **Another cluster**: the same manifests. Each cluster needs its own join token, since
  the token pins that cluster's signing keys; give the bot a second token or a second bot.
- **Add SSH or Kubernetes access**: tbot's `ssh-multiplexer` and `kubernetes/v2` services
  fit the same sidecar; the role would gain `node_labels` or `kubernetes_labels`.

## License

Apache-2.0.
