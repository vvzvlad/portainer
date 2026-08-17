# Secret resolver: keeping secret values out of Portainer

Design document for a fork feature. Nothing here is implemented yet — this is the
handoff from the investigation that produced the design, so that whoever picks it
up does not re-derive it or re-walk the dead ends.

Everything marked **verified** was confirmed against source or by running a command;
the command or the file:function is named. Everything else is explicitly flagged.

---

## 1. The problem

Secret values (database passwords, API tokens, bot tokens) are written as literals
straight into the body of the compose files that Portainer stores in its own
database. AI agents read those bodies routinely through the Portainer API while
doing ordinary infrastructure work, so every read drops live credentials into a
model context and from there into session transcripts.

The goal is **not** to build a wall an agent cannot cross — the agents have root SSH
to these hosts anyway. The goal is to stop shoving secrets into the agent's face on
paths it traverses by default. Configs are such a path. A root-owned file the agent
has no reason to open is not.

### Requirements, as finally settled

1. **Values are not stored in Portainer** — not in the compose body, not in the
   stack's `Env` field, not anywhere in its database or volume. References are fine.
2. **The store is Vaultwarden.** Not negotiable; alternatives (sops+age, OpenBao,
   Infisical) were explored and rejected by the owner.
3. **A deploy fetches the current value at deploy time.** No pre-materialised copy
   that can drift, no "remember to re-sync before redeploying" ritual.
4. **The compose body stays vanilla and portable.** Plain `${VAR}`, so a third party
   clones the repo, writes an ordinary `.env` next to it, and runs
   `docker compose up -d` with none of our tooling.
5. **No dependency on anyone's workstation.** Deploy and rotation must work with
   every laptop switched off.
6. **The stack stays a Portainer stack** — editing and redeploying from the UI keeps
   working exactly as it does today.

---

## 2. Current state of the estate (verified)

Read-only inventory over both Portainer servers via their APIs.

| Server | Version | Environments | Stacks |
| --- | --- | --- | --- |
| borneo | CE 2.44.0, our own build | 6 | 40 |
| hatbox | EE 2.42.0, **stock** | 1 | 13 |

- All 53 stacks are **Type 2 (compose standalone)**. There is not a single swarm
  environment, so Docker swarm secrets are unavailable by construction.
- All 53 deploy from a file/string uploaded into Portainer. `GitConfig == null` and
  `AutoUpdate == null` on every one of them. Git-based deploy is used nowhere.
- `env_file:` — 0 stacks. compose/docker `secrets:` — 0 stacks.
- The stack `Env` field is populated on exactly **one** stack (hatbox
  `google-workspace-mcp`).
- **33 stacks carry secret literals in the compose body**, roughly **65 distinct
  variable names**.

### Things that will bite during migration

- **Secrets outside `environment:`.** About six stacks hide them in `command:`
  (`--api-key`, `--requirepass`), in `healthcheck.test`, inside an `entrypoint:`
  heredoc, and in traefik `labels` (`basicauth.users`, a bcrypt hash — with the
  plaintext password sitting in a neighbouring YAML comment in three cases).
  These are **not** found by grepping for `environment:`.
- **Values reused across stacks**: watchtower bot token in two, Grafana admin in
  two, InfluxDB in two, `APP_SECRET` + postgres password across two docmost stacks.
  And reused *within* a stack constantly (`DATABASE_URL` + `POSTGRES_PASSWORD` +
  `PGPASSWORD`). A registry of value → usages is needed before the first edit, or
  rotation will silently split services apart.
- **Credentials that are not in compose at all** live in named volumes (traefik
  `acme.json`, mosquitto, Home Assistant, qBittorrent). Out of scope, but they exist.

---

## 3. How Portainer actually deploys a stack (verified against source)

This is the part that determines the entire design, and it is counter-intuitive.

### 3.1 Compose runs inside the Portainer Server container. Always.

`pkg/libstack/compose/composeplugin.go` imports the compose engine as a **library**
and calls `composeService.Up(ctx, project, opts)`:

```go
"github.com/compose-spec/compose-go/v2/cli"
cmdcompose "github.com/docker/compose/v2/cmd/compose"
"github.com/docker/compose/v2/pkg/api"
"github.com/docker/compose/v2/pkg/compose"
```

There is no `exec.Command("docker-compose", …)` anywhere. The comment
`// CompomposeStackManager is a wrapper for docker-compose binary` in
`api/exec/compose_stack.go` is a stale artifact and does not describe the code.

### 3.2 Local socket vs Portainer Agent differ only in the Docker API address

`api/exec/common.go`, `fetchEndpointProxy()`:

```go
if strings.HasPrefix(endpoint.URL, "unix://") || strings.HasPrefix(endpoint.URL, "npipe://") {
    return "", nil, nil            // default socket mounted into the Portainer container
}
proxy, err := proxyManager.CreateAgentProxyServer(endpoint)
return fmt.Sprintf("tcp://127.0.0.1:%d", proxy.Port), proxy, nil
```

| Environment | Where compose runs | Docker API target |
| --- | --- | --- |
| `borneo.lc` (local socket) | Portainer container | mounted `/var/run/docker.sock` |
| `island.lc`, `nebula.lc`, `backlash`, `asakusa-infra`, `slug` (Agent) | Portainer container | `tcp://127.0.0.1:<proxy>` → agent → remote daemon |

**Consequence, and it kills a whole family of designs:** everything compose reads
*as a file* (the compose body, `.env`, `env_file:`, `secrets: file:`) resolves in the
**Portainer container's** filesystem. Only bind mounts under `volumes:` resolve on
the target host, because those are performed by the daemon. An `env_file:` pointing
at a path on the target host is therefore invisible to compose.

### 3.3 What Portainer does with `Stack.Env`

`api/exec/compose_stack.go`, `createEnvFile(stack)`:

```go
if len(stack.Env) == 0 { return "", nil }

envFilePath := path.Join(stack.ProjectPath, "stack.env")
envfile, err := os.OpenFile(envFilePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
defaultEnvPath := path.Join(stack.ProjectPath, path.Dir(stack.EntryPoint), ".env")
copyDefaultEnvFile(envfile, defaultEnvPath)   // .env content first
copyConfigEnvVars(envfile, stack.Env)         // stack.Env appended, wins
return envFilePath, nil
```

The path flows into `libstack.Options.EnvFilePath` → `cli.WithEnvFiles(...)` in
`createProject()`. If `Env` is empty, `envFiles` is empty and `cli.WithEnvFiles()`
with no arguments falls back to `<workingDir>/.env` (compose-go
`cli/options.go`). Precedence is stated in `api/stacks/stackutils/env.go`:
`OS env < .env < stack.Env`.

`workingDir` is `filepath.Dir(configFilepaths[0])` == `ProjectPath`, i.e.
`/data/compose/<id>/v<N>` — inside the `portainer_data` volume.

### 3.4 `libstack.Options.Env` — the injection point this design uses

`pkg/libstack/libstack.go`:

```go
// Env is a list of environment variables to pass to the command, example: "FOO=bar"
Env []string
```

It reaches `cli.WithEnv(options.Env)` in `composeplugin.go` and has the **highest**
precedence of all sources. The swarm manager already uses exactly this
(`api/exec/swarm_stack.go` builds `env` from `stack.Env` into `swarm.Options{Env: env}`
with no file at all); the compose manager simply never caught up.

**Values passed this way never touch disk.** No `stack.env`, no `.env`, nothing in a
backup, no transient file to clean up and no window where a crash leaves one behind.

### 3.5 What the API exposes (verified)

- `StackFileInspect` (`GET /stacks/{id}/file`) returns **only the entrypoint body** —
  `FileService.GetFileContent(projectPath, stack.EntryPoint)` in
  `api/http/handler/stacks/stack_file.go`. There is no API to read `.env` or
  `stack.env` at all.
- `StackInspect` / `StackList` return the whole `portainer.Stack`, including
  `Env []Pair` **with values**.

So the leak channels into an agent's context are exactly two: literals in the body,
and the `Env` field. Both are removed by this design.

### 3.6 Residual leak channels (not closed by this work)

| Channel | Status |
| --- | --- |
| `Config.Env` of the running containers, via Portainer's docker proxy | **Open.** No stack-level design closes this; it needs endpoint permissions for the agent. |
| `POST /backup` | Closed by using `Options.Env` — `filesToBackup` includes `compose`, so any materialised `.env` **would** be in the backup. This is why the simple ".env in the project dir" variant was rejected. |
| `Stack.DeploymentStatus[].Message` | **Risk, unverified.** `api/stacks/stackutils/stack_status.go` stores a failed deploy's `err.Error()` in the DB and `StackInspect` returns it. Compose errors normally name variables, not values, but it is not proven that a substituted value can never appear. Worth a look during implementation. |
| Deploy logs | Safe by name: compose logrus → zerolog via `pkg/libstack/compose/logwriter.go`; the notable message is `template.go` warning with the variable **name**. Container log, not exposed over the API. |

### 3.7 A latent fork bug found on the way

`api/http/handler/stacks/stack_versioning.go`, `collectStackFilesContent()` copies
only `stack.EntryPoint` and `stack.AdditionalFiles` into the new `v<N>` directory.
A `.env` placed in the project directory therefore **disappears on every redeploy**,
and compose does not fail on the resulting undefined variables — it logs a warning
and substitutes an empty string (compose-go `template/template.go`). A stack comes up
with blank passwords and looks healthy.

Independent of this feature, and worth fixing separately (~10 lines). Note the
semantics decision it forces: on `RollbackTo`, does `.env` come from the target
version (rolling secrets back too) or from the current one?

---

## 4. The design

### 4.1 Shape

```
compose body        stack.Env            resolver socket        Vaultwarden
────────────        ─────────            ───────────────        ───────────
${DB_PASSWORD:?}    DB_PASSWORD=         batch fetch, one       collection
                      vw:stack/...       request per deploy     `infra`
      │                    │                     │                   │
      └──── Portainer patch ────────────────────►│──── rbw ─────────►│
                  Options.Env (memory only)
```

- **Compose body**: plain `${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}`.
  The `:?` form matters — see §5.
- **`stack.Env`**: holds *references*, not values. UI round-trips them untouched
  (the frontend treats `Env[].Value` as an opaque string), so no TypeScript changes.
- **Portainer patch**: resolves references at deploy and injects via
  `libstack.Options.Env`. Roughly 50 lines in `api/exec/compose_stack.go`, covering
  all three entry points (`Up`, `Run`, `Pull` — every compose deploy funnels through
  them via `api/stacks/deployments/deployer.go`).
- **Resolver**: a separate service on borneo behind a unix socket.

### 4.2 The abstraction boundary is the wire, not a Go interface

Portainer must be patched **once, ever**. Its build has no backend-only path — even a
pure server change pays for pnpm, Node 22 and a production webpack run
(`.gitea/workflows/build-image.yml`) — and every fork change is a future rebase.

So the patch **does not parse the reference**. It forwards the whole string and gets
a value or an error:

```go
// Portainer never parses the scheme — routing is the resolver's job, so adding a
// backend never requires touching (and rebuilding) Portainer.
vals, err := resolver.Fetch(ctx, refs)   // ["vw:stack/nebula/arcextension/ADMIN_TOKEN", ...]
if err != nil {
    return fmt.Errorf("secret resolution failed: %w", err)  // hard fail, never a fallback
}
```

The scheme prefix (`vw:`, `vault:`, `file:`) is routed by the resolver. Adding a
backend = resolver config + restart. No Portainer rebuild, no stack changes, and
stacks can migrate piecemeal because each reference carries its own scheme.

Resolver-side:

```go
// One backend per scheme. Freshness semantics live here, not in Portainer: the
// Vaultwarden backend needs sync + mtime proof, an OpenBao backend is fresh by
// construction.
type Backend interface {
    Scheme() string
    Fetch(ctx context.Context, refs []string) (map[string]string, error)
}
```

Protocol: **batched, one request per deploy** — not an optimisation but a
requirement, because one Vaultwarden sync pulls the whole vault. All values or an
error; no partial results. A version field in the request lets the resolver evolve
without touching the fork. The resolver address is configuration (unix socket by
default, URL possible), so the resolver can move later without a patch.

### 4.3 Why `rbw` and not our own Bitwarden client

Rejected: writing the Bitwarden client crypto in Go (prelogin → PBKDF2/Argon2id →
HKDF-stretch → user key → RSA → org key → AES-CBC+HMAC). That is 400–700 lines of
security-critical code whose silent failure mode looks like a working system, to
maintain forever, for 65 secrets. Note that the closest open reference
implementation (`Turbootzz/Vaultwarden-API`) **fails on exactly our case** — org
collection items, `MAC verification failed` — because the organisation key path is
the hard part, and ours live in the org `agents`, collection `infra`.

`rbw` gets its crypto maintained by someone else. Its one real defect is handled in
§4.4. The pinentry shim it requires is ugly but inert under this threat model.

Accepted risk: `rbw` is in maintenance mode (author: "essentially feature-complete…
unlikely to spend time implementing new features"). If it breaks, writing the Go
client becomes a forced move with a clear reason, rather than an upfront bet.

### 4.4 Freshness: `rbw get` never contacts the server

**This is the crux, and it is verified three ways.**

The agent protocol (`src/protocol.rs`, dispatcher in `src/bin/rbw-agent/agent.rs`)
has `Register, Login, Unlock, CheckLock, Lock, Sync, Decrypt, Encrypt,
ClipboardStore, Quit, Version` — **there is no "fetch an entry" action**. `rbw get`
reads the local cache itself and only sends ciphertext to the agent for `Decrypt`.
`unlock()` skips the network entirely when the cache holds tokens
(`needs_login()` in `src/db.rs`).

Empirically: `rbw get` takes 0.00 s against 0.13–0.15 s for `rbw sync`; and with
`base_url` pointed at a dead port, `rbw unlock` still reaches the password check.

And there is **no freshness signal**: the cache struct carries no timestamp or
revision, there are no `--offline`/`--max-age` flags, and the exit code is `1` for
every failure. A successful `rbw get` from a stale cache is byte-identical to a fresh
one. Naively calling `rbw get` would therefore silently deploy an old password —
exactly the failure this design exists to prevent.

**The protocol that fixes it**, built on two verified behaviours — `rbw sync` fails
loudly (exit 1, explicit stderr) when the server is unreachable, and the cache file's
mtime advances on **every** successful sync even when the content is unchanged
(three consecutive syncs produced three distinct nanosecond mtimes at a constant file
size, because the agent unconditionally calls `save_db`):

1. record `mtime_before` of the cache file;
2. `rbw sync` — **non-zero exit ⇒ refuse**, never serve from cache;
3. assert `mtime_after > mtime_before` — proves *this* sync reached the server;
4. optionally refuse if the sync is more than a few seconds old;
5. only now `rbw get` for each reference; non-zero ⇒ refuse.

Cache path: `~/.cache/rbw/<base_url percent-encoded>:<email>.json`.

Races are safe in the right direction: the only writer of the cache is a successful
sync, so a concurrent background sync can only make a value *newer*. A concurrent
`rbw purge` or a torn write (`Db::save` is marked `// XXX need to make this atomic`)
makes `get` fail — a safe refusal.

Caveat to carry into the implementation: this mtime contract is a side effect of the
implementation, not a documented guarantee.

### 4.5 The resolver runs as a container

In `/opt/portainer/docker-compose.yml` on borneo, next to Portainer itself, brought
up from the host with `docker compose up -d`. **Not a Portainer stack** — for the same
reason Portainer itself is not one: a component the control plane depends on cannot be
deployed through that control plane.

Containerising is the better option here, not a compromise: the top `rbw` operational
hazard is an `XDG_RUNTIME_DIR` mismatch between whoever started the agent and whoever
talks to it, which silently spawns a second, locked agent. In a container `HOME`,
`XDG_RUNTIME_DIR` and the profile are fixed by the image and there is no second user,
so that entire class of bug — along with the systemd dance (`Type=`,
`SuccessExitStatus=23`, `PrivateTmp`, unit ordering) — disappears. Unlocking happens
in the entrypoint at start.

- socket: bind-mounted host directory into both containers, not a named volume
- **master password: a read-only mounted file, never an env var** — container env is
  visible through `docker inspect`, i.e. through the very proxy the agents use
- `rbw` cache and config: a volume, or every restart is a fresh network `rbw login`
- `restart: always`, and **no watchtower label** — a component on the deploy critical
  path gets updated deliberately, as Portainer itself does
- image: multi-stage, our resolver plus an `rbw` binary. `rbw` is **not** packaged for
  Debian (`apt-cache policy rbw` on borneo is empty), so build it in a rust stage.

### 4.6 Network path (verified on borneo)

DNS resolves `vaultwarden.vvzvlad.xyz` to a public address, but the internal path is
open and the certificate validates on it:

```
tcp 10.31.40.120:443            open
curl --resolve …:10.31.40.120   http=200  tls_verify=0  remote_ip=10.31.40.120
curl (public)                   http=200            remote_ip=46.188.5.21
```

So keep `base_url` at the public name (the certificate is issued for it) and pin
resolution inward:

```yaml
extra_hosts:
  - "vaultwarden.vvzvlad.xyz:10.31.40.120"   # keep the deploy path inside the LAN
```

This removes cloudflared 2023.10.0 — which reliably fails 3–4 times at host boot
before systemd restarts it — from the deploy critical path.

---

## 5. Compose-level rules for migrated stacks

- **Always use `${VAR:?message}`**, never bare `${VAR}`. An undefined variable is a
  warning plus an empty string, not an error (compose-go `template/template.go`), so
  a bare reference means a service silently starting with a blank password.
- **Interpolation applies to every YAML *value*, not just `environment:`** — including
  `command:`, `healthcheck.test`, `entrypoint:` and `labels`. This is what makes the
  six awkward stacks solvable by the same mechanism, and it is the key difference from
  `env_file:`, which only populates the container's environment and takes no part in
  interpolating the compose body. Interpolation does **not** apply to YAML *keys*, so
  labels must use the list form (`- "key=${VALUE}"`).
- **`$` in values.** In a plain `.env` (the portable path for third parties) bcrypt and
  argon2 values (`$2y$…`, `$argon2id$…`) must be **single-quoted**, or the dotenv
  parser interpolates them. This hits our traefik `basicauth.users` hashes directly.
  Verify with `docker compose config` — but never log its output, it prints values.

---

## 6. Rejected alternatives, and why

Recorded so they are not re-proposed.

| Option | Why rejected |
| --- | --- |
| Values in the stack `Env` field, pushed at deploy | Values sit in Portainer's DB — visible via `StackInspect`. Fails requirement 1. |
| Plain `.env` in the stack's project directory | A materialised copy inside `portainer_data`; `filesToBackup` includes `compose`, so any admin token pulls every `.env` through `POST /backup`. Also dies on redeploy (§3.7). |
| Env file on the **target host**, referenced by path | Compose runs inside the Portainer container (§3.2) — it cannot see the target host's filesystem at all. Mechanically impossible. |
| Files on borneo rendered from a laptop over ssh | Ties server infrastructure to a personal machine, and is a second copy that drifts. |
| Portainer speaks to Vaultwarden directly (crypto in the fork) | 400–700 lines of security-critical Go, the org-key path is where reference implementations break, and it puts the vault credential inside Portainer. |
| A syncer into Infisical, machines read Infisical | Does not remove the Bitwarden machinery, it relocates it (still needs master password, org key, and polling — Vaultwarden has no change feed). Does not solve freshness: either the deploy triggers the sync synchronously, putting Vaultwarden back on the critical path and making Infisical pointless, or a staleness window remains. Plus a third store to back up, and 3 containers / 4–8 GB RAM for 65 variables. |
| sops+age, OpenBao, Infisical as the store | Technically clean (sops exposes `github.com/getsops/sops/v3/decrypt`, a 3-function stable API; OpenBao's `seal "static"` solves auto-unseal). Rejected by the owner: the store is Vaultwarden. |
| Docker/compose `secrets:` with `file:` | Feeds a file into the container, not the `${VAR}` interpolation, so it cannot fix the six awkward stacks; and it needs images that support `*_FILE`, breaking the "vanilla compose" requirement. |

---

## 7. Work plan

1. **Resolver service** (own project in `home-network`, image to
   `gitea.vvzvlad.xyz/projects/`): backend interface, Vaultwarden backend with the
   sync+mtime protocol, unix socket, container, entrypoint unlock. ~150 lines plus
   Dockerfile.
2. **Fork patch** (this branch): reference detection in `stack.Env`, batch call to the
   resolver, injection through `libstack.Options.Env`, hard failure on any error.
   ~50 lines in `api/exec/compose_stack.go`, no frontend. PR into `develop` in the
   fork's usual style (`feat(...)` with an issue number, Go tests next to the code).
3. **Pilot**: `arcextension` on nebula.lc — two variables (`METRICS_TOKEN`,
   `ADMIN_TOKEN`), small blast radius.
4. **Migration tooling** (`home-network`, modelled on `projects/vw-agent-secrets`
   with its `plan`/`push`/`rewrite` commands and its rule of never printing a value):
   build the value → usages registry, push into Vaultwarden, rewrite stack bodies to
   `${VAR:?…}` and set the references in `Env`.
5. **The dense stacks**: `rss`, `telegram_bots`, `mcp-websearch`, `ucuetis`, then the
   rest.
6. **Rotation.** These values sat in compose bodies that agents read as a matter of
   course. Migrating them is hygiene; it does not undo the exposure. ~65 variables.

### Naming in the vault

`stack/<endpoint>/<stack>/<VARIABLE>`. The endpoint segment is mandatory: stack names
repeat across environments (`docker-etc` four times, `monitoring` three, `timeseries`
and `rdesktops` twice each). A reused value gets **one** entry referenced from several
stacks, never copies, or rotation will split services apart.

### Explicitly out of scope

- **hatbox** runs stock EE 2.42.0, not our build — its 13 stacks stay outside this
  scheme until it moves to our image.
- The `vaultwarden` stack itself (id 30, island.lc) must be **excluded** from the
  scheme, or deploying it would require itself. It has no secrets in its compose, so
  the exclusion is free — but it has to be explicit.
- `Config.Env` visibility through the docker proxy (§3.6).

---

## 8. Sources

Portainer fork: `pkg/libstack/compose/composeplugin.go`, `pkg/libstack/libstack.go`,
`api/exec/compose_stack.go`, `api/exec/common.go`, `api/exec/swarm_stack.go`,
`api/stacks/stackutils/env.go`, `api/stacks/deployments/deployer.go`,
`api/http/handler/stacks/stack_file.go`, `api/http/handler/stacks/stack_versioning.go`,
`api/backup/backup.go`, `.gitea/workflows/build-image.yml`.

`rbw`: `src/protocol.rs`, `src/db.rs`, `src/actions.rs`, `src/dirs.rs`,
`src/pinentry.rs`, `src/bin/rbw-agent/{agent,actions,state,daemon}.rs`,
`bin/rbw-pinentry-keyring` (reference pinentry implementation).

Compose: interpolation and environment-precedence documentation, compose-go
`cli/options.go` and `template/template.go`.

Portainer upstream: discussions #11297 (external secret vaults — unimplemented since
2024), issue #5223 (secret provider integration — not done). There is no plugin or
hook mechanism; patching is the only way in.
