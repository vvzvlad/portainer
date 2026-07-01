# Running a local demo stand

How to build a Portainer image from one or more feature branches, run it, and log
in — plus the non-obvious gotchas that will otherwise eat an hour. Written from
real setup pain — read the **Gotchas** section before you start, especially
gotcha #1 (a development client bundle silently kills the whole UI).

## Prerequisites

- **Go 1.26+** (backend, `go.mod` at repo root), **Node 20+ / pnpm 10+**
  (frontend), and **Docker** (to build the image and to run it against the local
  Docker socket).
- Frontend deps installed: `pnpm install --no-frozen-lockfile` then
  `git checkout pnpm-lock.yaml` (never commit the lockfile).

## 1. Pick what goes in the image

For a single branch just check it out. To demo **several open PRs together**,
make an integration branch off `develop` and merge each PR branch into it:

```bash
git checkout -B stand/all-prs origin/develop
git merge --no-edit origin/feat/2-stream-logs     # PR #6
git merge --no-edit origin/feat/3-auto-update      # PR #19
# resolve any conflicts, then build from stand/all-prs
```

## 2. Build the image — pass `ENV=production` (see gotcha #1)

```bash
export PATH=/path/to/go/bin:$PATH
make build-image ENV=production TAG=stand
#   → builds client (webpack, production), server binary, and the Docker image
#   → produces portainerci/portainer-ce:stand  (~190 MB, FROM portainer/base)
```

`make build-image` depends on `build-all`, so it **re-runs** `build-client` and
`build-server` itself — you do **not** need to run them first. But `build-all`
uses the Makefile's default `ENV`, which is `development`. **You must pass
`ENV=production` to `build-image`** or it ships a development client bundle that
the browser refuses to run (gotcha #1). The client webpack build takes a couple
of minutes; the node-version warning (`wanted 22, current 20`) and the babel
`isModuleDeclaration` deprecation warning are both harmless.

## 3. Run it

```bash
docker run -d \
  --name portainer-stand \
  -p 9000:9000 -p 9443:9443 -p 8000:8000 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v portainer-stand-data:/data \
  --restart unless-stopped \
  portainerci/portainer-ce:stand --no-setup-token       # --no-setup-token: gotcha #3
```

- UI: `http://localhost:9000` (HTTP) or `https://localhost:9443` (HTTPS).
- To reach it from another machine, use the box's LAN/VPN IP — the server binds
  `0.0.0.0`, so no extra flag is needed (unlike a Vite dev server).

## 4. Seed the admin and a Docker environment

You can click through the first-run wizard, or seed it via the API so the login
is ready and there is something to manage:

```bash
# Create the admin (needs --no-setup-token from step 3, or the setup token header)
curl -s -X POST http://localhost:9000/api/users/admin/init \
  -H 'Content-Type: application/json' \
  -d '{"Username":"admin","Password":"portainer1234"}'

# Log in to get a JWT
JWT=$(curl -s -X POST http://localhost:9000/api/auth \
  -H 'Content-Type: application/json' \
  -d '{"Username":"admin","Password":"portainer1234"}' | jq -r .jwt)

# Add the local Docker socket as an environment so the UI has live containers
curl -s -X POST http://localhost:9000/api/endpoints \
  -H "Authorization: Bearer $JWT" \
  -F "Name=local" -F "EndpointCreationType=1"
```

> **Use a simple password with no special characters** — but note Portainer
> enforces a **minimum length of 12** for the admin account, so it can't be a
> single short word. Use a plain alphanumeric string of 12+ chars (e.g.
> `portainer1234`), not `Str0ng!Pass@2026`. Demo/test credentials get passed
> through shells, JSON payloads, and URLs by scripts and automation, where `!`
> `@` `$` `&` etc. get mangled or need escaping — a plain alphanumeric string
> avoids a whole class of "wrong password" confusion.

The `-v portainer-stand-data:/data` volume persists the admin and environments,
so rebuilding/replacing the container keeps your login.

## Gotchas (the грабли)

1. **`make build-image` without `ENV=production` ships a broken UI.**
   `build-image` → `build-all` → `build-client` runs with the Makefile default
   `ENV=development`, whose webpack config uses `devtool: 'eval-source-map'`. That
   wraps **every module in an `eval()` call**. Portainer serves a
   Content-Security-Policy with `script-src` that does **not** include
   `'unsafe-eval'` (gotcha #2), so the browser blocks every module, the app never
   bootstraps, and you're stuck forever on **"Loading Portainer…"** with **zero
   API calls** in the network tab (all static assets load `200`, which makes it
   look like a backend/network problem — it isn't). Always build the image with
   `ENV=production`. To confirm a good bundle: `dist/public/main.*.js` should have
   a hashed filename and **not** contain thousands of `eval(` calls or an
   `"eval-source-map devtool has been used"` header.

2. **CSP is on by default and omits `'unsafe-eval'`.** The `--csp` flag
   (env `CSP`) defaults to `true`, so the server sends
   `Content-Security-Policy: script-src 'self' …` on every response
   (`api/http/security/bouncer.go`). The **production** bundle is CSP-safe; only
   the development (eval) bundle trips over it. You normally should not need to
   touch this — fix the bundle (gotcha #1), don't disable CSP. If you truly must,
   run with `--csp=false`, but then you're not testing what ships.

3. **First-run admin init requires a setup token.** Recent Portainer refuses
   `POST /api/users/admin/init` unless you send the `X-Setup-Token` header (the
   token is printed in the server logs at startup). For a throwaway demo, start
   the container with `--no-setup-token` to disable that requirement.

4. **Admin password minimum length is 12.** Unlike some stands where a short
   one-word password is fine, Portainer rejects an admin password shorter than 12
   characters (`RequiredPasswordLength` in `/api/settings/public`). Keep it
   simple and special-char-free, just make it 12+ (e.g. `portainer1234`).

5. **`build-image` double-builds the client.** Because it re-runs `build-all`, if
   you already ran `make build-client` yourself the client compiles twice. Just
   run `make build-image ENV=production` and skip the separate `build-client`.
