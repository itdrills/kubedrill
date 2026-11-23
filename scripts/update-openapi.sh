#!/usr/bin/env bash
# Refreshes the embedded upstream Kubernetes OpenAPI v3 documents.
set -euo pipefail
cd "$(dirname "$0")/.."
K8S_VERSION="${1:?kubernetes version like v1.37.2 required}"
for f in api__v1 apis__apps__v1 apis__discovery.k8s.io__v1 apis__networking.k8s.io__v1; do
  curl -fsSL "https://raw.githubusercontent.com/kubernetes/kubernetes/${K8S_VERSION}/api/openapi-spec/v3/${f}_openapi.json" \
    | gzip -9n > "internal/api/openapi/${f}.json.gz"
  echo "updated internal/api/openapi/${f}.json.gz"
done
