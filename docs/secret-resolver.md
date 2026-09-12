# Secret resolver: keeping secret values out of Portainer

Design document for a fork feature. It began as the handoff from the investigation that
produced the design, so that whoever picked it up would not re-derive it or re-walk the dead
ends; the feature is now **implemented** in `pkg/secretresolver`, `api/exec` and
`api/stacks/deployments`, and the sections below say "now enforced in code" and name the test
that pins each decision. §6 is the only part that is still a plan.

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

### 3.1 Compose always runs inside the Portainer Server container

`pkg/libstack/compose/composeplugin.go` imports the compose engine as a **library**
and calls `composeService.Up(ctx, project, opts)`:

```go
"github.com/compose-spec/compose-go/v2/cli"
cmdcompose "github.com/docker/compose/v2/cmd/compose"
"github.com/docker/compose/v2/pkg/api"
"github.com/docker/compose/v2/pkg/compose"
```

There is no `exec.Command("docker-compose", …)` anywhere. The comment
`// ComposeStackManager is a wrapper for docker-compose binary` in
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

`api/exec/compose_stack.go`, `createEnvFile` — **as this patch leaves it**. Vanilla took
only the stack and read `stack.Env` directly; the second parameter is the change, and it is
the whole mechanism by which a resolved value never reaches the file:

```go
func createEnvFile(stack *portainer.Stack, env []portainer.Pair) (string, error) {
    if len(env) == 0 { return "", nil }            // literals only; references are not here

    envFilePath := stackEnvFilePath(stack)         // <ProjectPath>/stack.env
    envfile, err := os.OpenFile(envFilePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
    defaultEnvPath := path.Join(stack.ProjectPath, path.Dir(stack.EntryPoint), ".env")
    copyDefaultEnvFile(envfile, defaultEnvPath)    // .env content first
    copyConfigEnvVars(envfile, env)                // the literals appended, they win
    return envFilePath, nil
}
```

The caller passes the literal half of `stack.Env`, so a stack whose every variable is a
reference writes no file at all.

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

### 3.6 The container-automation daemon does not go through this path

`grep -rn "DeployComposeStack(" api --include="*.go"` gives callers only in
`api/stacks/deployments/{deployer,deployment_compose_config,deploy}.go` and
`api/http/handler/stacks/stack_start.go`. There is **no** call from
`api/containerautomation` — that package works one level down, on containers:
`ContainerInspect` → pull → recreate → health gate → rollback (`autoupdate.go`,
`seams.go`). It reads the compose stack name only to label a notification.

So every compose deploy really does funnel through `DeployComposeStack` →
`ComposeStackManager.Up`, and the resolver placed there covers all of them,
including `StackStart` and the git-polling `RedeployWhenChanged`.

**But the check surfaced a genuine operational consequence.** Because the daemon
recreates a container from the *existing* container's config, it reuses the `Env`
that container was created with — values already substituted at its last real
deploy. Therefore:

- an auto-update will never deploy an unresolved reference (it never re-substitutes),
  which is the good half;
- an auto-update will also never pick up a **rotated** secret. A container updated by
  the daemon — or by watchtower, which the park currently runs on and which behaves the
  same way — keeps the old value until someone redeploys the stack for real.

Rotation therefore means: change the value in the vault **and redeploy the stack**. An
image update is not a rotation. This has to be in the migration runbook, because the
failure is silent: the service keeps working on the old credential until the day the
old credential is revoked.

**Container-level rollback does not resurrect old values either.** Worth stating
because the opposite is a natural guess: `rollback.go` re-tags the previous image id
onto the original reference and then calls `Recreate` on the *new* container, so the
inspected `Config.Env` is the current one. Only the image travels back.

**Stack-level `RollbackTo` is the real trap, and it is worse than a one-off deploy of
an old body.** `snapshotFileBasedStackVersion` (`api/http/handler/stacks/stack_update.go`)
with `rollbackTo != nil` reads the target version's entrypoint from disk
(`GetStackProjectPathByVersion` → `GetFileContent`; client-supplied content is ignored)
and feeds it into `collectStackFilesContent`, which snapshots it as a **new** version,
`StackFileVersion+1`. So rolling a migrated stack back past the migration does not merely
deploy the old body once — it makes that body **current**, and one mis-click restores
secret literals as the live definition.

Where they land, precisely — this matters for the runbook, because getting it wrong
sends someone to clean the wrong place: for file-based stacks the compose body lives
**on disk under `ProjectPath`**, not in the database; the database holds `Env []Pair`.
Cleaning the DB therefore does not remove restored literals. And the body is exposed
through `GET /api/stacks/{id}/file` (`api/http/handler/stacks/handler.go`), so agents
see it exactly as they see `Env`.

Runbook rule: once a stack is migrated, versions older than the migration are unsafe
to roll back to, and pre-migration version directories should be pruned rather than
left as tempting restore points.

The same applies to the **Portainer image**: once stacks are migrated, rolling it back to a
build without this patch is unsafe — stock Portainer passes `secret:vw:…` through as a
literal, and a service that creates its credential on first start against an empty volume
creates it *as that string*.

### 3.7 Residual leak channels (not closed by this work)

| Channel | Status |
| --- | --- |
| `Config.Env` of the running containers, via Portainer's docker proxy | **Open.** No stack-level design closes this; it needs endpoint permissions for the agent. |
| `POST /backup` | Closed by using `Options.Env` — `filesToBackup` includes `compose`, so any materialised `.env` **would** be in the backup. |
| `Stack.DeploymentStatus[].Message` | **Now verified: the leak is real.** Closed by *withholding* the deployer's error text, not by redacting it — see below for why redaction was not enough. |
| Deploy logs | **Narrow but real, and not closed.** See below. |
| `compose-unpacker` command line | **Was open; closed by a guard.** `getEnv` in `api/stacks/deployments/compose_unpacker_cmd_builder.go` builds `--env=NAME=VALUE` verbatim from `Stack.Env`. See below. |
| `PORTAINER_*` on the server process, via compose interpolation | **Open, pre-existing, and worth knowing about.** |
| `Stack.DeploymentStatus[].Message` as a *size and byte* channel, separately from the value question | **Partly closed.** The refusals this feature adds are bounded and escaped — §3.13. The pre-existing one next to them is not: `stackutils.ValidateComposeURLs` and `ValidateStackFiles` return the compose-go loader's error almost unchanged, and it quotes the compose file's own text verbatim, raw control bytes included, bounded by nothing. `ValidateComposeURLs` runs for **every** user, so no admin gate covers it. Not this feature's to close; an agent reading a deployment status message must still treat it as untrusted. |

**On the `DeploymentStatus` row.**
Reproduced against `pkg/libstack/compose` on compose-go v2.9.1: whenever a substituted
value lands in a field compose validates *by value*, the value is copied verbatim into
the error text.

```text
ports                    →  invalid containerPort: <value>
deploy.resources.limits  →  'services[app].deploy.resources.limits.memory'
                            strconv.ParseFloat: parsing "<value>": invalid syntax
depends_on               →  service "app" depends on undefined service "<value>"
```

That error is wrapped as `failed to deploy a stack: %w` and stored by
`UpdateStackStatusFromDeploymentResult` (`api/stacks/stackutils/stack_status.go`) as
`Message: err.Error()` in the database, from where `StackInspect` serves it. So the
value lands **permanently** in the very channel this feature exists to close — and the
likeliest way to trigger it is a mistyped reference or a wrong vault entry, which is
the characteristic accident of migrating 65 variables.

**So the control is a whitelist instead: when a deploy resolved at least one reference,
the deployer's error text does not leave the deploy path at all.** The caller — and
therefore `DeploymentStatus[].Message`, the database and the API — gets a fixed,
value-free message naming the stack and the operation. This is sound by construction
rather than by pattern-matching, and it does not decay as dependencies change.

The cost is real and accepted deliberately: a failed deploy of a secret-bearing stack
shows no compose diagnostics in the UI. Requirement 1 is absolute, diagnosability of a
*misconfigured* stack is not; and the redacted text is still written to the Portainer
server log as defence in depth, where the owner can read it. The redaction that feeds
that log line covers the raw, quoted, JSON and lowercased forms plus `:`/`/`/`,`
fragments, applies replacements longest-first (otherwise a short value that is a
substring of a longer one leaves the longer one's tail exposed), and withholds the line
entirely rather than shredding it when a value is too short to redact safely.

**Not covered by any of this:** compose's own internal logging, which reaches
`pkg/libstack/compose/logwriter.go` through logrus. That is the container log rather
than the API — a lesser channel, and not one an agent reads by default — but it is not
proven clean.

**A second disk-level leak of the same family.** `createEnvFile`
returns early without touching the filesystem when it has nothing to write. That is
exactly the state a stack reaches once **all** its variables have migrated, so the
`stack.env` written by the last pre-migration deploy stays on disk with live values —
and `compose` is in `filesToBackup` (`api/backup/backup.go`), so it keeps riding out in
every `POST /backup`.

**Deleting that file on the next deploy was tried and reverted — do not reintroduce it.**
`<ProjectPath>/stack.env` is not necessarily a file Portainer wrote: Portainer's own UI
states that when deploying via **Repository** the `stack.env` must already reside in the
git repo, and the clone lands in `stack.ProjectPath`
(`app/react/components/form-components/EnvironmentVariablesFieldset/StackEnvironmentVariablesPanel.tsx`).
The empty branch is reached by every stack with no variables at all, so deleting there
destroys a user's repository file and breaks a documented configuration — and gating it
on "had variables, all of them references" still deletes the repo's file for a fully
migrated git stack. **A deploy must not delete files it did not create.**

The cleanup therefore belongs to migration, not to the deploy path — and it has to be a
sweep rather than a single delete anyway, because deleting the current copy does not
clean history: old `v<N>` directories keep their own `stack.env` (up to 20 versions are
retained, `stack_versioning.go`), and `RollbackTo` can point `ProjectPath` back at one of
them. So after migrating a stack, sweep `/data/compose/<id>/v*/stack.env` by hand — the
same sweep that §3.6 already requires for pre-migration compose bodies, and for the same
reason.

**On the `compose-unpacker` row.** `getEnv` (`api/stacks/deployments/compose_unpacker_cmd_builder.go`)
turns `Stack.Env` into `--env=NAME=VALUE` arguments for the `compose-unpacker` container,
with no resolution step — so a `secret:` reference would be handed to a service as a
literal credential, the exact failure the swarm guard exists to prevent. It is
unreachable in CE today **only** because `stackutils.IsRelativePathStack` is hardcoded to
`false` with the comment "This function is only for code consistency with EE"
(`api/stacks/stackutils/util.go:47`). A security invariant resting on a CE stub is not an
invariant — any rebase onto EE code removes it — so the same refusal is now written
there explicitly.

**On the deploy-logs row.** Verified in
compose-go v2.9.1, `loader/interpolate.go:101-116`: `toBoolean` logs the
**post-interpolation** value.

```go
case "y", "yes", "on":
    logrus.Warnf("%q for boolean is not supported by YAML 1.2, please use `true`", value)
```

So a secret whose value happens to be exactly `y`, `yes`, `on`, `n`, `no` or `off`,
placed in a boolean-typed field (`read_only`, `privileged`, `init`, `tty`,
`networks.*.external`, …), is written to the log. A six-value alphabet makes this
close to unreachable for a real credential.

The redaction that closes the `DeploymentStatus` channel does **not** cover this: it
acts on the error returned from the deployer, not on what compose logs internally.
Closing it properly would mean filtering the log writer, which needs the current
deploy's values in a place that has no business holding them. Left open deliberately,
recorded so the next person does not rediscover it as a surprise.

**On the `PORTAINER_*` row** (verified, `pkg/libstack/libstack.go:17`): `PortainerEnvVars()`
sweeps **every** environment variable of the Portainer server process whose name starts
with `PORTAINER_` into the compose environment, and `createProject` feeds it to
`cli.WithEnv` before anything else. So a stack body containing `${PORTAINER_ANYTHING}`
interpolates the server's own variable into a service, and the result is readable in
that container's `Config.Env` through the docker proxy — the channel row 1 of this table
already describes.

This predates the feature and is not made worse by it, but it constrains the design in
one concrete way, and the design already respects it: **the only `PORTAINER_`-prefixed
variables this feature adds are addresses** — the resolver endpoint and its timeout.
The Vaultwarden master password deliberately lives in the resolver's container as a
mounted file, not in Portainer's environment, and not as an environment variable even
there.

`secretresolver.New` now **refuses** an `http(s)://` endpoint whose
userinfo carries a credential, so Portainer starts with a resolver configuration that fails
every deploy of a stack using references rather than one that quietly publishes its own
credential. Nothing is lost by the refusal: the deployment is a unix socket (§4.6), remote
authentication was never a requirement, and this design has no channel that could carry a
token safely — adding one is a design change, not a configuration option.

The general rule that follows: **never put a secret into a `PORTAINER_`-prefixed
variable on the server process.** Worth stating out loud, because it looks like the
obvious place to put one.

### 3.8 A latent fork bug found on the way

`api/http/handler/stacks/stack_versioning.go`, `collectStackFilesContent()` copies
only `stack.EntryPoint` and `stack.AdditionalFiles` into the new `v<N>` directory.
A `.env` placed in the project directory therefore **disappears on every redeploy**,
and compose does not fail on the resulting undefined variables — it logs a warning
and substitutes an empty string (compose-go `template/template.go`). A stack comes up
with blank passwords and looks healthy.

Independent of this feature, and worth fixing separately (~10 lines). Note the
semantics decision it forces: on `RollbackTo`, does `.env` come from the target
version (rolling secrets back too) or from the current one?

### 3.9 And another one: compose log levels are dead code

Found while checking the deploy-logs row of §3.7; independent of this feature.

`pkg/libstack/compose/logwriter.go:21` decides a compose log line's severity by testing
`strings.HasPrefix(logMessage, "time=")`, falling back to `info` plus the raw line when
the test fails. But `composeplugin.go:31-35` configures logrus with
`TextFormatter{DisableTimestamp: true}`, so every line it emits starts with `level=`,
never `time=`.

The prefix test is therefore **always false**. Two consequences, both live today:

- the entire level-mapping switch underneath it is unreachable for compose logs;
- every compose message — warnings and errors included — is emitted at zerolog
  **Info**, with the raw `level=warning msg="…"` text carried inside the message body.

So compose failures cannot be filtered by level, and everything compose says is visible
at default verbosity. Roughly a two-line fix (accept a `level=` prefix as well, or drop
`DisableTimestamp`), but it belongs in its own change, not this one.

### 3.10 A third one, and this one leaks credentials today

Found while closing the compose-unpacker pass-through; **independent of this feature and
live in the current fork.**

`api/stacks/deployments/deployer_remote.go:251-254` logs the unpacker's whole command
line:

```go
log.Debug().Str("cmd", strings.Join(cmd, " ")).Msg(…)
```

That command line is assembled from `generateRegistriesStrings` and
`appendGitAuthIfNeeded`, so it carries `--registry=<user>:<password>:<url>`,
`-u <user> -p <git password>` and every `--env=NAME=VALUE`. **Registry passwords and git
credentials therefore reach the Portainer server log at DEBUG level today**, with no
secret-resolver feature involved at all.

Two things follow. First, it wants fixing on its own merits — it is a plain credential
leak into a log an operator or an agent may read. Second, it constrains this feature: if
resolution is ever extended to the remote/unpacker path, **that line must be redacted or
removed first**, or resolved values join the registry passwords already there. What keeps
values out of it right now is precisely the refusal added to `getEnv`, not any property
of the logging.

**Two related boundary facts, recorded so the withholding is not over-trusted:**

- **The withholding boundary is the `libstack.Deployer` return, not the compose process.**
  `pkg/libstack/compose/composeplugin.go:352-355` does `log.Warn().Err(err)` and
  `pkg/libstack/compose/status.go:65,80` does `log.Error().Err(err)`, both *inside* the
  deployer, upstream of anything this fork wraps. No
  outer error type can protect those: if a compose error there ever quotes a value, it
  lands in the log unredacted. This is the same channel §3.7's deploy-logs row leaves
  open, reached by a second route.
- `api/http/handler/stacks/create_compose_stack.go:75,84` logs `fmt.Sprintf("%+v", stack)`
  — the entire `portainer.Stack`, `Env` included. Under this design that prints
  *references*, which are not secret. But it is the one place the whole `Env` is dumped,
  so it must stay on the "references only, never values" side of the invariant, and it is
  a reason not to relax the rule that `Env` never holds a value.

### 3.11 Validation sees references, the deploy sees values

Not a leak — a **divergence**, and the one place
where this design weakens an existing security control rather than strengthening it.

`stackutils.BuildEnvMap` (`api/stacks/stackutils/env.go`) builds the interpolation
environment for `IsValidStackFile` and `ValidateComposeURLs` out of `stack.Env` — which
under this design holds **references**. The deploy interpolates **values**. Before this
work both stages saw the same strings; now they do not.

`IsValidStackFile` is what enforces, for non-administrators, the environment's policy on
bind mounts, `privileged`, `pid: host`, devices, sysctls, `security_opt` and capabilities,
by inspecting the resolved compose config. So the check runs against one string and the
deploy runs against another.

The concrete shape of the problem: with `AllowBindMountsForRegularUsers` off, a regular
user writes `volumes: ["${MOUNT}"]` and sets `MOUNT=secret:vw:stack/app/TOKEN`. Two
mechanics in compose-go v2.9.1 make that pass validation, and both are verified here
rather than assumed:

- **The source is classified as a named volume.** `format/volume.go:180` `isFilePath`
  returns true only for a source beginning with `.`, `/`, `~`, `\\` or a Windows drive.
  `secret` begins with none of them, so `populateType` sets `VolumeTypeVolume` and the
  bind-mount policy has nothing to object to.
- **The leftover segment is silently discarded.** The short syntax splits on `:` into
  source, target and *mode*, so the vault path lands in the mode position — where an
  unrecognised option is not an error. `format/volume.go:113` says so in its own words:
  `// ignore unknown options FIXME why not report an error here?`

At deploy time the same variable resolves to whatever the vault holds, `/:/host` included,
and *that* string does begin with `/`, so it becomes a real bind mount. The same
divergence applies to `devices`, `sysctls` and `security_opt`; and `ValidateComposeURLs` —
the SSRF policy, which applies to **every** user — probes `secret:vw:…` instead of the
real registry host.

Exploiting it requires the ability to create a vault entry with the chosen value, so it is
not reachable by a Portainer user alone. That is a mitigation, not a fix.

**The rule that follows, and it is a real constraint on rollout: a stack carrying secret
references must not be deployed — or edited — by a user who is not an environment
administrator.** **The deploy half is
now enforced in code on every path that builds a `ComposeStackDeploymentConfig`** — stack
create (`stackbuilders`), `PUT /stacks/{id}`, the git redeploy handler and `POST
/stacks/{id}/migrate` — and nowhere else, because the gate sits inside
`ComposeStackDeploymentConfig.Deploy`. The edit half stays an access-control matter for
whoever grants stack access, though in practice `PUT /stacks/{id}` redeploys and therefore
meets the same gate.

**The swarm deployment config needs no such gate.**
`SwarmStackDeploymentConfig.Deploy` reaches either `SwarmStackManager.Deploy`
(`api/exec/swarm_stack.go`), which refuses a reference for *every* user before it opens the
endpoint proxy, or `DeployRemoteSwarmStack`, which refuses it before it touches the environment.

- **`ValidateComposeURLs` — the SSRF half — still probes the reference.** It applies to every
  user, administrators included, so an admin gate cannot fix it; this is an accepted, documented
  divergence whose mitigation is the one above: exploiting it requires the ability to write the
  vault entry in the first place.

### 3.12 The resolver's `error` field is the only external text that reaches the API

The withholding of §3.7 covers the **deployer's** errors. The errors of the resolution step
itself — everything that can leave `resolveStackSecrets` — deliberately do **not** go
through it. Enumerate what can leave `resolveStackSecrets`:

- `manager.secretResolver == nil` — "stack %q uses secret references but no secret resolver
  is configured, set PORTAINER_SECRET_RESOLVER": constants, the stack name, an env var name.
- from `Fetch` — the configuration error (an env var name and the operator's own bad value),
  a **classified** transport failure (see below), a non-200 status with no error field,
  "response exceeds N bytes", "no value for reference %q" (that is the **reference**, not the
  value), the decode failure, and `parsed.Error`.
- `stack %q: no value resolved for variable %q` — a stack name and a variable name, both
  bounded by `TruncateName` and escaped by the `%q` that prints them; see §3.13.

Every component of every one of those, `parsed.Error` aside, is constructed by Portainer out
of data that is already public in the stack config — **with one exception, which had to be
engineered away rather than argued away.** The list above once named the dial error and the
timeout outright, and called them safe because they are built from public data.

Both come back as a `*url.Error`, and a `*url.Error` prints the URL of the request. The
endpoint is *not* wholly public — it can be **given** with userinfo, and an operator who has
a credential will put it there, which is why every printed endpoint goes through
`redactEndpoint`, the refusal error included. The startup log is now reachable with userinfo
only through the `unix://` form, where it is a stretch of the socket path rather than a
credential. `net/http` builds that error from `stripPassword(req.URL)`, and `stripPassword`
masks a **password** and nothing else, so the `http://<token>@resolver:9100` form this
package once offered came back whole — on the commonest failure this feature has, a resolver that is down,
unreachable or slow, and on every retry of it, accumulating in `DeploymentStatus[]`.
Reproduced rather than suspected:

```text
Post "http://TOKENINUSERNAME@127.0.0.1:1/v1/resolve": dial tcp: connection refused
Post "http://user:***@127.0.0.1:1/v1/resolve":        dial tcp: connection refused
```

`New` **refuses** an endpoint whose userinfo carries
a credential, and builds a request URL only from one that does not, so neither `c.url` nor
`req.URL` can hold a credential and no transport error can quote one.

The credential lived in `PORTAINER_SECRET_RESOLVER`, and
`PortainerEnvVars()` sweeps every `PORTAINER_`-prefixed variable of the server process into
the compose environment of **every** project, so a stack body containing
`${PORTAINER_SECRET_RESOLVER}` published it into that container's `Config.Env` — readable
through the docker proxy by anyone who can edit a stack.

**The endpoint itself was the last unbounded term anywhere in this package, for the same
reason: it is the operator's text, so it was treated as free.** It is not free, because of
where it goes — `Client.endpoint` is in every transport error `Fetch` returns and in every log
line the failure writes, and the transport error is persisted as the stack's deployment status
message. Measured with a mebibyte in `PORTAINER_SECRET_RESOLVER`, before and after:

```text
transport error, unix endpoint       1048698 → 364 bytes
configErr, unparsable port           1048735 → 390 bytes
configErr, endpoint with no host     1048621 → 296 bytes
configErr, unsupported scheme        1048682 → 359 bytes
```

A `*url.Error` prints the URL *and* whatever it wraps, and what it wraps for a
response `net/http` cannot parse is the resolver's own header text, quoted whole: `bad
Content-Length "…"`, `unsupported transfer encoding "…"`, `malformed HTTP response "…"`,
`http: message cannot contain multiple Content-Length headers; got […]`. Nothing bounds those
below `MaxResponseHeaderBytes`, 10 MiB. Measured against the client before this was fixed,
with the text planted in the header and the marker present in every case:

```text
bad Content-Length, 1 MiB      1048724 bytes
bad Content-Length, 8 MiB      8388756 bytes
duplicate Content-Length          4297 bytes
Transfer-Encoding, 1 MiB       1048736 bytes
malformed status line, 1 MiB   1048729 bytes
```

These are copied from `transportLeakShapes` in `pkg/secretresolver/secretresolver_test.go`,
which is the authoritative record.

That is the same publication channel the redirect refusal closed, reached with no redirect at
all — and `%w` carried all of it into `Stack.DeploymentStatus[].Message`. The fix is
**classification rather than wrapping**: `Client.transportFailure` reduces the failure to one
of a closed set of reasons and returns a `transportError` whose text is that reason, the step,
the endpoint as `redactEndpoint` renders it, and a pointer at the server log. Nothing else.
The set is closed and it is **eleven**:

| # | Reason | Note |
| --- | --- | --- |
| 1 | the deploy was cancelled | the caller's context, checked first |
| 2 | the deploy's own deadline expired | likewise |
| 3 | no answer within *&lt;the client's budget&gt;* | the characteristic wedged-resolver failure |
| 4 | the call was cancelled | **defence in depth**, see below |
| 5 | the endpoint's host name did not resolve | |
| 6 | the resolver's TLS certificate did not verify | |
| 7 | the endpoint is https but the resolver answered in plain HTTP | |
| 8 | the endpoint is https but the resolver did not answer with TLS | |
| 9 | the connection could not be opened | a dial failure |
| 10 | the connection failed during the exchange | |
| 11 | *its answer could not be read as an HTTP response* | the catch-all |

Each of those reasons is a fact **this**
process produced; the catch-all is where every error built out of resolver text lands, and it
is a fixed phrase. `net/http`'s real message goes to the server log, sanitised and bounded to
`maxTransportDetailBytes` — and the returned error now says *see the Portainer server log*, as
the decode branch already did, because that log line is the only place the real reason exists.
`errors.Is` still reaches `context.Canceled` and
`context.DeadlineExceeded` — the error unwraps to those two sentinels and to nothing else, so
no sink anywhere can unwrap its way back to the withheld text. Pinned from a **raw
`net.Listener`** by `TestFetchBoundsATransportError`, on both endpoint forms and all five
shapes: an `httptest`-based probe is falsely green here, because Go's own server caps the
request header it will read and quietly ends the exchange instead.

Nothing the resolver *sent* is quoted back either: an undecodable body, an oversized body and
the raw body behind a non-2xx status all produce an error built from non-content facts, with
the status, the content type and the body length going to the server log instead. That is not a theoretical precaution — the JSON
decoder in use (`segmentio/encoding`, mandated by depguard) appends the first 32 bytes of
the buffer it choked on to its syntax errors, and that buffer is the response body
`{"values":{"<ref>":"<value>"…`, so for a short reference the window reaches into the value.

The same rule reaches into the **status line**, which is the trap here, because
`resp.Status` reads like the status code and is not. Go fills it from the status line as
the server wrote it, so it is `"<code> <reason phrase>"` and the phrase is text the
*resolver* chose — bounded only by `net/http`'s 10 MiB header limit, and on an `http://`
endpoint settable by anyone on the path. Echoing it would have been a second, unbounded
channel of resolver text into the very error `maxResolverErrorBytes` exists to bound, on
every retry of a failing deploy. The client therefore formats `resp.StatusCode`: the
number is ours — three digits parsed by `net/http` — while the phrase beside it is theirs
and is dropped. The decode failure logs the number for the same reason, and bounds the
content type it logs beside it. Reviewed and reproduced: a raw TCP server answering
`HTTP/1.1 500 <2 KiB of planted text>` put all of it into
`Stack.DeploymentStatus[].Message` and out of `StackInspect`.
`TestFetch/keeps the status line reason phrase out of the returned error` pins it from a
raw `net.Listener`, because an `httptest` server can only ever write the canonical phrase.

**The widest resolver-chosen header is `Location`, and the refusal of redirects is what keeps
it out of the returned error — but refusing alone left the operator with nothing.** A 3xx
lands on the non-2xx branch, which names the status code and the bounded `error` field, so a
deploy behind a path-canonicalising proxy failed with exactly `secret resolver returned status
308` and no hint of where the request was being sent (§4.6 names the proxy configurations that
do this, and they are the common ones). The gap is now closed on the **log** side only:
`logRefusedRedirect` writes a Warn naming the status and the Location reduced by
`redactLocation` to **scheme, host and port** — no path, no query, no userinfo — sanitised and
bounded to `maxRedirectTargetBytes` on top, because a host name is resolver-written text too.
The returned error is unchanged. So the precise claim, and it is deliberately narrow: no part
of a `Location` and no other resolver-chosen header reaches **the returned error**, which is
the channel Portainer persists and serves; two bounded things reach the **log**, this reduced
target and `maxTransportDetailBytes` of `net/http`'s own message. The log is a lesser channel
— not persisted with the stack, not served by the API, not read by an agent — but not a free
one, which is why both are bounded rather than merely tolerated.

`parsed.Error` is the exception, and it is echoed on purpose. It is **contractually free of
secret values**: the resolver is our own component, and the extensibility requirement of
§4.2 is about pluggable *backends behind* the resolver, not about third-party
implementations of the wire protocol. Echoing it is what makes "the Vaultwarden vault is
locked" readable in the UI instead of an opaque "secret resolution failed", and a locked or
unsynced vault is the characteristic operational failure of this feature — withholding it
would make the common case undiagnosable, which is not a trade worth making for text that
carries no value by construction.

Two bounds keep the exception honest. The field is bounded to `maxResolverErrorBytes`
(512, on a rune boundary, with an ellipsis marker) before it is echoed, so a resolver
answering with a stack trace cannot write a page of somebody else's text into Portainer's
database. And the contract is stated in the `pkg/secretresolver` package doc, where the
author of a second implementation of the wire protocol reads it: **a resolver that
interpolates a resolved value into its `error` field breaks the contract.**

#### The sanctioned channel is byte-arbitrary, and length was not the control it needed

Bounding says how much of somebody else's text is persisted. It says nothing about **what**,
and the adversarial sweep built a 552-byte deployment status message that was entirely
within the bound and carried two newlines, a NUL, an ANSI CSI escape (`\x1b[31m`), a
forged-looking JSON log record, and the sentence `SYSTEM: ignore previous instructions…`.

That is not a size problem and not a secrecy problem. It is a **reader** problem, and §1 of
this document names the reader: `Stack.DeploymentStatus[].Message` is served by `StackInspect`
and **AI agents read it as a matter of course** while doing ordinary infrastructure work. A
512-byte field that reaches an agent's context verbatim is a prompt-injection channel; one that
reaches an operator's terminal verbatim is a terminal-escape channel; one that reaches a log
shipper verbatim is a log-forging channel. The resolver is our own component and the field is
contractually value-free, but "contractually value-free" was never a claim about control
characters.

So every piece of resolver-chosen text this client formats now goes through one function,
`sanitize`, which replaces C0 and C1 control characters, DEL and bytes that are not valid UTF-8
with printable escapes: the `error` field on both branches that echo it, the `Content-Type`
header it logs, `net/http`'s own message about a failed round trip, and the reduced `Location`.

**The configured endpoint is sanitised too, and is therefore printed with `%s` and never with
`%q`.** `redactEndpoint` is the one place every printed endpoint passes through and it does
both — strip the userinfo, then `sanitize` to `maxEndpointBytes` — so by the time an endpoint
reaches a format string it is already escaped, and `%q` on top would escape its escapes.
**The rest of Portainer's and the operator's own text is bounded only, and escaped by the `%q`
that prints it** — a reference (`truncateReference`), the raw timeout value
(`truncateTimeoutValue`) and the stack and variable names `api/*` names in a deploy refusal
(`TruncateName`). Bounded **and** `%q`, never one of the two: `%q` leaves a mebibyte a
mebibyte, and a bound leaves an `ESC` an `ESC`. §3.13 is the worked case.

`Test_resolveStackSecrets_errorPathsCarryNoSecretValue` (`api/exec`) is the pin. It drives
every path listed above — against the real client and a fake resolver, so the two halves
cannot drift apart — and asserts that a planted value appears in none of the messages. A new
error path that carries a value fails it.

### 3.13 The text that was *not* the resolver's was the unbounded one

The same message is built from Portainer's own data — the stack's name and the name of the
variable that carries the reference — and **three refusals this feature adds formatted the
variable name with `%s`**: `deployment_compose_config.go` (the non-administrator gate),
`compose_unpacker_cmd_builder.go` and `swarm_stack.go`. Neither name is validated on the way
in. `updateComposeStackPayload.Validate` checks that the compose file is non-empty and says
nothing about `Env` at all, nothing in the server caps a request body, and `normalizeStackName`
— which does restrict a stack name to `[-_a-z0-9]` on the create and update paths — is not
applied by `POST /stacks/{id}/migrate`. So both the length and the bytes were the caller's.
The hostile name throughout is `"NAME\x00\x1b[31m\n"` + 1 MiB of `A`. The left figure is the
message **before** the fix, with that name in the *variable* position and an ordinary stack name
— except `withheldError`, which names no variable, where it is the stack name. The right figure
is the same message **after** it, with a hostile name in *both* positions, which is what the two
pins below drive; with an ordinary stack name the right column would instead read 433, 410, 378,
318 and 213.

```text
refuseSecretReferencesForNonAdmin  1048755 →  690 bytes
checkNoSecretReferences            1048732 →  667 bytes
SwarmStackManager.Deploy           1048700 →  635 bytes
no value resolved for variable     1048649 →  575 bytes
withheldError                      1048781 →  470 bytes
```

with the NUL, the ESC and the newline present in the first three and gone from all five. A
mebibyte in *both* names put the four that name two names over 2 MiB; `withheldError` names only
the stack and stayed at one.

**The inversion is the point.** `refuseSecretReferencesForNonAdmin` fires precisely *for* a
non-administrator — the lowest-privileged actor in this threat model, and the one the gate
exists to contain.

**Escaped *and* bounded, because neither half is a fix alone.** `%q` closes the control
characters and leaves the mebibyte; a bound closes the mebibyte and leaves the `ESC`. The
fourth site, `"no value resolved for variable %q"`, already had the verb and had no bound —
1048649 bytes of perfectly escaped text — which is the cleanest demonstration of that available.
The bound is `secretresolver.TruncateName`, and it lives in `pkg/secretresolver` because
`api/*` already depends on that package and the reverse edge does not exist; its doc comment is
where the class is argued once for all four files.
`Test_secretRefusals_boundAndEscapeHostileNames` (`api/stacks/deployments`) and
`Test_secretErrors_boundAndEscapeHostileNames` (`api/exec`) are the pins.

**The bound is on the *input* of `%q`, and `%q` inflates.** `TruncateName` runs before the verb,
which is the right order — an escape sequence cannot be cut in half that way — but it means
`maxNameBytes` bounds what `%q` is *handed*, not what it writes: an invalid UTF-8 byte and a C0
byte each come back out as four characters of `\xNN`. A 256-byte name of either prints as 1029
bytes, so the gate reaches **2212** bytes in the worst case rather than the 690 the table above
shows for a name filled with `A`. `sanitize` is the deliberate
contrast — it escapes and bounds in one pass, so *its* limit bounds its output — and the two
doc comments now say which channel gets which and why.

**The log sites are outside this rule, deliberately.** Four statements in three functions name a
stack with no bound and no call: `LogEscapedValues` (reached from all three deploy paths),
`logWithheldError`'s two branches, and `warnAboutStaleEnvFile`. The log is a smaller channel
than `DeploymentStatus[].Message` — the server's stderr, stored with nothing, served by no API —
and vanilla Portainer already opens it wider on paths this feature does not touch, logging
`fmt.Sprintf("%+v", stack)` in `create_compose_stack.go` and an unbounded `stack.Name` in
`deploy.go`. The usual justification for it is wrong, though, and was checked rather than
assumed against zerolog v1.34.0: `ConsoleWriter` — which is what the default `--log-mode PRETTY`
gives — escapes as well as JSON mode does, because `writeFields` puts every string field through
`needsQuote` and then `strconv.Quote`, so no raw NUL, ESC or newline reaches the terminal in any
of the three modes. What is genuinely unbounded is the *length*: 1048698–1048734 bytes for a
mebibyte name, in every mode. That is the half being accepted, and it is accepted on the
channel, not on the escaping — the escaping is zerolog's and is pinned by no test of this fork.

**What this does not make trusted.** `DeploymentStatus[].Message` still carries pre-existing
vanilla channels that are wider than the one just closed — above all the compose-go loader
error echoed by `stackutils.ValidateComposeURLs` and `ValidateStackFiles`, which quotes the
compose file's own text verbatim, raw control bytes included, bounded by nothing. Those are
listed in §3.7; closing them is not this feature's to do, and an agent reading a deployment
status message should still treat it as untrusted.

---

## 4. The design

### 4.1 Shape

```text
compose body        stack.Env            resolver socket        Vaultwarden
────────────        ─────────            ───────────────        ───────────
${DB_PASSWORD:?}    DB_PASSWORD=         batch fetch, one       collection
                      secret:vw:stack/…  per compose call       `infra`
      │                    │                     │                   │
      └──── Portainer patch ────────────────────►│──── rbw ─────────►│
                  Options.Env (memory only)
```

- **Compose body**: plain `${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}`.
  The `:?` form matters — see §5.
- **`stack.Env`**: holds *references*, not values. UI round-trips them untouched
  (the frontend treats `Env[].Value` as an opaque string), so no TypeScript changes.
- **Portainer patch**: resolves references at deploy and injects via
  `libstack.Options.Env`, covering all three entry points (`Up`, `Run`, `Pull` — every
  compose deploy funnels through them via `api/stacks/deployments/deployer.go`).
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
//
// The marker is part of the reference on the wire: refs are sent exactly as the stack
// stores them, and the resolver requires the "secret:" prefix. It is also the key the
// values map comes back under.
vals, err := resolver.Fetch(ctx, refs)   // ["secret:vw:stack/nebula/arcextension/ADMIN_TOKEN", ...]
if err != nil {
    // Hard fail, never a fallback. The implemented wording is
    // "failed to resolve secret references of stack %q: %w", with the name bounded.
    return err
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

Protocol: **batched, one request per compose invocation** — batching is not an
optimisation but a requirement, because one Vaultwarden sync pulls the whole vault. All
values or an error; no partial results. A version field in the request lets the resolver
evolve without touching the fork. The resolver address is configuration (unix socket by
default, URL possible), so the resolver can move later without a patch.

The unit is the compose invocation and not the deploy: a
forced pull runs `Pull` and then `Up`, each resolving on its own, so that deploy makes
**two** requests and two vault syncs.

Settled concretely, so the two halves cannot drift:

```text
POST /v1/resolve      {"version":1,"refs":["secret:vw:stack/<env>/<stack>/<VAR>", …]}
  200                 {"values":{"<ref>":"<value>", …}}      every requested ref present
  non-2xx             {"error":"<reason>"}
GET  /healthz         200 when the vault is unlocked, 503 otherwise; never touches
                      the network — it is the compose healthcheck

limits, all four load bearing and none of them negotiable at run time:
  MAX_BODY_BYTES      256 KiB   request body; over it the answer is 413, not 400
  MAX_REFS            256       refs per request; over it, 400
  maxResponseBytes    1 MiB     CLIENT-side cap on the response; the resolver has none
  the version field   accepted values are absent or 1 — see below
```

**The two request caps are the resolver's and the operator sees them as ordinary errors.**
Over `MAX_BODY_BYTES` the status is **413** rather than 400, which matters because the client
formats the status code and the `error` field and nothing else: an operator reading "413" has
to be able to find it here. Over `MAX_REFS` it is a 400, and the real reason reaches the
operator intact — measured with 300 refs, the answer was a 400 whose `error` field named the
limit, echoed into the deployment status message through the usual bounded path.

**The response cap is the client's, and the resolver has no counterpart.** `maxResponseBytes`
is 1 MiB and it is enforced in `pkg/secretresolver` after the answer is on the wire: over it
the deploy fails with "secret resolver response exceeds 1048576 bytes" and the body is
discarded unread. Nothing on the resolver side bounds what it sends, so the two limits can
disagree, and they do: measured with 256 refs of 5000 bytes each, the resolver produced a
**legitimate 200 of 1288204 bytes** — comfortably inside its own request budget, since the
request carrying those 256 refs is small — and the client refused it outright. A second
implementation of this protocol has no way to discover that number except from here, so it is
stated here: **a resolver must keep one response under 1 MiB**, which with `MAX_REFS` at 256
means an average value under about 4 KiB.

It is enforced **twice**: an `io.LimitReader` decides how much is *read*, and a length check
beside it turns the result into a message saying which of "oversized" and "corrupt" it was. The
limit reader is the memory control — without it `io.ReadAll` pulls whatever a hostile resolver
streams into the Portainer server's heap, bounded only by `DefaultTimeout`. It is measured
from the far end: `TestFetch/"stops reading at the cap rather than merely reporting it"` streams
32 MiB from a raw listener and asserts the resolver never got past 8 MiB before the client hung
up.

**The `version` field does not do what both sides assume, and the rule that makes it work has
to be written down.** The intent recorded above — "a version field in the request lets the
resolver evolve without touching the fork" — is only true if a bump keeps the old version
working. Today the resolver accepts **absent or 1** and nothing else, and the client always
emits **1**, with no way to configure it. So a resolver bumped to 2 answers every deployed
Portainer with `400 unsupported request version 1`: the field produces the exact lockstep
break it exists to prevent, and it produces it on every stack at once. The rule, then:

> **A version bump must keep accepting the previous version for at least one release.** The
> resolver adds the new shape and goes on serving the old one; Portainer is rebuilt to emit
> the new version; only then may the old one be dropped. A resolver that accepts exactly one
> version has a field that names the coupling instead of removing it.

No code changes for this — the client's emitted version is correct as it stands, and the
constraint is on whoever changes the resolver next.

**The marker is `secret:`, and Portainer's parsing stops there.** A stack env value
beginning with `secret:` is a reference; everything after it — including the inner
scheme `vw:` — is opaque and forwarded whole. Detection cannot be "any value with a
`scheme:` prefix": `postgres://…` and `https://…` are ordinary values and would be
swallowed. One fixed marker keeps detection unambiguous while leaving the scheme
space entirely to the resolver, which is what §4.2 is for.

**The marker's cost, and the escape that pays it.** A bare prefix takes the value space
above it: an existing stack whose variable legitimately holds a `secret:`-prefixed literal
— some third-party application's own URI-ish configuration format — would behave
differently on this fork than on vanilla Portainer, with **no workaround available in
Portainer's UI**: the compose path fails for want of a resolver or inside one that cannot
make sense of the reference, and the swarm and unpacker paths refuse it outright. So the
colon is doubled to escape it: **`secret::<rest>` is a literal, and what reaches compose is
`secret:<rest>`** — one colon removed. `secret:::x` is therefore the literal `secret::x`.
`secretresolver.IsReference` returns false for the escaped form and `secretresolver.Unescape`
performs the removal, applied on every path that passes a literal through — compose, swarm
and the unpacker alike, so that a stack's values do not depend on how it is deployed, and in
`stackutils.BuildEnvMap`, so that the deploy-time validation of a stack file interpolates the
same string the deploy does rather than the stored one.

**The escape's one silent effect, and the log line that pays for it.** Everything else the
marker does is loud: an unresolvable reference fails the deploy, and the swarm and unpacker
paths refuse one by name. But an existing stack whose value happens to begin with `secret::`
deploys *successfully* here, with a value one colon shorter than vanilla Portainer gives it,
and nothing in the result would tell an operator that the container got something other than
what the stack stores. Each of the three deploy paths therefore calls
`secretresolver.LogEscapedValues` once per deploy — the stack name and the number of
variables affected, never the values — so the change is visible in the server log at the
moment it happens.

Configuration is **environment variables, not CLI flags** —
`PORTAINER_SECRET_RESOLVER` (endpoint, `unix://` or `http://`) and
`PORTAINER_SECRET_RESOLVER_TIMEOUT` (default 60 s, see §4.3). A fork carries every added flag
through every future rebase, across `api/cli`, the flags struct and the composition
root; an env var read at construction costs one call site. `NewComposeStackManager`
keeps its signature for the same reason.

Unset resolver + a stack that holds references ⇒ **hard error**. Never fall through:
an unresolved reference reaching a container as the literal string `secret:vw:…`
is a service quietly running on a garbage password, which is worse than a failed
deploy and much harder to notice.

### 4.3 Deadlines: give the resolver call its own, and make it short

There is a recurring timeout mistake in this area of the codebase, and the resolver
must not repeat it in mirror image.

For the resolver the requirement is the **opposite direction**: the call is a local
round trip over a unix socket to fetch a handful of short strings, so it gets its own
explicit, *bounded* deadline, independent of any docker client. A wedged or unresponsive
resolver must fail the deploy loudly and on a budget of its own — not hang it for the
length of an image pull, and still less inherit the hour a docker client would give it.
Do not create a docker client for this and do not pass `nil` anywhere near it.

**The chain, and the order it must keep.** `secretresolver.DefaultTimeout` is **60 s**, and
it is the outermost of three nested budgets that span both halves of this system:

```text
RBW_TIMEOUT_SECONDS(20)  <  REQUEST_TIMEOUT_SECONDS(45)  <  DefaultTimeout(60)
   one vault subprocess       one whole resolve request      the client round trip
```

Each must outlive the one inside it, or the inner timeout can never fire and its
diagnostic — the one that says *which* call hung — is never printed. Lowering the outermost
alone silently disables the resolver's own error reporting, so the three move together. This
belongs here and not only in a Go doc comment, because this document is the specification the
resolver is configured from: an operator who reads a wrong number here sets the inner budgets
around it and inverts the order, which is the exact breakage the comment on `DefaultTimeout`
warns about.

**Why 45 and 60.** The middle budget
was sized as though a resolve request were one subprocess call. It is **1+N**: one `rbw sync`
followed by one `rbw get` per reference. A worst-case sync of 20 s plus the process-spawn
overhead at `MAX_REFS=256` — 257 spawns at about 46 ms each, measured on an idle eight-core
box, so 11.8 s — is **31.8 s**, which fits 45.
Nothing is added on top for network latency, because `rbw get` reads a local cache and never
contacts the Vaultwarden server (§4.5).

The client
side is `DefaultTimeout`, pinned by `TestDefaultTimeoutMatchesTheDocumentedChain`; the
resolver's compose fragment sets `PORTAINER_SECRET_RESOLVER_TIMEOUT: 60s` explicitly so the
two halves are stated in the same file the operator edits.

### 4.4 Why `rbw` and not our own Bitwarden client

Rejected: writing the Bitwarden client crypto in Go (prelogin → PBKDF2/Argon2id →
HKDF-stretch → user key → RSA → org key → AES-CBC+HMAC). That is 400–700 lines of
security-critical code whose silent failure mode looks like a working system, to
maintain forever, for 65 secrets. Note that the closest open reference
implementation (`Turbootzz/Vaultwarden-API`) **fails on exactly our case** — org
collection items, `MAC verification failed` — because the organisation key path is
the hard part, and ours live in the org `agents`, collection `infra`.

`rbw` gets its crypto maintained by someone else. Its one real defect is handled in
§4.5. The pinentry shim it requires is ugly but inert under this threat model.

Accepted risk: `rbw` is in maintenance mode (author: "essentially feature-complete…
unlikely to spend time implementing new features"). If it breaks, writing the Go
client becomes a forced move with a clear reason, rather than an upfront bet.

### 4.5 Freshness: `rbw get` never contacts the server

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

### 4.6 The resolver runs as a container

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

**Do not put a reverse proxy in front of the resolver, and never one that rewrites paths.**
The client refuses redirects outright — it does not follow them, on either endpoint form, for
the reasons in `refuseRedirects` — so a proxy that answers `POST /v1/resolve` with a 301 or a
308 breaks every deploy of every stack that uses references. The two configurations that do
this by default are the common ones: trailing-slash canonicalisation (nginx `rewrite`,
Traefik's `redirectregex`, Apache `DirectorySlash`) and an http→https upgrade on the same
host. **The symptom is exact and unmistakable:** every deploy fails with

```text
secret resolver returned status 308
```

and the Portainer server log carries, next to it, a `Secret resolver answered with a
redirect` warning naming the scheme, host and port it was being sent to — the path is
deliberately not logged, because a Location is resolver-written text (§3.12). If a proxy is
unavoidable, it must pass `/v1/resolve` through untouched.

**A proxy writing a `Location` that Go cannot parse looks different and is worth recognising**,
because the deploy then fails with the generic

```text
failed to reach the secret resolver at <endpoint>: its answer could not be read as an HTTP
response, see the Portainer server log
```

The warning is still there, with `location="(unparsable location)"` beside the status. A 3xx carrying no `Location` header at all (a 300 or a 304) logs `"(no location)"`.

**The `https://` endpoint in front of a plain-HTTP resolver is the other misconfiguration this
section should let an operator recognise on sight.** Every deploy fails with

```text
failed to reach the secret resolver at https://<host>: the endpoint is https but the resolver
answered in plain HTTP, see the Portainer server log
```

which is a different message from *the resolver did not answer with TLS* — that one means the
far end wrote something that is neither TLS nor HTTP. Both are rows of the closed set in §3.12.

### 4.7 Network path (verified on borneo)

DNS resolves `vaultwarden.vvzvlad.xyz` to a public address, but the internal path is
open and the certificate validates on it:

```text
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
- **After a stack has migrated *fully*, delete its `stack.env` by hand.** With no literal
  variables left, a deploy writes no env file — so the one written by the last
  pre-migration deploy stays in the project directory with the old values in it, and rides
  out in every `POST /backup`. Portainer will not remove it: the same path can hold a
  `stack.env` that came from a git clone, and a deploy must not destroy a file it did not
  create (§3.7). A deploy of a fully migrated stack now logs a warning naming the stack and
  the path when the file is still there — and the warning only says the file *may* hold
  pre-migration values, because its firing condition cannot tell Portainer's file from the
  one a git-deployed stack brings in its clone; check which it is, and whether an
  `env_file:` directive still points at it, before deleting. Nothing removes the file, so
  the line repeats on every deploy until you do. Sweep `/data/compose/<id>/v*/stack.env`,
  not just the current version — old version directories keep their own copy.

### Three traps, each of which can bite silently

- **`env_file:` is not fed by this mechanism.** `Options.Env` supplies compose's
  *interpolation* environment; it does **not** add entries to a service's `env_file:`
  list. A stack that today passes a variable into its container via
  `env_file: stack.env` — rather than via `environment:` with `${VAR}` — will, after
  migration, start with that variable simply **missing**, and with no error, because
  nothing was interpolated and nothing failed. Before migrating a stack, check how each
  variable actually reaches the container, and convert `env_file:` consumers to explicit
  `environment:` entries with `${VAR:?}`. The `:?` is what turns this silent failure into
  a loud one.
- **A reference is visible to pre-deploy validation.** `stackutils.BuildEnvMap` puts the
  raw `secret:vw:…` string into the map used by `ValidateStackFiles` and
  `ValidateComposeURLs` (when SSRF protection is on). No secret is exposed — a reference
  is not secret — but a reference substituted into a typed field fails validation with a
  *different* message than the same mistake produces at deploy time, and
  `ValidateComposeURLs` may go and probe a meaningless registry host. Expect the
  discrepancy rather than debugging it twice. On the compose path `ValidateStackFiles`
  no longer sees a reference at all: it runs only for a user who is not an environment
  administrator, and that user's deploy is now refused before it (§3.11). It still runs
  on the swarm path, where the deploy is refused further down instead.
- **A forced pull resolves twice.** `DeployComposeStack` calls `Pull` and then `Up` when
  `forcePullImage` is set (`api/stacks/deployments/deployer.go`), and each does its own
  full `Fetch`. Harmless in itself — but it doubles the vault syncs per deploy, and if a
  secret is rotated in the window between the two calls, the pull and the deploy run with
  different values.

---

## 6. Work plan

1. **Resolver service** — **its own repository**, `~/Data/Projects/secret_resolver`,
   image to `gitea.vvzvlad.xyz/projects/secret_resolver`: backend interface,
   Vaultwarden backend with the sync+mtime protocol, unix socket, container,
   entrypoint unlock.

   It is **not** a subdirectory of `home-network`: that repository has no git remote
   and no CI at all, and this service needs an image built by Gitea Actions and
   pulled on borneo. And it is **Python**, not Go — the `Backend` sketch above stays
   accurate as a shape, but the estate's convention for server projects is the
   Python-in-Docker scaffold with its own guide, and the service is a subprocess
   wrapper around `rbw` with one HTTP endpoint. Its requirements, verified findings
   and the two deliberate deviations from that guide are in its own `docs/SPEC.md`.
2. **Fork patch** (this branch): reference detection in `stack.Env`, batch call to the
   resolver, injection through `libstack.Options.Env`, hard failure on any error.
   PR into `develop` in the fork's usual
   style (`feat(...)` with an issue number, Go tests next to the code).
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

One invariant this plan does **not** buy: nothing makes Portainer the only client of the
resolver — the socket is a bind-mount, so any container allowed to mount that path asks the
resolver for values directly, past every gate Portainer applies.

### Naming in the vault

`stack/<endpoint>/<stack>/<VARIABLE>`. The endpoint segment is mandatory: stack names
repeat across environments (`docker-etc` four times, `monitoring` three, `timeseries`
and `rdesktops` twice each). A reused value gets **one** entry referenced from several
stacks, never copies, or rotation will split services apart.

### Follow-ups this change deliberately does not make

Each was found while building this and each belongs in its own change, so that a security
feature does not arrive carrying unrelated repairs. Listed in the order they deserve
attention.

1. **The unpacker command line leaks registry and git credentials into the log at DEBUG
   today** (§3.10). The only one of these that is a live credential leak rather than a
   latent bug, and the only one that also constrains this feature's future: it must be
   fixed before resolution is ever extended to the remote/unpacker path.
2. **`.env` files are lost on redeploy** (§3.8).
3. **Compose log levels are dead code** (§3.9) — every compose message, warnings and
   errors alike, is emitted at zerolog Info.
4. **`make lint` cannot run as written**: the Makefile reads `.golangci-version`, and that
   file is absent from the tree. Worth fixing before relying on lint in CI — and note the
   repo is not clean under `golangci-lint:latest`, with issues in packages this work never
   touches (`api/containerautomation`, `api/oauth`, `api/agent`), so the pinned version is
   load-bearing.
5. **Lint findings inside this patch's own new test file**: `errcheck` on unchecked
   `w.Write`/`Encode`/`Serve`/`Close`/`os.RemoveAll`, `testifylint`'s go-require rule for
   `require` used inside HTTP handlers, and one `usetesting` hit on a deliberate
   `os.MkdirTemp` that exists to keep the socket path under the `sockaddr_un` length limit
   — that last one wants a `//nolint:usetesting` carrying the reason rather than a change.

6. **A divergence accepted rather than a repair deferred: a forced pull resolves the
   references twice.** `DeployComposeStack` calls `Pull` and then `Up` when
   `forcePullImage` is set, and each runs its own full `Fetch` — two round trips per
   deploy, and in principle two different values if the secret is rotated in the window
   between them. Accepted: the call is local and batched, and an env var takes no part in
   pulling an image, so the pull's copy of the value is never used for anything that
   outlives the call. Also noted from the compose side in §5.
7. **Likewise accepted: the client treats only `200` as success.** The wire contract of
   §4.2 defines success as a 2xx; `Fetch` tests `resp.StatusCode != http.StatusOK`. A
   resolver answering `202` would be a success to the spec and a failure to the client.
   Accepted as the stricter reading — this is a synchronous request for values that are
   either in hand or not, so a `202` has no meaning here — but written down so the author
   of a second resolver implementation is not surprised by it.

### Explicitly out of scope

- **hatbox** ran stock EE 2.42.0, not our build, which put its 13 stacks outside this
  scheme. **This may already be obsolete:** a session doing field work reports that on
  2026-08-17 the host `slug` (13 stacks, 42 containers, public services — gitea, the
  wiki, the sites) was moved off its own Portainer onto this fork as environment id 11,
  reached through an agent over WireGuard. Reported, **not verified here** — the
  Portainer MCP servers were unavailable in the window where this was written. Confirm
  the environment count on borneo before relying on it; if it holds, those 13 stacks
  are in scope and the estate is one control plane, not two.
- The `vaultwarden` stack itself (id 30, island.lc) must be **excluded** from the
  scheme, or deploying it would require itself. It has no secrets in its compose, so
  the exclusion is free — but it has to be explicit.
- `Config.Env` visibility through the docker proxy (§3.7).

---

## 7. Sources

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
