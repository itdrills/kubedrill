# Architecture

KubeDrill is one Go binary. `kubedrill start` launches `kubedrill serve` as a detached
background process; every other command is a thin client.

```
cmd/kubedrill            main, version via -ldflags
internal/brand         all product names (rename here)
internal/cli           Cobra commands; admin API client; kubectl wrapper
internal/daemon        home dir, TLS (local CA), background process, runtime loop,
                       persistence, admin HTTP API (/kubedrill/v1/...)
internal/api           Kubernetes-compatible HTTP API
internal/sim           simulated cluster: admission + controllers + image catalog
internal/store         in-memory typed object store, resourceVersions, watch, snapshots
internal/registry      served resources (GVR, kind, scope, short names, categories)
internal/scenario      scenario format, validation, engine, success criteria
internal/kubeconfig    standalone kubeconfig + safe merge into the user's kubeconfig
internal/ui            `kubedrill ui`: loopback web page (embedded assets), token + host/origin checks, command allowlist (no shell)
test/e2e               real kubectl against a real kubedrill process (build tag e2e)
```

## Request path

1. kubectl connects to `https://127.0.0.1:<port>` with the bearer token from the
   kubeconfig. HTTPS is required because client-go does not send bearer tokens
   over plain HTTP; the CA is generated locally (`~/.kubedrill/pki`) and trusted only
   through the kubeconfig entry.
2. `internal/api` serves:
   - **Discovery**: legacy `/api`, `/apis`, `/api/v1`, `/apis/<g>/<v>`. kubectl first
     asks for aggregated discovery; because the server answers with plain
     `application/json`, client-go falls back to legacy discovery.
   - **OpenAPI v3**: the unmodified upstream documents for `api/v1`, `apps/v1`,
     `discovery.k8s.io/v1` are embedded (gzip, ~270 KB). They power
     `kubectl explain` and tell kubectl that the server supports the
     `fieldValidation` parameter, so kubectl relies on server-side validation.
   - **CRUD** with JSON and protobuf bodies (typed clients such as
     `kubectl create deployment` send protobuf), strict decoding of unknown fields
     (`fieldValidation=Strict|Warn|Ignore`), `dryRun`, optimistic concurrency.
   - **PATCH**: JSON patch, merge patch, strategic merge patch (using the typed Go
     structs from `k8s.io/api`), and a simplified server-side apply.
   - **Watch**: newline-delimited JSON events from the store's fan-out with a
     replay buffer of 2000 events (`410 Gone` for older versions), Table watches
     for `kubectl get -w`, and `sendInitialEvents` bookmarks for watch-list clients.
   - **Tables**: server-side printing (`as=Table`) with the same columns as the
     upstream printers (pods STATUS logic mirrors kubectl's).
   - Subresources: `pods/log`, `deployments|replicasets/scale`, `*/status`;
     `exec/attach/portforward` return a clear error.
3. Writes go through `sim.Cluster`, which holds a single lock, runs **admission**
   (defaulting + validation with API-server-style error messages) and stores the
   object, then runs the controllers until the state is stable.

## Simulation model

All controllers are deterministic functions of the stored state and the clock.
They run synchronously after each write (so `kubectl get` right after `create`
already shows pods) and once per second from the server's ticker (time-based
transitions such as image pulls, crash back-off and probes).

| Controller | Behaviour |
|---|---|
| namespace | default ServiceAccount + `kube-root-ca.crt` ConfigMap per namespace; namespace deletion removes contents |
| deployment | pod-template-hash, ReplicaSet per revision, revision annotations, rolling update (maxSurge/maxUnavailable), Recreate, rollback, revisionHistoryLimit, conditions incl. ProgressDeadlineExceeded |
| replicaset | creates/deletes pods (deterministic names), status counts |
| scheduler | filters: unschedulable, taints/tolerations, NotReady, nodeSelector, pod count, CPU/memory requests vs. allocatable; score: least allocated; upstream-style FailedScheduling messages |
| kubelet | per container: pull (online registry lookup via `internal/imagecheck` with offline catalog fallback, per-node image cache, pull policy) → config (env from ConfigMaps/Secrets, volumes) → start → run plan (long running, exit code, OOMKilled) → restart with back-off; readiness/liveness probes; pod phase and conditions; synthetic logs |
| endpoints | Endpoints and EndpointSlices from Service selectors and pod readiness |
| ingress | simulated ingress-nginx (addon in namespace `ingress-nginx`, default IngressClass `nginx`): DefaultIngressClass admission, publishes the address when the controller Deployment is available, reports missing Services/ports/endpoints after a 5 s grace period as Warning events and controller log lines |
| GC | cascading deletion via ownerReferences (background/foreground), orphaning |
| events | core/v1 Events with count aggregation, chronological names |

The image check (`internal/imagecheck`) is the only component that talks to
the network. `Lookup` never blocks: it returns `Pending` and resolves the
reference in a goroutine (anonymous bearer token + `HEAD /v2/<repo>/manifests/<ref>`,
max. 4 in parallel, 8 s timeout). The kubelet keeps the container in "Pulling"
until the answer arrives, so the cluster lock is never held during network I/O.
Results are cached in memory and in `image-cache.json`. Tests pass no verifier
(`sim.Options.ImageVerifier == nil`) and stay deterministic.

Generated names and UIDs come from a seeded counter, so a given sequence of
operations produces the same names (tests rely on this). The clock is an
interface: `RealClock` (with `--time-scale`) in the server, `FakeClock` in tests.
Simulation-internal state (container runtimes, IP allocation, counters) lives in
`sim.State` and is persisted together with the objects.

## Scenario engine

`internal/scenario` loads built-in scenarios (embedded YAML) and custom packs,
validates them strictly, and runs challenges inside a cluster transaction:
snapshot → choose target or create setup → render placeholders → apply JSON
patch → snapshot. Challenge state (hints revealed, solution revealed, solved,
self-solved) is persisted in the simulation state. Success criteria evaluate the
end state only. See [scenarios.md](scenarios.md).

## Persistence

`state.json` (objects + simulation state) is written atomically (temp file +
rename, mode 0600) at most every 2 seconds after changes and on shutdown. A
corrupt file is moved aside and the simulator starts fresh. SQLite was not
needed for the MVP; the store interface allows swapping persistence later.

## Interfaces that are meant to stay stable

- `registry.Resource` – adding a resource = registry entry + (optional) table
  columns + (optional) controller.
- `sim.Cluster` public methods (`Create`, `Mutate`, `Update`, `Delete`, `Logs`,
  `WithLock`/`Tx`, `Snapshot`/`Restore`) – used by the API and the scenario engine.
- Scenario YAML `itdrills.github.io/v1alpha1` – versioned; breaking changes get a new version.
- Admin API `/kubedrill/v1/...` – JSON, used by the CLI and later by integrations
  (e.g. learning platforms).

## Packaging

- `make package` cross-compiles static binaries (CGO disabled) for darwin/arm64,
  darwin/amd64, linux/amd64, linux/arm64 and writes `checksums.txt`.
- `make brew-local-install` installs through a local tap from a `file://` URL
  (development only).
- `.goreleaser.yaml` builds the same matrix for tagged releases: tarballs,
  `.deb` and `.rpm` (nfpm; rpm signed with the release GPG key), the GitHub
  release and a Homebrew cask in `itdrills/homebrew-tap`. `make snapshot` runs
  the whole pipeline locally without publishing.
- `scripts/publish-repos.sh` turns the packages into a signed apt repository
  (`apt-ftparchive`, InRelease/Release.gpg) and a dnf repository (`createrepo_c`,
  signed `repomd.xml`) on the `gh-pages` branch, served at
  https://itdrills.github.io/kubedrill/. Setup: [releasing.md](releasing.md).
- **Gatekeeper**: Go's linker ad-hoc signs darwin/arm64 binaries. Locally built or
  `file://`-installed binaries are not quarantined. For public downloads, sign
  with a Developer ID Application certificate and notarize (GoReleaser's
  `notarize.macos` section with `MACOS_SIGN_P12`, `MACOS_SIGN_PASSWORD`,
  `MACOS_NOTARY_KEY`, `MACOS_NOTARY_KEY_ID`, `MACOS_NOTARY_ISSUER_ID` secrets);
  this is prepared but disabled until certificates exist.
