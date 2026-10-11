# Agent notes

A fork of Portainer CE hosted at gitea.vvzvlad.xyz/projects/portainer (default branch
`develop`). Upstream layout, build commands and coding conventions are in `CLAUDE.md`;
this file covers only what the fork adds.

## Fork additions

- `api/containerautomation/` - native container auto-heal and auto-update (Watchtower-style
  per-container recreate, health-gated rollback, webhook notifications, the inbound
  registry-push webhook). Opt-in per container via labels such as
  `io.portainer.autoheal.enable` and `io.portainer.update.enable` (see `labels.go`).
- `api/docker/container.go`, `api/docker/images/` - container recreate with restore on
  failure, and the image puller with stall detection.
- `pkg/secretresolver/` - client for an external secret resolver; stacks store references,
  values are resolved at deploy time. Configured with `PORTAINER_SECRET_RESOLVER` and
  `PORTAINER_SECRET_RESOLVER_TIMEOUT`. Design and invariants: `docs/secret-resolver.md`.
- File version history for file-based stacks (`api/http/handler/stacks/stack_version*.go`,
  migration `api/datastore/migrator/migrate_2_44_0.go`).

## Build, test, deploy

- Do not build or run tests on the workstation. CI (`.gitea/workflows/build-image.yml`)
  builds and publishes the image on pushes to `develop` and on `v*` tags.
- Secret resolver integration test against a real resolver:
  `ci/secretresolver-integration/run.sh` (runs on the remote Docker context `cirunner`).
- Local demo stand: `docs/dev-stand.md`.

## Invariants

- `pkg/secretresolver` depends on nothing from Portainer.
- No resolved secret value may reach an error, a log line, `stack.env` or a backup; deploy
  errors are persisted as the stack's deployment status message. Names printed in such
  errors go through `secretresolver.TruncateName` and `%q`.

## Limits for agents
- Tests: at most three times the code (`wc -l` of `tests/` against the code). Over the limit,
  still write the test you need, and in the same change go through the old tests: drop or merge
  duplicates and tests of implementation rather than behaviour.
- Comments explain the code as it is and why it is built this way. History ("used to", "after
  the review", "since the incident") goes into the commit message.
- This file stays under ~150 lines: only what an agent would get wrong without it. No decision
  history, incident logs or retelling of the code.
