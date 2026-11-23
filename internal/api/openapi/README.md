# Embedded OpenAPI v3 documents

These files are unmodified, gzip-compressed copies of the upstream Kubernetes
OpenAPI v3 specification (`api/openapi-spec/v3/*.json`) from
https://github.com/kubernetes/kubernetes at tag `v1.37.2`, licensed under the
Apache License 2.0 (Copyright The Kubernetes Authors).

They are served by the simulator at `/openapi/v3/...` so that `kubectl explain`
works offline and kubectl uses server-side field validation. They describe the
full upstream API; the simulator implements only the subset documented in the
README ("Supported subset").

Refresh with `make openapi K8S_VERSION=v1.37.2`.
