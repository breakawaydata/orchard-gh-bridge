# orchard-gh-bridge

Autoscaling GitHub Actions runners on ephemeral macOS VMs, orchestrated by [Orchard](https://tart.run/orchard/)/[Tart](https://tart.run) on Apple Silicon.

## Overview

`orchard-gh-bridge` is a lightweight Go service that connects GitHub Actions to Orchard, an orchestrator for [Tart](https://tart.run) macOS virtual machines running on Apple Silicon hardware.

It uses the [GitHub Actions Runner Scale Set API](https://github.com/actions/scaleset) to listen for CI job demand, then provisions ephemeral macOS VMs via Orchard's REST API. Each job gets a clean, isolated VM that is automatically destroyed after the job completes.

**Key features:**

- **Ephemeral VMs** — every CI job runs in a fresh macOS VM. No shared state between jobs.
- **Autoscaling** — VMs are created on demand and destroyed when jobs finish. No idle runners.
- **Multiple scale sets** — run different Xcode/macOS versions side by side (e.g. `runs-on: macos-sequoia-xcode-16`).
- **Global capacity management** — a shared semaphore prevents over-provisioning across all scale sets, respecting the macOS 2-VM-per-host EULA limit.
- **Helm chart** — deploy to any Kubernetes cluster. Published as an OCI artifact for `helm install`.
- **Pull-through cache support** — configurable image registry for air-gapped or cached environments.

## Architecture

```
Kubernetes cluster
┌──────────────────────────────────────────────────────────┐
│                                                          │
│  orchard-gh-bridge (Deployment, 1 replica)               │
│    ├── listener goroutine per scale set (long-poll)      │
│    │     └── implements scaleset.Scaler interface        │
│    ├── cleanup goroutine (reaps stopped/failed VMs)      │
│    └── health server (/healthz, /readyz)                 │
│                                                          │
│  Orchard controller (StatefulSet)                        │
│    └── REST API: VM create/delete, worker listing        │
│                                                          │
└──────────┬───────────────────────────────┬───────────────┘
           │                               │
           │ GitHub Scale Set API          │ Orchard worker protocol
           │ (HTTPS, long-poll)            │ (outbound from workers)
           ▼                               ▼
    GitHub Actions                 Mac Minis (Apple Silicon)
    ┌────────────┐                 ├── Worker 1
    │ Job queue  │                 │   ├── Tart VM (job A)
    │ Scale sets │                 │   └── Tart VM (job B)
    └────────────┘                 └── Worker 2
                                       ├── Tart VM (job C)
                                       └── Tart VM (job D)
```

### How it works

1. The bridge registers one or more **runner scale sets** with GitHub Actions.
2. For each scale set, a **listener goroutine** long-polls GitHub for job demand.
3. When GitHub assigns jobs, the bridge generates a **JIT runner config** and creates an **Orchard VM** with a startup script that boots the GitHub Actions runner.
4. The runner inside the VM picks up the job and executes it.
5. When the job completes, the bridge **deletes the VM** and releases the capacity slot.
6. A background **cleanup goroutine** reaps any VMs that are stopped, failed, or exceed a 2-hour safety timeout.

## Prerequisites

- **Orchard cluster** — a controller (runs on Linux/K8s) and one or more workers (run on macOS Apple Silicon). See [Orchard quick start](https://tart.run/orchard/quick-start/).
- **Tart VM images** — OCI images with macOS + Xcode pre-installed. Cirrus Labs provides these at `ghcr.io/cirruslabs/macos-*-xcode`. See [available images](https://github.com/cirruslabs/macos-image-templates).
- **GitHub App** (recommended) with `Self-hosted runners: Read & Write` permission at the organization level, or a **Personal Access Token** with `admin:org` scope.
- **Kubernetes cluster** for production, or **Go 1.25+** for local development.

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
  maxVMs: 4  # global cap across all scale sets (respects macOS 2-VM-per-host limit)
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

    - name: macos-sequoia-xcode-16.3
      githubConfigURL: https://github.com/your-org
      labels: [self-hosted, macOS, ARM64, xcode-16.3]
      vm:
        image: ghcr.io/cirruslabs/macos-sequoia-xcode:16.3
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
```

### Full values reference

See [charts/orchard-gh-bridge/values.yaml](charts/orchard-gh-bridge/values.yaml) for all available options including:

| Section | Key options |
|---------|------------|
| `image` | `registry`, `repository`, `tag`, `pullPolicy` |
| `config.orchard` | `address`, `username`, `insecure` |
| `config.github` | `appID`, `installationID`, `privateKeyPath`, `token` |
| `config.scaleSets[]` | `name`, `githubConfigURL`, `labels`, `maxRunners`, `vm.image`, `vm.cpu`, `vm.memory` |
| `config.maxVMs` | Global VM capacity cap |
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
maxVMs: 2
orchard:
  address: http://127.0.0.1:6120
  insecure: true
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

## CI/CD

The project includes GitHub Actions workflows:

- **CI** (`.github/workflows/ci.yaml`) — runs on every PR and push to `main`:
  - Go build, vet, and test (with race detector)
  - golangci-lint
  - Helm chart lint and template validation
  - Docker image build (no push)
  - Integration tests against a real Orchard controller container

- **Release** (`.github/workflows/release.yaml`) — triggered by `v*` tags:
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
├── main.go                  # Entrypoint: config → clients → manager → health → signal handling
├── config/                  # YAML config loading, env var overrides, validation
├── orchard/                 # Thin HTTP client for Orchard REST API (VM + worker CRUD)
├── bridge/                  # Core logic
│   ├── bridge.go            # scaleset.Scaler implementation (per scale set)
│   ├── capacity.go          # Global VM capacity semaphore
│   ├── cleanup.go           # Background goroutine reaping stale VMs
│   └── vm.go                # VM naming, startup script generation
├── manager/                 # Multi-scale-set orchestration, listener lifecycle
├── health/                  # /healthz and /readyz HTTP endpoints
├── charts/orchard-gh-bridge/  # Helm chart
└── .github/workflows/       # CI and release pipelines
```

## License

Apache-2.0 — see [LICENSE](LICENSE).
