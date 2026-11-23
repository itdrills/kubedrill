#!/usr/bin/env bash
# Smoke test in an isolated sandbox: never touches ~/.kube/config or ~/.kubedrill.
set -euo pipefail
BIN="${1:-}"
if [ -z "$BIN" ]; then
  BIN="$(command -v kubedrill || true)"
  [ -n "$BIN" ] || BIN="$(cd "$(dirname "$0")/.." && pwd)/bin/kubedrill"
fi
command -v kubectl >/dev/null || { echo "kubectl is required for the smoke test" >&2; exit 1; }
SANDBOX="$(mktemp -d "${TMPDIR:-/tmp}/kubedrill-smoke.XXXXXX")"
export KUBEDRILL_HOME="$SANDBOX/home" KUBECONFIG="$SANDBOX/kubeconfig"
# Offline by default so the smoke test needs no network; SMOKE_IMAGE_CHECK=auto
# exercises the real registry lookup.
export KUBEDRILL_IMAGE_CHECK="${SMOKE_IMAGE_CHECK:-off}"
PORT="${SMOKE_PORT:-18443}"
cleanup() {
  "$BIN" stop >/dev/null 2>&1 || true
  rm -r -- "${SANDBOX:?}"
}
trap cleanup EXIT
k() { kubectl --context kubedrill --cache-dir "$SANDBOX/cache" "$@"; }
step() { printf '\n== %s\n' "$*"; }

step "binary: $BIN ($("$BIN" version))"
step "start"; "$BIN" start --port "$PORT" --time-scale 2
step "kubectl get nodes"; k get nodes
step "create deployment"; k create deployment nginx --image=nginx
k rollout status deployment/nginx --timeout=60s
k get deployments,pods
step "challenge"; "$BIN" challenge start image-pull-backoff
for _ in $(seq 1 30); do k get pods | grep -q ImagePullBackOff && break; sleep 1; done
k get pods
"$BIN" hint
if "$BIN" challenge check >/dev/null; then echo "check unexpectedly passed" >&2; exit 1; fi
step "fix"; k set image deployment/nginx nginx=nginx:1.27
k rollout status deployment/nginx --timeout=60s
"$BIN" challenge check
"$BIN" challenge stop
step "stop"; "$BIN" stop
echo; echo "SMOKE TEST PASSED"
