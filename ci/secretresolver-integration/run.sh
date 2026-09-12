#!/bin/sh
# Run pkg/secretresolver's integration test against a REAL secret_resolver process.
#
#   ci/secretresolver-integration/run.sh
#   SECRET_RESOLVER_REPO=/path/to/secret_resolver ci/secretresolver-integration/run.sh
#
# Both sides are shipped into one throwaway container on the remote Docker context "cirunner"
# and wired together there: the resolver listens on a unix socket backed by a fake `rbw`, and
# `go test -tags integration` dials it. Nothing runs on the workstation, no image is built, and
# no real vault is reachable - the endpoint is an RFC 2606 .invalid name and the `rbw` on PATH
# is written by inside.sh.
#
# Bind mounts do not work on cirunner, so the sources travel as a tar piped into `docker run -i`
# and are unpacked inside. The container's exit code is this script's exit code.
set -eu

IMAGE=golang:1.26
DOCKER_CONTEXT=cirunner

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
FORK_ROOT=$(cd "$SCRIPT_DIR/../.." && pwd)
RESOLVER_ROOT=${SECRET_RESOLVER_REPO:-$HOME/Data/Projects/secret_resolver}

if [ ! -f "$RESOLVER_ROOT/main.py" ]; then
	echo "no secret_resolver checkout at $RESOLVER_ROOT (set SECRET_RESOLVER_REPO)" >&2
	exit 1
fi

# Only what the two sides need to build and run. The fork contributes go.mod, go.sum and the
# package itself - `go test ./pkg/secretresolver/` compiles nothing else, because the package
# imports the standard library, zerolog and segmentio/encoding, its tests add testify, and none
# of it is from Portainer.
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT

mkdir -p "$STAGE/fork/pkg" "$STAGE/resolver"
cp "$FORK_ROOT/go.mod" "$FORK_ROOT/go.sum" "$STAGE/fork/"
cp -R "$FORK_ROOT/pkg/secretresolver" "$STAGE/fork/pkg/"
cp "$RESOLVER_ROOT/main.py" "$RESOLVER_ROOT/requirements.txt" "$STAGE/resolver/"
cp -R "$RESOLVER_ROOT/src" "$STAGE/resolver/"
cp "$SCRIPT_DIR/inside.sh" "$STAGE/"

echo "shipping $FORK_ROOT and $RESOLVER_ROOT to $DOCKER_CONTEXT ($IMAGE)"

status=0
tar -cf - -C "$STAGE" . |
	docker --context "$DOCKER_CONTEXT" run --rm -i "$IMAGE" \
		sh -c 'set -eu; mkdir -p /work; cd /work; tar -xf -; exec sh /work/inside.sh' || status=$?

exit "$status"
