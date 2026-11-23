# KubeDrill

**Practice Kubernetes. No Cluster Required.**

[![ci](https://github.com/itdrills/kubedrill/actions/workflows/ci.yml/badge.svg)](https://github.com/itdrills/kubedrill/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/itdrills/kubedrill)](https://github.com/itdrills/kubedrill/releases)
[![license](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

KubeDrill is a lightweight, local Kubernetes *simulator*. You use the **real
`kubectl`** against a simulated cluster to learn commands, resources and
troubleshooting – without a control plane, etcd, containers, Docker or VMs.
Built-in challenges inject realistic faults (ImagePullBackOff, CrashLoopBackOff,
broken Service selectors, Ingress port mismatches, …), give graded hints and
check the end state of the cluster.

```console
$ kubedrill start
$ kubectl --context kubedrill create deployment nginx --image=nginx:1.27
$ kubectl --context kubedrill get deployments,pods
NAME                    READY   UP-TO-DATE   AVAILABLE   AGE
deployment.apps/nginx   1/1     1            1           4s

NAME                         READY   STATUS    RESTARTS   AGE
pod/nginx-574fbc9564-ssvv7   1/1     Running   0          4s
$ kubedrill challenge start image-pull-backoff
$ kubectl --context kubedrill get pods
NAME                     READY   STATUS             RESTARTS   AGE
nginx-574fbc9564-ssvv7   1/1     Running            0          8s
nginx-7ff4b6c78d-vqsmz   0/1     ImagePullBackOff   0          5s
```

Everything is simulated: **no container image is ever pulled or executed.**
The only network traffic leaving your machine is the optional image check:
anonymous `HEAD` requests that verify an image tag exists in its registry (see
[Image check](#image-check)). Disable it with `kubedrill start --image-check=off`
and KubeDrill works completely offline.

> Status: early release (v0.x). Feedback and challenge ideas are welcome in the
> [issues](https://github.com/itdrills/kubedrill/issues).

---

## Contents

- [Installation](#installation)
- [Quick start](#quick-start)
- [Working with the simulated cluster](#working-with-the-simulated-cluster)
- [Challenges](#challenges)
- [Browser UI](#browser-ui)
- [Safety: your real clusters stay untouched](#safety-your-real-clusters-stay-untouched)
- [Command reference](#command-reference)
- [Supported subset](#supported-subset)
- [Limits of the simulation](#limits-of-the-simulation)
- [Image check](#image-check)
- [Uninstall](#uninstall)
- [Architecture](#architecture) · [Roadmap](#roadmap) · [Development](#development)

## Installation

KubeDrill is a single static binary for macOS and Linux (amd64 and arm64). You
also need [`kubectl`](https://kubernetes.io/docs/tasks/tools/) in your `PATH`
(tested with v1.37; the simulator reports Kubernetes v1.37.2).
`kubedrill doctor` checks your setup.

### macOS – Homebrew

```sh
brew install itdrills/tap/kubedrill
brew install kubectl        # if you do not have kubectl yet
```

Upgrade with `brew upgrade kubedrill`.

### Debian / Ubuntu – apt

```sh
sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://itdrills.github.io/kubedrill/gpg.key \
  | sudo gpg --dearmor -o /etc/apt/keyrings/kubedrill.gpg
echo "deb [signed-by=/etc/apt/keyrings/kubedrill.gpg] https://itdrills.github.io/kubedrill/deb stable main" \
  | sudo tee /etc/apt/sources.list.d/kubedrill.list
sudo apt update
sudo apt install kubedrill
```

Updates arrive with `sudo apt upgrade`. The repository is signed; apt refuses
packages whose signature does not match the key.

### Fedora / RHEL / Rocky / Alma – dnf

```sh
sudo curl -fsSL -o /etc/yum.repos.d/kubedrill.repo https://itdrills.github.io/kubedrill/kubedrill.repo
sudo dnf install kubedrill
```

dnf asks you to confirm the signing key on first install. Updates arrive with
`sudo dnf upgrade`.

### Any Linux or macOS – binary download

Download the archive for your platform from the
[releases page](https://github.com/itdrills/kubedrill/releases), verify it
against `checksums.txt` and put the binary into your `PATH`:

```sh
VERSION=0.1.0   # see the releases page
OS=linux        # or darwin
ARCH=amd64      # or arm64
curl -fsSLO "https://github.com/itdrills/kubedrill/releases/download/v${VERSION}/kubedrill_${VERSION}_${OS}_${ARCH}.tar.gz"
curl -fsSLO "https://github.com/itdrills/kubedrill/releases/download/v${VERSION}/checksums.txt"
sha256sum --ignore-missing -c checksums.txt     # macOS: shasum -a 256 --ignore-missing -c checksums.txt
tar -xzf "kubedrill_${VERSION}_${OS}_${ARCH}.tar.gz" kubedrill
sudo install -m 0755 kubedrill /usr/local/bin/kubedrill
```

### From source

With Go 1.27.4 or newer (older Go versions download the required toolchain automatically):

```sh
go install github.com/itdrills/kubedrill/cmd/kubedrill@latest
```

or clone the repository and run `make build` (binary in `bin/kubedrill`).

## Quick start

```sh
kubedrill start                       # starts the simulator in the background
kubectl --context kubedrill get nodes
```

`kubedrill start` launches a local API server on `https://127.0.0.1:16443` and
adds a `kubedrill` context to your kubeconfig. **Your current context is not
changed**, so plain `kubectl` keeps talking to whatever it talked to before.
Choose one of three ways to reach the simulator:

```sh
kubectl --context kubedrill get pods     # explicit context (recommended)
kubedrill kubectl get pods               # wrapper: always the simulator, refuses --context
kubectl config use-context kubedrill     # switch for good – remember to switch back!
```

Stop the simulator with `kubedrill stop`; its state is saved and restored on the
next start. `kubedrill reset` gives you a pristine cluster.

## Working with the simulated cluster

All examples use plain `kubectl` with `--context kubedrill` (omitted below for
readability, e.g. after `kubectl config use-context kubedrill`).

**Pods**

```sh
kubectl run nginx --image=nginx:1.27
kubectl get pods -o wide
kubectl logs nginx
kubectl describe pod nginx
kubectl run broken --image=nginx:9.99      # tag does not exist -> ErrImagePull / ImagePullBackOff
```

**Deployments and Services**

```sh
kubectl create deployment web --image=nginx:1.27 --replicas=3 --port=80
kubectl expose deployment web --port=80
kubectl get deploy,rs,pods,svc,endpoints
kubectl set image deployment/web nginx=nginx:1.28
kubectl rollout status deployment/web
kubectl rollout history deployment/web
kubectl rollout undo deployment/web
kubectl scale deployment/web --replicas=1
```

**Ingress** – a simulated ingress-nginx controller and the default IngressClass
`nginx` are pre-installed (namespace `ingress-nginx`):

```sh
kubectl create ingress web --rule="web.local/*=web:80"
kubectl get ingress                         # ADDRESS is filled in by the controller
kubectl describe ingress web                # backends with their endpoints
kubectl logs -n ingress-nginx deploy/ingress-nginx-controller
```

**Manifests** – `kubectl apply -f`, `edit`, `patch`, `label`, `annotate`,
`explain`, `get -w` and `--dry-run=server` work as on a real cluster, including
server-side validation of unknown fields.

What does not work: `exec`, `port-forward`, `cp`, `top` – nothing runs, so they
return a clear error. See [Supported subset](#supported-subset).

## Challenges

```sh
kubedrill challenge list
kubedrill challenge start image-pull-backoff   # or without id: first unsolved
kubedrill challenge status                     # task and live state of the success criteria
kubedrill hint                                 # next hint (graded)
kubedrill challenge check                      # evaluates the cluster state
kubedrill solve                                # diagnosis + fix, counts as "completed with help"
kubedrill challenge reset                      # back to the broken starting point
kubedrill challenge stop                       # end and restore the state before the challenge
```

Challenges check the **end state** of the cluster, not the commands you typed –
any correct fix counts.

## Browser UI

Prefer a browser over the terminal? With the simulator running:

```sh
kubedrill ui            # prints a link and opens it in your default browser
kubedrill ui --no-open  # only print the link
```

The page shows a command line, the task of the active challenge with the live
state of its success criteria, and buttons for hint, check, reset, solve and
stop. Clicking a challenge in the list starts it. Every button runs the same
command you would type (`kubedrill hint`, `kubedrill challenge check`, …), so
you learn the CLI along the way. Stop the UI with Ctrl+C; the simulator keeps
running.

The command line is **not a shell**. It accepts `kubectl …` with the common
subcommands (get, describe, logs, create, apply, delete, patch, set, scale,
rollout, …) and `kubedrill challenge|hint|solve|status`. Pipes, redirects,
variables, `exec`, `port-forward`, `edit`, plugins, watches (`-w`, `logs -f`)
and flags that point kubectl elsewhere (`--context`, `--kubeconfig`, …) are
refused – use your own terminal for those.

How the page is protected:

- It listens on **127.0.0.1 only**, on a free port (or `--port`).
- The printed link carries a **random one-time token**. Opening it sets a
  `HttpOnly`, `SameSite=Strict` cookie and removes the token from the address
  bar; requests without the cookie get `401`. Don't share the link.
- Requests must name `127.0.0.1:<port>` or `localhost:<port>` in the `Host`
  header (protects against DNS rebinding), and commands are only accepted from
  the page's own origin. No CORS headers are sent, so other websites cannot
  read or drive the UI.
- A strict Content Security Policy allows only the page's own embedded files.
  The page loads nothing from the internet; HTML, CSS and JavaScript are
  embedded in the binary.
- Commands are split without a shell and run with the simulator's kubeconfig
  and context only, with a 30-second timeout.

## Safety: your real clusters stay untouched

- The API server binds to **127.0.0.1 only** and serves HTTPS with a CA generated
  locally in `~/.kubedrill/pki` (trusted only via the kubedrill kubeconfig entry).
  Every request needs a random bearer token stored in `~/.kubedrill/token` (0600).
- `kubedrill start` writes a standalone kubeconfig (`~/.kubedrill/kubeconfig`) and adds
  **only** the entries named `kubedrill` (cluster, user, context) to the kubeconfig
  kubectl uses (`$KUBECONFIG` or `~/.kube/config`), using kubectl's own merge
  logic. Other entries and your **current-context are never changed** unless you
  pass `--set-current-context`. A backup is written to `~/.kubedrill/backups/` before
  every modification. `--no-kubeconfig` skips the merge entirely.
- If an existing `kubedrill` entry points to a non-loopback server, KubeDrill refuses
  to touch it.
- Challenge commands (`challenge`, `hint`, `solve`, `reset`) talk **only** to the
  simulator recorded in `~/.kubedrill/server.json`, verified by an instance ID –
  never to anything from your kubeconfig. Before a challenge starts, KubeDrill
  prints which context plain `kubectl` would use and warns if it is not `kubedrill`.
- `kubedrill kubectl` refuses `--context`, `--kubeconfig`, `--server`, `--cluster`,
  `--user`, `--token` and `--as`.
- `kubedrill stop` leaves your kubeconfig as it is; `kubedrill kubeconfig remove`
  deletes the `kubedrill` entries.
- The end-to-end tests run a fake "production" API server in the kubeconfig and
  assert that it receives **zero** requests and that its entries are unchanged.

Files in `~/.kubedrill` (override with `KUBEDRILL_HOME`): `kubeconfig`, `server.json`,
`state.json`, `server.log`, `token`, `pki/`, `backups/`, `scenarios/`.

## Command reference

| Command | Description |
|---|---|
| `kubedrill start [--port 16443] [--fresh] [--no-kubeconfig] [--set-current-context] [--image-check auto\|off] [--time-scale 1] [--scenarios DIR] [--foreground] [--verbose]` | Start the simulator in the background |
| `kubedrill stop` | Stop the simulator; state is saved to `state.json` |
| `kubedrill status` | Show address, PID, kubeconfig and active challenge |
| `kubedrill reset` | Reset the cluster to its pristine state (deletes your resources and challenge progress) |
| `kubedrill kubectl <args>` | Run kubectl against the simulator only |
| `kubedrill kubeconfig merge [--set-current-context]` / `remove` / `path` | Manage the `kubedrill` context |
| `kubedrill challenge list` | List challenges (built-in and custom) |
| `kubedrill challenge start [id]` | Start a challenge (first unsolved if no id) |
| `kubedrill challenge status` | Task, hints used, live state of the success criteria |
| `kubedrill challenge check` | Evaluate the end state; exit code 1 if not solved |
| `kubedrill challenge reset` | Restore the broken starting point of the active challenge |
| `kubedrill challenge stop [--keep]` | End the challenge and restore the pre-challenge state (or keep it) |
| `kubedrill challenge validate <file\|dir>...` | Strictly validate scenario YAML files (offline) |
| `kubedrill hint [--level N]` | Show the next (or a specific) hint |
| `kubedrill solve` | Show diagnosis, root cause, fix and commands – the cluster is **not** changed and the result is recorded as "completed with help" |
| `kubedrill ui [--port 0] [--no-open]` | Browser UI on 127.0.0.1: command line, challenge task, hints and check ([details](#browser-ui)) |
| `kubedrill doctor` | Check kubectl, server and kubeconfig setup |
| `kubedrill version` | Print versions |

Built-in challenges (`kubedrill challenge list`):

| ID | Difficulty | Fault |
|---|---|---|
| `image-pull-backoff` | beginner | Deployment updated to a non-existent image tag; rollout stuck (injected into **your** existing Deployment if you have one) |
| `crashloop-missing-env` | beginner | App exits because `DATABASE_URL` is missing; synthetic crash logs |
| `service-selector` | beginner | Service selector typo; no endpoints |
| `missing-configmap` | beginner | `configMapKeyRef` to a missing ConfigMap → `CreateContainerConfigError` |
| `readiness-probe` | intermediate | Readiness probe on a path that returns 404; Running but 0/1 |
| `pending-resources` | intermediate | Memory requests larger than any node → `Pending` / `FailedScheduling` |
| `ingress-wrong-port` | intermediate | Ingress backend points to a Service port that does not exist → controller warnings, 503 |

Write your own scenario packs without touching Go code: see
[docs/scenarios.md](docs/scenarios.md). Put them into `~/.kubedrill/scenarios/` or
pass `--scenarios DIR` to `kubedrill start`.

## A complete troubleshooting run

The following run is the real output of the commands (shortened):

```console
$ kubedrill challenge start image-pull-backoff
Challenge started: Rollout stuck: the new image cannot be pulled  [beginner]
  A colleague just rolled out a new version of the Deployment
  "web" in namespace "default".
  The rollout never finishes and one pod is not starting.

$ kubectl --context kubedrill get pods
NAME                   READY   STATUS             RESTARTS   AGE
web-7655c9f8c9-pfjrb   1/1     Running            0          6s
web-7655c9f8c9-qsmwg   1/1     Running            0          6s
web-7655c9f8c9-xzx78   1/1     Running            0          6s
web-855f89b5dc-hxfm2   0/1     ImagePullBackOff   0          6s

$ kubectl --context kubedrill describe pod web-855f89b5dc-hxfm2
...
Events:
  Type     Reason     Age   From               Message
  Normal   Scheduled  8s    default-scheduler  Successfully assigned default/web-855f89b5dc-hxfm2 to kubedrill-worker2
  Normal   Pulling    8s    kubelet            Pulling image "nginx:1.27-doesnotexist"
  Warning  Failed     7s    kubelet            Failed to pull image "nginx:1.27-doesnotexist": rpc error: code = NotFound desc = failed to pull and unpack image "docker.io/library/nginx:1.27-doesnotexist": ... not found
  Warning  Failed     7s    kubelet            Error: ErrImagePull
  Normal   BackOff    5s    kubelet            Back-off pulling image "nginx:1.27-doesnotexist"
  Warning  Failed     5s    kubelet            Error: ImagePullBackOff

$ kubedrill challenge check
Success criteria:
  [ ] Deployment web is fully rolled out and all replicas are available  (replicas=4 updated=1 available=3 desired=3)
  [ ] The Deployment no longer references the non-existent image tag  (value "nginx:1.27-doesnotexist")
Not solved yet. Need help? `kubedrill hint` reveals the next hint.

$ kubedrill hint
Hint 1/4:
  Start broad: `kubectl get deployments,pods -n default`. Which pod is not Running, and what does its STATUS say?

$ kubectl --context kubedrill set image deployment/web web=nginx:1.27
deployment.apps/web image updated
$ kubectl --context kubedrill rollout status deployment/web
deployment "web" successfully rolled out

$ kubedrill challenge check
Success criteria:
  [x] Deployment web is fully rolled out and all replicas are available  (replicas=3 updated=3 available=3 desired=3)
  [x] The Deployment no longer references the non-existent image tag  (value "nginx:1.27")
Solved! Well done – you fixed it on your own (1 hint(s) used).

$ kubedrill challenge stop
Challenge "image-pull-backoff" ended: solved on your own.
The cluster was restored to the state before the challenge.
```

## Supported subset

Verified with the real kubectl v1.37 by the end-to-end tests (`make e2e`) and
manual runs:

| Area | Supported |
|---|---|
| Discovery | legacy discovery (`/api`, `/apis`, group versions), `kubectl api-resources`, `kubectl version` |
| OpenAPI | upstream OpenAPI v3 documents embedded → `kubectl explain`, server-side field validation (`--validate=strict` errors on unknown fields) |
| Resources | Namespace, Node, Pod, Deployment, ReplicaSet, Service, Endpoints, EndpointSlice, ConfigMap, Secret, ServiceAccount, Event, Ingress, IngressClass |
| Ingress | `create ingress`, `get/describe ingress` (backends with endpoints), default IngressClass `nginx`, simulated ingress-nginx controller in namespace `ingress-nginx` (publishes the ADDRESS, warns about missing Services/ports/endpoints in events and controller logs), validation like the API server |
| Reads | `get` (tables, `-o wide/yaml/json/name/jsonpath`, `-A`, `-n`, `-l`, `--field-selector`, `--show-labels`), `get -w` (watch), `describe`, `logs` (synthetic, `--previous`, `--tail`, `-c`), `get events` |
| Writes | `create` (incl. `create deployment/configmap/secret/namespace/service`), `apply -f` (client-side), `apply --server-side` (simplified, no field ownership), `delete` (cascading), `edit`, `patch` (strategic/merge/json), `label`, `annotate`, `set image/env/resources`, `scale`, `expose` |
| Rollouts | `rollout status`, `rollout history`, `rollout undo`, rolling update with maxSurge/maxUnavailable, Recreate |
| API semantics | resourceVersions + optimistic concurrency (409), watch with replay buffer (410 Gone when too old), `dryRun=All`, generation/observedGeneration, Table output, protobuf and JSON request bodies |
| Not available | `exec`, `attach`, `port-forward`, `cp`, `top`, `auth can-i`, CRDs, RBAC enforcement – they return a clear "not available in the simulator" error or "the server doesn't have a resource type" |

## Limits of the simulation

- **Nothing runs.** Whether a pull succeeds is decided by the [image check](#image-check);
  how a container behaves (ports, required env vars, memory, logs) comes from a
  small built-in catalog (`nginx`, `httpd`, `redis`, `postgres`, `mysql`,
  `busybox`, `alpine`, `ubuntu`, `debian`, `python`, `kubedrill/demo-api`, …).
  Other images behave like a generic web server. Logs are synthetic.
- **Ingress routes no traffic.** The simulated controller only publishes the
  address and reports broken backends; there is no HTTP endpoint to `curl`.
- **Networking is simulated**: Cluster IPs and pod IPs are not routable, Services
  have endpoints but no traffic flows, there is no DNS and no port-forward.
  LoadBalancer services stay `<pending>`. No port other than the API port is bound.
- **Simplified controllers**: deployment rolling updates, ReplicaSets, scheduling
  (taints/tolerations, nodeSelector, requests vs. allocatable), probes, restarts
  and back-off are modelled; affinity, topology spread, preemption, PDBs,
  terminating grace periods, finalizers and init containers are not (yet).
- **Time** runs on a virtual clock (real time × `--time-scale`). Back-offs follow
  Kubernetes (10s, 20s, 40s … 5m).
- **Server-side apply** is approximated (create or strategic merge; no managed
  fields/conflict detection). Client-side `kubectl apply` is fully supported.
- No authentication/authorization model beyond the single local token.
- State is kept in memory and persisted as JSON (`~/.kubedrill/state.json`).

## Image check

Like a real kubelet, KubeDrill asks the image's registry whether the reference
exists: Docker Hub, ghcr.io, quay.io, registry.k8s.io and any other OCI
registry that allows anonymous pulls.

| Registry answer | Pod |
|---|---|
| tag/digest exists | pull succeeds, `imageID` carries the real digest |
| tag/digest unknown | `ErrImagePull` → `ImagePullBackOff` ("not found") |
| repository unknown or private | `ErrImagePull` ("pull access denied") |
| unreachable (offline, timeout, rate limit) | built-in catalog decides; Warning event `RegistryUnreachable` |

- Only anonymous `HEAD` requests on the manifest are sent; they do not count
  towards the [Docker Hub pull limit](https://docs.docker.com/docker-hub/usage/pulls/).
  No credentials are used, so private images count as "pull access denied".
- Results are cached in `~/.kubedrill/image-cache.json` (existing: 24 h, missing:
  10 min), so later starts work without network.
- Simulator-only images (`kubedrill/demo-api`, `kubedrill/memory-hog`) are never
  looked up.
- `kubedrill start --image-check=off` (or `KUBEDRILL_IMAGE_CHECK=off`) disables the
  lookup: then unknown images are assumed to exist and only typos of well-known
  images (e.g. `ngnix`) and tags like `…-doesnotexist` fail. Tests and CI use
  this mode for determinism.
- `kubedrill status` shows the active mode.

## Architecture

```
kubectl ──HTTPS──▶ api (discovery, OpenAPI, CRUD, patch, watch, tables, logs)
                     │
kubedrill CLI ──▶ admin API ──▶ scenario engine (inject, hints, check, reset)
                     │                   │
                     ▼                   ▼
               sim.Cluster: admission (defaults, validation) + controllers
               (deployment, replicaset, scheduler, kubelet, endpoints, ingress,
               GC, events)            kubelet ──▶ imagecheck (OCI registry HEAD, cache)
                     │
                     ▼
               store (typed objects, resourceVersion, watch fan-out, snapshots)
                     │
                     ▼
               daemon: background process, TLS, ticker, persistence (state.json)
```

Details: [docs/architecture.md](docs/architecture.md).

## Roadmap

1. More resources: StatefulSet, DaemonSet, Job, CronJob, PVC/PV, ServiceAccount
   tokens, Role/RoleBinding (with `auth can-i`), NetworkPolicy, HPA, PDB.
2. More challenges: RBAC, PVC binding, DNS, NetworkPolicy, Node NotReady,
   OOMKilled, liveness-probe restarts, composite failures.
3. Aggregated discovery (v2), real server-side apply with managed fields.
4. Optional simulated HTTP demo endpoint for Services (explicitly opt-in).
5. Scenario packs from URLs with progress export (no login required).
6. Signed and notarized macOS binaries (Developer ID).

## Uninstall

```sh
kubedrill stop
kubedrill kubeconfig remove        # removes the kubedrill entries from your kubeconfig
rm -rf ~/.kubedrill                # state, local CA, token, image cache

brew uninstall kubedrill                                          # Homebrew
sudo apt remove kubedrill && sudo rm /etc/apt/sources.list.d/kubedrill.list /etc/apt/keyrings/kubedrill.gpg   # apt
sudo dnf remove kubedrill && sudo rm /etc/yum.repos.d/kubedrill.repo                                          # dnf
```

## Development

```sh
make help         # all targets
make test         # unit + API tests (race detector)
make e2e          # real kubectl against a real kubedrill process
make lint         # gofmt, go vet, golangci-lint/staticcheck if installed
make snapshot     # full GoReleaser dry run: archives, .deb, .rpm, Homebrew cask in dist/
make smoke-test   # isolated smoke test (never touches ~/.kube/config)
```

See [CONTRIBUTING.md](CONTRIBUTING.md) and, for maintainers,
[docs/releasing.md](docs/releasing.md). Security issues: [SECURITY.md](SECURITY.md).

## License

Licensed under the [Apache License 2.0](LICENSE). The embedded OpenAPI documents
are © The Kubernetes Authors, Apache-2.0 (see
[internal/api/openapi/README.md](internal/api/openapi/README.md)).
