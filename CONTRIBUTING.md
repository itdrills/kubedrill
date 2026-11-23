# Contributing

Thanks for helping! KubeDrill aims to be small, honest and reliable.

## Ground rules

- **Prove compatibility with the real kubectl.** A feature counts as supported
  only when an end-to-end test (or a documented manual run) shows it working with
  the real binary. Update the "Supported subset" table in the README.
- **Never touch real clusters.** Code must not read credentials from or send
  requests to anything but the simulator. Tests use temporary `KUBEDRILL_HOME`
  and `KUBECONFIG` directories.
- **Deterministic simulation.** Use the cluster clock and the seeded name
  generator; no `time.Now()` or `math/rand` inside controllers. Network access
  is limited to the optional image check (`internal/imagecheck`); tests run
  with `KUBEDRILL_IMAGE_CHECK=off` or a fake verifier.
- Keep dependencies minimal (official Kubernetes libraries, Cobra).

## Workflow

```sh
make build test lint      # before every commit
make e2e                  # requires kubectl in PATH
make snapshot             # optional: GoReleaser dry run (packages + cask)
```

- Format with `gofmt` (`make fmt`).
- Add unit tests for controller behaviour (`internal/sim`), API tests for new
  endpoints (`internal/api`), scenario lifecycle tests for new built-in
  challenges (`internal/scenario/scenario_test.go` has a `fixes` table).
- Conventional commit messages are appreciated (`feat:`, `fix:`, `docs:`).

## Adding a resource

1. Register it in `internal/registry/registry.go` (GVR, kind, scope, short names,
   categories, subresources).
2. Add table columns in `internal/api/table.go`.
3. If needed, add defaulting/validation in `internal/sim/admission.go` and a
   controller in `internal/sim`.
4. Add the upstream OpenAPI document for new groups (list it in
   `scripts/update-openapi.sh`, then `make openapi`).
5. Extend the e2e tests and the README.

## Adding a challenge

Write a YAML file (see [docs/scenarios.md](docs/scenarios.md)), validate it with
`kubedrill challenge validate`, and for built-ins add an automated fix to
`internal/scenario/scenario_test.go`.

## Reporting security issues

Please do not open public issues for vulnerabilities – see [SECURITY.md](SECURITY.md).
