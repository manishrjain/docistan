#!/bin/bash
# Two processes, one lifetime.
#
# Typesense lives in here with docovia because it holds nothing: its index is
# rebuilt from the JSON sidecars on every boot, so there is no volume, no
# migration and no version skew to manage — bumping it is a line in the
# Dockerfile and a rebuild, which is the whole reason it belongs in the same
# image rather than beside it.
#
# That also decides what happens when either one dies: everything does. There is
# no supervisor here and deliberately so. A restarted Typesense would come back
# empty and docovia would go on serving an archive that had silently lost every
# document; a restarted docovia would replay the sidecars into an index that was
# never lost. Neither is worth the machinery. The container exits, the restart
# policy starts it again, and the boot replay puts both back in step — which is
# the recovery path that already exists and is exercised on every start.
set -uo pipefail

TYPESENSE_DATA="${TYPESENSE_DATA:-/var/lib/typesense}"

# Generated rather than required. This key guards a port on loopback inside one
# container, shared by two processes that are born together and die together —
# there is no second party to agree it with and nothing to coordinate, so making
# an operator invent one only ever produced a value that had to be stored
# somewhere. An externally supplied one still wins, which is what the dev
# compose file relies on.
: "${TYPESENSE_API_KEY:=$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')}"
export TYPESENSE_API_KEY

mkdir -p "$TYPESENSE_DATA"

# Loopback only, so the one way to the index is through docovia even if the
# container's port is published carelessly.
#
# The key goes through the environment, not --api-key, because a container does
# not have its own process table: `ps` on the host prints the full command line
# of everything inside it, so an argument here is a secret published to every
# user on the machine. Typesense reads TYPESENSE_API_KEY on its own, and
# docovia's -typesense-key defaults to the same variable.
typesense-server \
  --data-dir="$TYPESENSE_DATA" \
  --api-address=127.0.0.1 \
  --enable-cors=false &
typesense=$!

# No wait-for-ready loop: docovia already refuses to start until Typesense
# answers, and retries for thirty seconds while it comes up. A second
# implementation of the same wait here could only disagree with it.
docovia \
  -typesense-url http://127.0.0.1:8108 \
  "$@" &
app=$!

# Forwarded rather than left to the kernel: docker stop signals PID 1, and
# docovia's own shutdown drains the workers so a document is not abandoned
# mid-OCR. Without this it would be killed outright at the end of the grace
# period and that work thrown away.
forward() {
  kill -TERM "$typesense" "$app" 2>/dev/null || true
}
trap forward TERM INT

# Returns as soon as either exits, which is the entire supervision policy.
wait -n
code=$?

forward
wait 2>/dev/null || true
exit "$code"
