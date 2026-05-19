#!/bin/bash
# Patch the operator-rendered authbridge ConfigMap to add the IBAC
# pipeline (a2a-parser inbound + mcp-parser + ibac outbound).
#
# Usage: patch-ibac-config.sh <namespace> <agent-name>
#
# The kagenti operator creates `authbridge-config-<agent>` when the
# agent's pod is admitted (server-side apply, line 682 of pod_mutator.go).
# Its default pipeline has only `jwt-validation` inbound and
# `token-exchange` outbound. This script:
#
#   1. Extracts the operator's config.yaml from the ConfigMap
#   2. Reads our additions from k8s/ibac-patch.yaml
#   3. Merges them via python3 + PyYAML (PyYAML is widely available;
#      we error out with an actionable hint if it's missing)
#   4. Replaces the ConfigMap's data.config.yaml with the merged version
#
# Authbridge's filesystem-watch hot-reload picks up the change without
# a Pod restart (the operator-injected sidecar doesn't set
# readOnlyRootFilesystem so fsnotify can see the symlink swap).

set -euo pipefail

NAMESPACE=${1:-team1}
AGENT_NAME=${2:-email-agent}
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
DEMO_DIR=$(dirname "$SCRIPT_DIR")
PATCH_FILE="$DEMO_DIR/k8s/ibac-patch.yaml"
CM_NAME="authbridge-config-$AGENT_NAME"

if ! python3 -c 'import yaml' 2>/dev/null; then
  cat <<'EOF' >&2
ERROR: python3-yaml (PyYAML) is required.
  Install with one of:
    pip3 install --user pyyaml
    brew install libyaml && pip3 install pyyaml      # macOS
    sudo apt install python3-yaml                    # Debian/Ubuntu
EOF
  exit 1
fi

if ! kubectl -n "$NAMESPACE" get configmap "$CM_NAME" >/dev/null 2>&1; then
  echo "ERROR: ConfigMap $NAMESPACE/$CM_NAME not found." >&2
  echo "       The operator should create this when the agent pod is admitted." >&2
  echo "       Check: kubectl -n $NAMESPACE get pods -l app.kubernetes.io/name=$AGENT_NAME" >&2
  exit 1
fi

# The merge runs in scripts/ibac-merge.py: reads the patch from a
# file (argv[1]), reads the operator's current config.yaml from stdin,
# emits merged YAML to stdout. Idempotent: re-running is a no-op once
# the plugins are present.
#
# Implemented as a separate .py file rather than an inline heredoc
# because heredoc + piped stdin clash — python3 with `<<EOF` reads
# its script from the heredoc, which silently drops the kubectl pipe.
echo "[*] Merging IBAC additions into $CM_NAME ..."
MERGED_YAML=$(
  kubectl -n "$NAMESPACE" get configmap "$CM_NAME" \
      -o jsonpath='{.data.config\.yaml}' \
    | python3 "$SCRIPT_DIR/ibac-merge.py" "$PATCH_FILE"
)

if [[ -z "$MERGED_YAML" ]]; then
  echo "ERROR: merge produced empty output" >&2
  exit 1
fi

# Apply the patched ConfigMap. Using `kubectl create configmap
# --from-file --dry-run=client -o yaml | kubectl apply` is the
# conflict-free pattern: it sidesteps the resource-version mismatch
# you get when piping the existing CM through edits and re-applying.
echo "[*] Applying patched ConfigMap ..."
TMP_CONFIG=$(mktemp)
trap 'rm -f "$TMP_CONFIG"' EXIT
printf '%s' "$MERGED_YAML" >"$TMP_CONFIG"

kubectl -n "$NAMESPACE" create configmap "$CM_NAME" \
    --from-file=config.yaml="$TMP_CONFIG" \
    --dry-run=client -o yaml \
  | kubectl apply -f -

echo "[*] Patched. Active plugins now:"
kubectl -n "$NAMESPACE" get configmap "$CM_NAME" \
    -o jsonpath='{.data.config\.yaml}' \
  | python3 -c '
import yaml, sys
c = yaml.safe_load(sys.stdin)
for d in ("inbound", "outbound"):
    names = [p["name"] for p in c.get("pipeline", {}).get(d, {}).get("plugins", [])]
    print(f"      {d}: {names}")
'
