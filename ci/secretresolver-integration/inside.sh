#!/bin/sh
# Runs INSIDE the throwaway container that run.sh starts - not on a workstation, and not
# anywhere a real vault can be reached. It is handed /work/fork (go.mod, go.sum,
# pkg/secretresolver) and /work/resolver (main.py, src, requirements.txt) by the tar run.sh
# pipes in, and it:
#
#   1. installs python3 and the resolver's runtime dependencies;
#   2. writes a fake `rbw` and puts it first on PATH - see the freshness protocol below;
#   3. starts the resolver on a unix socket;
#   4. runs the fork's integration test against that socket;
#   5. stops the resolver and exits with the test's status.
set -eu

FORK=/work/fork
RESOLVER=/work/resolver

RESOLVER_HOME=/opt/resolver
RESOLVER_DATA=$RESOLVER_HOME/data
RESOLVER_CACHE=$RESOLVER_HOME/cache
MASTER_PASSWORD_FILE=$RESOLVER_HOME/master-password
SOCKET=/run/secret-resolver/resolver.sock
FAKE_BIN=/opt/fake-bin
VENV=/opt/venv
LOG=/tmp/resolver.log
TEST_LOG=/tmp/gotest.log

echo "==> installing python and the resolver's requirements"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends python3 python3-venv >/dev/null
python3 -m venv "$VENV"
"$VENV/bin/pip" install --quiet --no-input --disable-pip-version-check -r "$RESOLVER/requirements.txt"

echo "==> writing the fake rbw"
mkdir -p "$FAKE_BIN" "$RESOLVER_DATA" "$RESOLVER_CACHE/rbw" "$(dirname "$SOCKET")"

# THE VALUES THIS FAKE SERVES ARE ALSO WRITTEN OUT IN
# pkg/secretresolver/integration_test.go. Neither file can import the other, so the two lists
# are kept in step by hand; a mismatch fails the test as a wrong value rather than as a
# protocol problem.
#
# The system python3 runs it, not the venv: the fake needs no dependency, and `rbw` has to work
# for whatever process the resolver spawns it from.
cat >"$FAKE_BIN/rbw" <<'PY'
#!/usr/bin/python3
"""A stand-in for `rbw` that satisfies src/backends/vw.py's freshness protocol.

Never talks to anything. The three subcommands the backend uses:

  unlocked  exit 0, the vault is "unlocked";
  sync      rewrite the cache file with changing content AND PUSH ITS mtime STRICTLY FORWARD.
            The backend stats the file before and after the sync and refuses the whole request
            unless mtime advanced - that proof is the entire point of the protocol, and a
            `touch` would not do it: the file system's timestamp granularity, not the write,
            decides whether two syncs a few milliseconds apart look identical;
  get -- N  print the entry's value on stdout and exit 0; for an unknown entry exit 1 with a
            message on stderr and NOTHING on stdout, because the backend would otherwise serve
            whatever was printed as the secret.
"""

import os
import sys
import time

ENTRIES = {
    "ci/alpha": "s3cr3t-alpha-9f2a",
    "ci/beta": "s3cr3t-beta-4d71",
    "ci/awkward": "s3cr3t-\"gamma\"\nпароль-ß",
}

# The same file the backend finds by globbing $XDG_CACHE_HOME/rbw/*.json, which is why there is
# exactly one .json in that directory: the backend refuses an ambiguous match.
CACHE_FILE = os.path.join(os.environ["XDG_CACHE_HOME"], "rbw", "ci.json")


def sync():
    try:
        before = os.stat(CACHE_FILE).st_mtime_ns
    except FileNotFoundError:
        before = 0

    with open(CACHE_FILE, "w") as handle:
        handle.write('{"synced_at_ns": %d}\n' % time.time_ns())

    # Set the mtime explicitly rather than trusting the write's own: `max(now, before + 1ms)`
    # is what makes the advance STRICT even if two syncs land inside one timestamp tick.
    after = max(time.time_ns(), before + 1_000_000)
    os.utime(CACHE_FILE, ns=(after, after))


def get(argv):
    # The backend always passes `--` before a caller-supplied name; accept it and step over it.
    if argv and argv[0] == "--":
        argv = argv[1:]
    if not argv:
        sys.stderr.write("fake rbw: get needs an entry name\n")
        return 1

    name = argv[0]
    if name not in ENTRIES:
        sys.stderr.write("fake rbw: no entry named %s in the vault\n" % name)
        return 1

    # Bytes, not print(): the values are not ASCII and the container has no locale to speak of,
    # so the encoding is chosen here rather than inherited. rbw ends its output with a newline
    # and the backend strips exactly one, so exactly one is written.
    sys.stdout.buffer.write((ENTRIES[name] + "\n").encode("utf-8"))
    return 0


def main():
    command = sys.argv[1] if len(sys.argv) > 1 else ""
    if command == "unlocked":
        return 0
    if command == "sync":
        sync()
        return 0
    if command == "get":
        return get(sys.argv[2:])

    sys.stderr.write("fake rbw: unsupported command %r\n" % command)
    return 2


sys.exit(main())
PY
chmod +x "$FAKE_BIN/rbw"
PATH="$FAKE_BIN:$PATH"
export PATH

# The cache file has to exist before the first request: the backend stats it BEFORE the sync,
# and a missing file is a refusal rather than a first sync.
printf '{"synced_at_ns": 0}\n' >"$RESOLVER_CACHE/rbw/ci.json"
# A path is all the resolver wants here, and the fake `rbw` never reads it - no pinentry runs.
printf 'not-a-real-password\n' >"$MASTER_PASSWORD_FILE"

echo "==> starting the resolver on $SOCKET"
cd "$RESOLVER"
VW_EMAIL=ci@example.invalid \
	VW_BASE_URL=https://vault.example.invalid \
	VW_MASTER_PASSWORD_FILE="$MASTER_PASSWORD_FILE" \
	DATA_DIR="$RESOLVER_DATA" \
	RESOLVER_SOCKET_PATH="$SOCKET" \
	XDG_CACHE_HOME="$RESOLVER_CACHE" \
	"$VENV/bin/python" main.py >"$LOG" 2>&1 &
resolver_pid=$!

waited=0
while [ ! -S "$SOCKET" ]; do
	if ! kill -0 "$resolver_pid" 2>/dev/null; then
		echo "the resolver exited before its socket appeared:" >&2
		cat "$LOG" >&2
		exit 1
	fi
	waited=$((waited + 1))
	if [ "$waited" -gt 150 ]; then # 150 x 0.2s = 30s
		echo "no socket at $SOCKET after 30s, the resolver's log so far:" >&2
		cat "$LOG" >&2
		kill "$resolver_pid" 2>/dev/null || true
		exit 1
	fi
	sleep 0.2
done

echo "==> running the integration test"
cd "$FORK"
status=0
# -v so the run says which tests ran, and -count=1 so a cached result from a previous run can
# never stand in for one against this resolver.
SECRET_RESOLVER_INTEGRATION_ENDPOINT="unix://$SOCKET" \
	go test -v -count=1 -tags integration ./pkg/secretresolver/ >"$TEST_LOG" 2>&1 || status=$?
cat "$TEST_LOG"

# A PASSING RUN IS NOT PROOF THAT THE SEAM WAS TESTED. `go test` exits 0 when the integration
# test is skipped - which is what it does when the endpoint variable is empty - and the package's
# own unit tests would carry the "ok" on their own. That is the one failure this whole harness
# exists to catch, so the parent test has to be seen passing by name.
if [ "$status" -eq 0 ] && ! grep -q '^--- PASS: TestIntegrationAgainstResolverService ' "$TEST_LOG"; then
	echo "the integration test did not run: no PASS for TestIntegrationAgainstResolverService" >&2
	echo "(an endpoint the test cannot see makes it skip, and a skip still exits 0)" >&2
	status=1
fi

if [ "$status" -ne 0 ]; then
	echo "==> the resolver's log:" >&2
	cat "$LOG" >&2
fi

kill "$resolver_pid" 2>/dev/null || true
wait "$resolver_pid" 2>/dev/null || true

exit "$status"
