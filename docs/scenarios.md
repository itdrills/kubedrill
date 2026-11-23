# Writing scenarios

A scenario (challenge) is a single YAML file. No Go code is needed: drop files
into `~/.kubedrill/scenarios/` (subdirectories allowed) or start the simulator with
`kubedrill start --scenarios ./my-pack`. A custom scenario with the same `id` as a
built-in one overrides it (a warning is logged).

Validate before use – validation is strict and works offline:

```sh
kubedrill challenge validate ./my-pack
```

Typos in field names are rejected (`unknown field "sucess"`), so a broken file
never silently does nothing. Invalid files in a pack are skipped with a warning
shown by `kubedrill challenge list`; the other scenarios keep working.

## Lifecycle

1. **Start** – a snapshot of the cluster is taken (`before`).
2. **Target selection** – if `spec.target` is set and a matching object exists in a
   non-system namespace (e.g. the learner's own Deployment), the fault is injected
   *there*. Otherwise the `spec.setup` manifests are created (existing objects
   with the same name are replaced; missing namespaces are created).
3. **Placeholders** – `{{target.name}}`, `{{target.namespace}}` and
   `{{target.kind}}` are replaced everywhere in the document (description,
   hints, success criteria, solution).
4. **Injection** – `spec.inject.patch` (RFC 6902 JSON patch) is applied to the
   target object. The result is strictly validated like any API request.
5. A second snapshot (`start`) is taken: the broken starting point.
6. **Hints** are revealed one level at a time (`kubedrill hint`, `--level N`).
7. **Check** evaluates `spec.success` against the *current cluster state* – it does
   not matter which commands the learner used.
8. **Solve** reveals `spec.solution`/`spec.explanation`; a later successful check
   counts as "completed with help", never as self-solved.
9. **Reset** restores the `start` snapshot; **stop** restores `before`
   (or keeps the state with `--keep`).

## Reference

```yaml
apiVersion: itskills.github.io/v1alpha1     # required
kind: Scenario                      # required
metadata:
  id: my-broken-app                 # required, lowercase, digits, dashes (<= 63)
  title: "Human readable title"     # required
  difficulty: beginner              # required: beginner | intermediate | advanced
  estimatedMinutes: 10
  tags: [pods, images]
  version: "1"
  author: "you"
spec:
  summary: One line for `challenge list`.          # required
  description: |                                   # required, shown on start
    The task for the learner. May use {{target.name}} placeholders.
  namespace: default        # default namespace for setup objects and criteria

  target:                   # optional: inject into an existing object if possible
    kind: Deployment        # any supported kind
    namespaces: [default]   # optional filter
    labelSelector: app=web  # optional filter

  setup: |                  # required if no target; fallback otherwise
    apiVersion: apps/v1     # multi-document YAML of supported resources
    kind: Deployment
    ...
    ---
    apiVersion: v1
    kind: Service
    ...

  inject:                   # optional: the fault as JSON patch on the target
    object:                 # required when no target: which setup object
      kind: Deployment
      name: web
    patch:
      - op: replace
        path: /spec/template/spec/containers/0/image
        value: nginx:1.27-doesnotexist

  hints:                    # required, ordered from gentle to concrete
    - "Level 1 ..."
    - "Level 2 ..."

  success:                  # required, all must pass
    - type: deploymentAvailable
      name: "{{target.name}}"
      namespace: "{{target.namespace}}"
      description: "Optional text shown in the check output"

  solution:                 # required
    diagnosis: "How to find the fault"
    rootCause: "Why it happens"
    fix: "What to change"
    commands:
      - kubectl set image deployment/{{target.name}} web=nginx:1.27

  explanation: |            # optional background shown with the solution
    ...
  reset:
    strategy: snapshot      # only "snapshot" is supported in v1alpha1
```

You can also put the fault directly into `setup` (no `inject`), e.g. a Service
with a wrong selector – see `internal/scenario/builtin/03-service-selector.yaml`.

### Success criteria

| type | fields | passes when |
|---|---|---|
| `deploymentAvailable` | `name`, `namespace` | observedGeneration is current and replicas = updated = available = desired > 0 |
| `podsReady` | `selector`, `namespace`, `min` (default 1) | at least `min` matching pods are Ready and **no** matching (non-completed) pod is unready |
| `serviceHasEndpoints` | `name`, `namespace`, `min` (default 1) | the Service's Endpoints list at least `min` ready addresses |
| `ingressReady` | `name`, `namespace` | the Ingress has an address and every backend names an existing Service port with at least one ready endpoint |
| `exists` / `absent` | `kind`, `name`, `namespace` | the object exists / does not exist |
| `field` | `kind`, `name`, `namespace`, `path` (JSON pointer) and one of `equals`, `notEquals`, `matches` (regex) | the field value matches; non-string values are compared as JSON |

### Simulated behaviour you can use

- **Images**: with the image check enabled (default) the real registry decides
  whether a tag exists. Offline or with `--image-check=off`, unknown images are
  assumed to exist; tags or names containing `doesnotexist`, `nonexistent`,
  `invalid`, `notfound` or `typo` fail to pull, as do typos of catalog images
  (`ngnix`) and catalog images with invalid tags (`nginx:9.99`). For a fault
  that behaves the same online and offline, use a tag like `1.27-doesnotexist`.
- **Ingress**: the simulated ingress-nginx controller assigns the address
  `172.18.0.2` and emits Warning events (`ServiceNotFound`,
  `ServicePortNotFound`, `NoActiveEndpoints`) for broken backends; use the
  `ingressReady` criterion to check an Ingress.
- **`kubedrill/demo-api:1.x`** listens on 8080, serves `/`, `/healthz`, `/ready`
  and exits with code 1 (CrashLoopBackOff) unless `DATABASE_URL` is set.
- **`postgres` / `mysql`** require `POSTGRES_PASSWORD` / `MYSQL_ROOT_PASSWORD`.
- **Shell images** (`busybox`, `alpine`, `ubuntu`, `debian`, `python`) interpret
  `command`/`args`: `sleep 3600`, `sleep infinity`, `tail -f`, `while true` keep
  running, `exit N` exits with N, anything else completes. `echo` text becomes logs.
- **Memory limits** below the image's working set → `OOMKilled` (exit 137).
  `kubedrill/memory-hog` needs ~700Mi.
- **Probes**: HTTP probes fail with "connection refused" on ports the process does
  not listen on and with 404 on unknown paths (for images with known paths);
  exec probes containing `false` or `exit 1` fail. Liveness failures restart the
  container.
- **Scheduling**: 3 nodes – a tainted control plane (4 CPU/8Gi) and two workers
  (2 CPU/~4Gi). Requests above that stay `Pending`.
- **Config**: missing ConfigMaps/Secrets in env → `CreateContainerConfigError`;
  as volumes → `ContainerCreating` with `FailedMount` events.

### Secrets in scenarios

Never put real credentials into scenarios. Use obviously fake values
(`not-a-real-password`).
