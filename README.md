# orchard-gh-bridge

Autoscaling GitHub Actions runners on ephemeral macOS VMs, orchestrated by [Orchard](https://tart.run/orchard/)/[Tart](https://tart.run) on Apple Silicon.

## Overview

`orchard-gh-bridge` is a lightweight Go service that connects GitHub Actions to Orchard, an orchestrator for [Tart](https://tart.run) macOS virtual machines running on Apple Silicon hardware.

It uses the [GitHub Actions Runner Scale Set API](https://github.com/actions/scaleset) to listen for CI job demand, then provisions ephemeral macOS VMs via the [official Orchard Go client library](https://github.com/cirruslabs/orchard). Each job gets a clean, isolated VM that is automatically destroyed after the job completes.

**Key features:**

- **Ephemeral VMs** -- every CI job runs in a fresh macOS VM. No shared state between jobs.
- **Autoscaling** -- VMs are created on demand and destroyed when jobs finish. No idle runners.
- **Auto-detected capacity** -- automatically discovers connected workers and their `tart-vms` slots. Refreshes every 60 seconds as workers come and go.
- **Multiple scale sets** -- run different Xcode/macOS versions side by side (e.g. `runs-on: macos-sequoia-xcode-16`).
- **Docker support** -- each VM gets Docker, Docker Compose, and Docker Buildx via Colima, with nested virtualization enabled.
- **Runner auto-update** -- always downloads the latest GitHub Actions runner from the releases API, avoiding version deprecation.
- **GitHub runner cleanup** -- automatically deregisters stale runner registrations from GitHub when VMs are deleted.
- **Helm chart** -- deploy to any Kubernetes cluster. Published as an OCI artifact for `helm install`.
- **Pull-through cache support** -- configurable image registry for air-gapped or cached environments.

## Architecture

```
Kubernetes cluster
+---------------------------------------------------------+
|                                                         |
|  orchard-gh-bridge (Deployment, 1 replica)              |
|    +-- listener goroutine per scale set (long-poll)     |
|    |     +-- implements scaleset.Scaler interface       |
|    +-- cleanup goroutine (60s interval)                 |
|    |     +-- reaps stopped/failed/stale VMs             |
|    |     +-- deregisters GitHub runners                 |
|    |     +-- refreshes capacity from workers            |
|    +-- health server (/healthz, /readyz)                |
|                                                         |
|  Orchard controller (StatefulSet)                       |
|    +-- REST API: VM create/delete, worker listing       |
|    +-- Scheduler: assigns pending VMs to workers        |
|                                                         |
+---------+-------------------------------+---------------+
          |                               |
          | GitHub Scale Set API          | Orchard worker protocol
          | (HTTPS, long-poll)            | (outbound from workers)
          v                               v
   GitHub Actions                 Mac Minis (Apple Silicon)
   +------------+                 +-- Worker 1 (tart-vms: 2)
   | Job queue  |                 |   +-- Tart VM (job A)
   | Scale sets |                 |   +-- Tart VM (job B)
   +------------+                 +-- Worker 2 (tart-vms: 1)
                                      +-- Tart VM (job C)
```

### How it works

1. The bridge registers one or more **runner scale sets** with GitHub Actions.
2. For each scale set, a **listener goroutine** long-polls GitHub for job demand.
3. When GitHub assigns jobs, the bridge generates a **JIT runner config** (single-use credential) and creates an **Orchard VM** with a startup script.
4. The Orchard controller's **scheduler** assigns the VM to an available worker based on architecture, runtime compatibility, and resource availability.
5. The worker boots the VM via Tart and executes the startup script over SSH (default credentials: `admin`/`admin`).
6. The **startup script** inside the VM:
   - Installs Docker, Docker Compose, Docker Buildx, and Colima via Homebrew
   - Starts Colima (Docker runtime for macOS, uses nested virtualization)
   - Downloads the **latest** GitHub Actions runner from the releases API
   - Runs the runner with the JIT config, which connects to GitHub and picks up the assigned job
7. When the job completes, the bridge **deletes the VM** from Orchard and **deregisters the runner** from GitHub.
8. A background **cleanup goroutine** runs every 60 seconds to:
   - Reap VMs that are stopped, failed, or exceed a safety timeout (default 2h, override with `maxVMAge`)
   - Deregister stale GitHub runner registrations for cleaned-up VMs
   - Refresh the maximum capacity from connected workers' `org.cirruslabs.tart-vms` resources
   - Reconcile the in-use capacity count with actual VM count in Orchard

### VM configuration

All VMs are created with the following settings via the [official Orchard client library](https://github.com/cirruslabs/orchard/pkg/client):

| Setting | Value | Notes |
|---------|-------|-------|
| OS | `darwin` | macOS |
| Architecture | `arm64` | Apple Silicon |
| Runtime | `tart` | Tart hypervisor |
| Headless | `true` | No GUI needed for CI |
| Nested virtualization | Configurable (`vm.nested`) | Required for Docker/Colima. Only works on M3+ Macs. |
| SSH credentials | `admin`/`admin` | Default for Cirrus Labs Tart images |
| Restart policy | `Never` | Ephemeral, one job per VM |
| Labels | Configurable (`vm.labels`) | Used as worker-affinity constraints for placement. See below. |
| Name prefix | `gha-orchard-` | Used by cleanup to identify managed VMs |

### Worker labels and VM placement

Orchard's scheduler uses **VM labels as worker-affinity constraints** -- a VM is only placed on workers whose labels are a superset of the VM's labels. This enables hardware-aware placement:

- **Nested virtualization**: M3+ Macs support nested virt (required for Docker/Colima). M1/M2 Macs do not. Label M3+ workers with `nested-virt: "true"` and set `vm.nested: true` + `vm.labels.nested-virt: "true"` on scale sets that need Docker.
- **Memory classes**: Workers with different RAM can be labeled (e.g., `vm-class: large` vs `vm-class: standard`) to route VMs to appropriate hardware.

When `vm.nested: true` is set, the startup script automatically installs Docker, Docker Compose, Docker Buildx, and Colima. When `false`, Docker is skipped entirely.

**Example worker setup:**

```bash
# M3 Mac Mini, 32GB -- supports nested virt and large VMs
orchard worker run \
  --labels nested-virt=true,vm-class=large \
  --resources org.cirruslabs.tart-vms:2

# M1 Mac Mini, 16GB -- standard VMs only, no nested virt
orchard worker run \
  --labels vm-class=standard \
  --resources org.cirruslabs.tart-vms:1
```

**Example scale set config:**

```yaml
scaleSets:
  # Docker-enabled scale set -- only runs on M3+ workers
  - name: macos-tahoe-docker
    githubConfigURL: https://github.com/your-org
    labels: [self-hosted, macOS, ARM64, docker]
    vm:
      image: ghcr.io/cirruslabs/macos-tahoe-xcode:26.4
      cpu: 4
      memory: 16384
      nested: true
      labels:
        nested-virt: "true"
        vm-class: "large"

  # Standard scale set -- runs on any worker
  - name: macos-tahoe
    githubConfigURL: https://github.com/your-org
    labels: [self-hosted, macOS, ARM64]
    vm:
      image: ghcr.io/cirruslabs/macos-tahoe-xcode:26.4
      cpu: 4
      memory: 8192
      labels:
        vm-class: "standard"
```

Then in workflows:

```yaml
jobs:
  build-with-docker:
    runs-on: macos-tahoe-docker
    steps:
      - run: docker compose up -d  # Docker available!

  build-standard:
    runs-on: macos-tahoe
    steps:
      - run: xcodebuild build       # No Docker, but works on any Mac
```

### Capacity management

The bridge tracks VM capacity with a global semaphore shared across all scale sets:

- **Auto-detection (default):** On startup, the bridge queries all Orchard workers and sums their `org.cirruslabs.tart-vms` resources. This is refreshed every 60 seconds by the cleanup goroutine, so capacity adjusts as workers come and go.
- **Static override:** Set `maxVMs` in config to a fixed number to override auto-detection.
- **Per-scale-set limits:** Set `maxRunners` on individual scale sets to cap them independently of the global limit.

### Per-host VM sizing (AutoSize)

By default `vm.cpu` and `vm.memory` are fixed per scale set, so every VM in that scale set gets the same allocation regardless of the host it lands on. That wastes capacity on heterogeneous fleets (e.g. one 10-core Mac Mini next to a 12-core MacBook Pro).

Setting `vm.autoSize.enabled: true` on a scale set switches it to per-host sizing:

```yaml
scaleSets:
  - name: macos-tahoe-xcode-26.4-large
    labels: [macOS, ARM64, macos-tahoe-xcode-26.4-large]
    vm:
      image: ghcr.io/cirruslabs/macos-tahoe-xcode:26.4.1
      dockerPort: 2375
      autoSize:
        enabled: true
        reserveCPU: 4         # Held back for macOS + orchard-worker + Colima
        reserveMemoryMiB: 4096
```

When AutoSize is on:

- `vm.cpu` and `vm.memory` are ignored. Each VM is created with `cpu = workerCores − reserveCPU`, `memory = workerMemoryMiB − reserveMemoryMiB`.
- The bridge picks one VM per worker and pins placement via a label, so a worker's full resources go to one VM at a time — regardless of its `tart-vms` slot count.
- Per-scale-set capacity is reported to GitHub as the count of label-matching workers, not the sum of their slots.
- The defaults (4 cores / 4096 MiB reserve) leave room for macOS, the orchard-worker daemon, and a default Colima Docker VM on the host. Bump them if your Colima profile is bigger.

**Worker setup for AutoSize:** each eligible worker must self-label with its own Orchard Name so the bridge can pin a VM there. The label key is `orchard-gh-bridge/worker-name`:

```bash
orchard worker run \
  --name my-mac-mini-1 \
  --labels "vm-class-large=true,orchard-gh-bridge/worker-name=my-mac-mini-1" \
  --resources "org.cirruslabs.tart-vms:1,org.cirruslabs.logical-cores:10,org.cirruslabs.memory-mib:16384"
```

The bridge logs `created autoSize VM ... worker=<name> cpu=<n> memoryMiB=<n>` for each provisioning, so the chosen size is visible in real time.

### Stateless design

The bridge is stateless -- all persistent state lives in Orchard and GitHub. If the bridge restarts:

- **Running VMs continue** -- they are managed by Orchard workers, not the bridge
- **GitHub replays pending jobs** -- the listener reconnects and receives current job assignments
- **Cleanup catches orphans** -- any VMs from the previous instance are detected by name prefix (`gha-orchard-*`) and cleaned up
- **Capacity reconciles** -- the cleanup goroutine resets the in-use count to match actual VMs in Orchard

## Prerequisites

- **Orchard cluster** -- a controller (runs on Linux/K8s) and one or more workers (run on macOS Apple Silicon). See [Orchard quick start](https://tart.run/orchard/quick-start/).
- **Tart VM images** -- OCI images with macOS + Xcode pre-installed. Cirrus Labs provides these at `ghcr.io/cirruslabs/macos-*-xcode`. See [available images](https://github.com/cirruslabs/macos-image-templates).
- **GitHub App** (recommended) with `Self-hosted runners: Read & Write` permission at the organization level, or a **Personal Access Token** with `admin:org` scope.
- **Kubernetes cluster** for production, or **Go 1.25+** for local development.

### Worker setup

Each Mac Mini runs an Orchard worker. Set the `tart-vms` resource to control how many concurrent VMs it can host:

```bash
# Mac with plenty of RAM -- run 2 VMs
orchard worker run --resources org.cirruslabs.tart-vms:2

# Mac with less RAM -- run 1 VM
orchard worker run --resources org.cirruslabs.tart-vms:1
```

The bridge auto-detects total capacity across all connected workers. No config changes needed when adding or removing Macs.

## Install

### From the Helm chart (recommended)

```bash
# Install from OCI registry
helm install orchard-gh-bridge \
  oci://ghcr.io/breakawaydata/charts/orchard-gh-bridge \
  --version 0.1.0 \
  -f values.yaml
```

### Using a pull-through cache or private mirror

Override the image registry so the container is pulled from your cache instead of ghcr.io directly:

```bash
helm install orchard-gh-bridge \
  oci://ghcr.io/breakawaydata/charts/orchard-gh-bridge \
  --set image.registry=my-cache.internal
```

This produces an image reference like `my-cache.internal/breakawaydata/orchard-gh-bridge:0.1.0`.

### From source

```bash
git clone https://github.com/breakawaydata/orchard-gh-bridge.git
cd orchard-gh-bridge
helm install orchard-gh-bridge ./charts/orchard-gh-bridge -f values.yaml
```

## Configuration

### Secrets

The bridge needs credentials for GitHub and Orchard. Three patterns are supported:

**Option 1: Pre-created Kubernetes Secret** (simplest)

```bash
# For GitHub App auth
kubectl create secret generic orchard-gh-bridge-secrets \
  --from-literal=ORCHARD_GH_BRIDGE_GITHUB_PRIVATE_KEY="$(cat private-key.pem)" \
  --from-literal=ORCHARD_GH_BRIDGE_ORCHARD_PASSWORD=your-orchard-token

# For PAT auth
kubectl create secret generic orchard-gh-bridge-secrets \
  --from-literal=ORCHARD_GH_BRIDGE_GITHUB_TOKEN=ghp_your_token \
  --from-literal=ORCHARD_GH_BRIDGE_ORCHARD_PASSWORD=your-orchard-token
```

Then reference it:

```yaml
existingSecret: orchard-gh-bridge-secrets
```

**Option 2: External Secrets Operator** (for 1Password, Vault, AWS Secrets Manager, etc.)

```yaml
externalSecret:
  enabled: true
  secretStoreRef:
    name: my-cluster-secret-store
    kind: ClusterSecretStore
  data:
    - secretKey: ORCHARD_GH_BRIDGE_GITHUB_PRIVATE_KEY
      remoteRef:
        key: path/to/github-app
        property: private_key
    - secretKey: ORCHARD_GH_BRIDGE_ORCHARD_PASSWORD
      remoteRef:
        key: path/to/orchard
        property: token
```

**Option 3: Inline via `--set`** (development only)

```bash
helm install orchard-gh-bridge ./charts/orchard-gh-bridge \
  --set config.github.token=ghp_dev_token
```

### Scale sets

Each scale set maps to a `runs-on:` label in your GitHub Actions workflows. Configure them in `values.yaml`:

```yaml
config:
  orchard:
    address: http://orchard-controller:6120
  github:
    appID: 123456
    installationID: 789
  scaleSets:
    - name: macos-sequoia-xcode-16
      githubConfigURL: https://github.com/your-org
      labels: [self-hosted, macOS, ARM64, xcode-16]
      vm:
        image: ghcr.io/cirruslabs/macos-sequoia-xcode:16
        cpu: 4
        memory: 8192

    - name: macos-tahoe-xcode-26.4
      githubConfigURL: https://github.com/your-org
      labels: [self-hosted, macOS, ARM64, tahoe, xcode-26.4]
      vm:
        image: ghcr.io/cirruslabs/macos-tahoe-xcode:26.4
        cpu: 4
        memory: 8192
```

Then in your workflow:

```yaml
jobs:
  build:
    runs-on: macos-sequoia-xcode-16
    steps:
      - uses: actions/checkout@v4
      - run: xcodebuild -version
      - run: docker compose up -d  # Docker is available!
```

### Full values reference

See [charts/orchard-gh-bridge/values.yaml](charts/orchard-gh-bridge/values.yaml) for all available options including:

| Section | Key options |
|---------|------------|
| `image` | `registry`, `repository`, `tag`, `pullPolicy` |
| `config.orchard` | `address`, `username` |
| `config.github` | `appID`, `installationID`, `privateKeyPath`, `token` |
| `config.scaleSets[]` | `name`, `githubConfigURL`, `labels`, `maxRunners`, `vm.image`, `vm.cpu`, `vm.memory`, `vm.nested`, `vm.labels` |
| `config.maxVMs` | Global VM capacity cap (0 = auto-detect from workers) |
| `config.maxVMAge` | VM reaping safety timeout as a Go duration, e.g. `4h` (empty = 2h default). Set above the longest consuming job's `timeout-minutes` so the job timeout governs and this stays a backstop. |
| `existingSecret` | Name of a pre-created K8s Secret |
| `externalSecret` | External Secrets Operator config |
| `metrics` | Prometheus ServiceMonitor config |
| `resources` | CPU/memory requests and limits |

## Local development

```bash
# Start a local Orchard dev cluster (macOS only, runs controller + worker)
orchard dev

# Create a config file
cat > config.yaml <<'EOF'
logLevel: debug
orchard:
  address: http://127.0.0.1:6120
github:
  token: ghp_your_pat_here
scaleSets:
  - name: macos-runner
    githubConfigURL: https://github.com/your-org
    labels: [self-hosted, macOS, ARM64]
    vm:
      image: ghcr.io/cirruslabs/macos-sequoia-xcode:latest
      cpu: 4
      memory: 8192
EOF

# Build and run
go run . -config config.yaml
```

### Running tests

```bash
# Unit tests
make test

# Lint
make lint

# Helm chart validation
make helm-lint

# Integration tests (requires a running Orchard controller)
docker run -d --name orchard-test -p 6120:6120 \
  -e ORCHARD_BOOTSTRAP_ADMIN_TOKEN=test-token \
  ghcr.io/cirruslabs/orchard:latest \
  controller run --listen :6120 --insecure-no-tls

ORCHARD_ADDRESS=http://localhost:6120 \
ORCHARD_PASSWORD=test-token \
go test -v -race -tags=integration ./...

docker stop orchard-test && docker rm orchard-test
```

### Building the Docker image

```bash
make docker           # builds ghcr.io/breakawaydata/orchard-gh-bridge:dev
make docker TAG=v0.1.0  # builds with a specific tag
```

## Monitoring

The bridge exposes health endpoints:

- `GET /healthz` -- liveness probe (always returns 200)
- `GET /readyz` -- readiness probe (checks Orchard controller connectivity)

Monitor VMs from the command line:

```bash
# Watch VM states
watch orchard list vms

# Check a specific VM
orchard get vm gha-orchard-macos-tahoe-xcode-26-4-a1b2c3d4

# List workers and their capacity
orchard list workers
```

## CI/CD

The project includes GitHub Actions workflows:

- **CI** (`.github/workflows/ci.yaml`) -- runs on every PR and push to `main`:
  - Go build, vet, and test (with race detector)
  - golangci-lint
  - Helm chart lint and template validation
  - Docker image build (no push)
  - Integration tests against a real Orchard controller container

- **Release** (`.github/workflows/release.yaml`) -- triggered by `v*` tags:
  - Builds and pushes multi-arch Docker image to `ghcr.io/breakawaydata/orchard-gh-bridge`
  - Packages and pushes the Helm chart to `oci://ghcr.io/breakawaydata/charts/orchard-gh-bridge`

To cut a release:

```bash
git tag v0.1.0
git push origin v0.1.0
```

## Project structure

```
orchard-gh-bridge/
+-- main.go                  # Entrypoint: config -> clients -> manager -> health -> signal handling
+-- config/                  # YAML config loading, env var overrides, validation
+-- orchard/                 # Orchard client wrapper (official library + type conversions)
|   +-- client.go            # Client interface + officialClient implementation
|   +-- types.go             # Bridge-local VM/Worker/VMScript types
+-- bridge/                  # Core logic
|   +-- bridge.go            # scaleset.Scaler implementation (per scale set)
|   +-- capacity.go          # Global VM capacity semaphore with dynamic max
|   +-- cleanup.go           # Background goroutine: VM reaping, runner deregistration, capacity refresh
|   +-- vm.go                # VM naming (gha-orchard- prefix), startup script generation
|   +-- runner_remover.go    # GitHub runner deregistration via scaleset client
+-- manager/                 # Multi-scale-set orchestration, listener lifecycle, capacity auto-detection
+-- health/                  # /healthz and /readyz HTTP endpoints
+-- charts/orchard-gh-bridge/  # Helm chart
+-- .github/workflows/       # CI and release pipelines
```

## Troubleshooting

### VMs stuck in `pending` with no assigned worker

The Orchard scheduler matches VMs to workers based on architecture, runtime, resources, and labels. Check:

1. **Worker is online:** `orchard list workers` -- `Last seen` should be recent, `Scheduling paused` should be `false`
2. **Worker has capacity:** Check `org.cirruslabs.tart-vms` resource -- must have available slots
3. **Architecture/runtime match:** Worker must report `arch: arm64` and `runtime: tart`
4. **VM labels:** VM labels act as worker-affinity constraints. The bridge intentionally sets no labels to avoid this issue.

### Nested virtualization error on M1/M2

If you see `"Error: Nested virtualization is available for Mac with the M3 chip, and later."`, the VM has `nested: true` but is scheduled on an M1/M2 worker. Either:
- Set `nested: false` for that scale set
- Add a `nested-virt: "true"` label to the VM and only label M3+ workers with it

### Runner version deprecated

The startup script auto-downloads the latest runner from GitHub's releases API. If this fails (e.g., network issues in the VM), check the VM logs:

```bash
orchard logs vm <vm-name>
```

### Stale offline runners in GitHub

The bridge deregisters runners on both normal job completion and cleanup sweep. If stale runners accumulate, they can be manually cleaned:

```bash
# List offline runners
gh api /orgs/YOUR-ORG/actions/runners --paginate | jq '.runners[] | select(.status=="offline" and (.name | startswith("gha-orchard-"))) | .id'

# Remove a specific runner
gh api -X DELETE /orgs/YOUR-ORG/actions/runners/RUNNER_ID
```

### Bridge restart / session conflict

If you see `409 Conflict: already has an active session`, the previous bridge instance's session hasn't expired yet. This resolves automatically within seconds. The bridge will retry on restart.

## License

Apache-2.0 -- see [LICENSE](LICENSE).
