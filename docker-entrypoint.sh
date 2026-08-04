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
TYPESENSE_API_KEY="${TYPESENSE_API_KEY:?TYPESENSE_API_KEY must be set}"

mkdir -p "$TYPESENSE_DATA"

# Loopback only, so the one way to the index is through docovia even if the
# container's port is published carelessly.
typesense-server \
  --data-dir="$TYPESENSE_DATA" \
  --api-key="$TYPESENSE_API_KEY" \
  --api-address=127.0.0.1 \
  --enable-cors=false &
typesense=$!

# No wait-for-ready loop: docovia already refuses to start until Typesense
# answers, and retries for thirty seconds while it comes up. A second
# implementation of the same wait here could only disagree with it.
docovia \
  -typesense-url http://127.0.0.1:8108 \
  -typesense-key "$TYPESENSE_API_KEY" \
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
