# gke-diag -- GKE Cluster Diagnostic Scanner

Fast, parallel cluster health scanner with Google Cloud Logging cross-referencing.
Designed for sysadmins who need answers in seconds, not minutes.

GKE counterpart to [eks-diag](../eks-diag/) -- identical architecture and principles.

## What it checks

### Kubernetes Resources (all parallel)
| Check | What it catches |
|-------|----------------|
| **Nodes** | NotReady, MemoryPressure, DiskPressure, PIDPressure, NetworkUnavailable, low allocatable capacity, deprecated Docker runtime |
| **Pods** | CrashLoopBackOff, OOMKilled, ImagePullBackOff, stuck Pending, Failed/Unknown phase, high restart counts |
| **Deployments** | Zero-ready outages, partial availability, stalled rollouts (ProgressDeadlineExceeded) |
| **DaemonSets** | Under-scheduled, unavailable, mis-scheduled pods |
| **StatefulSets** | Partial readiness, stuck updates (revision mismatch) |
| **Services** | Empty endpoints with selectors, LoadBalancer without external IP |
| **PVCs** | Pending claims, lost claims |
| **Events** | Aggregated Warning events (OOMKilling, Evicted, FailedMount, etc.) |
| **ResourceQuotas** | Quotas at >=90% utilisation |

### GKE Cluster Health (parallel with k8s checks)
| Check | What it catches |
|-------|----------------|
| **Cluster Status** | ERROR, DEGRADED, STOPPING, PROVISIONING, RECONCILING states |
| **Cluster Conditions** | GKE-reported health issues with gRPC error codes |
| **Logging/Monitoring** | Disabled cluster logging or monitoring services |
| **Security Config** | Workload Identity, Binary Authorization, Network Policy, Shielded Nodes, Master Authorized Networks |
| **Node Pools** | Pool status (ERROR, RUNNING_WITH_ERROR, STOPPING), conditions, autoscaling config, upgrade settings, shielded instance config |

### Google Cloud Logging (parallel with all other checks)
| Check | What it catches |
|-------|----------------|
| **Container Logs** | severity>=ERROR from k8s_container resources |
| **Node Logs** | severity>=ERROR from k8s_node resources |
| **Cluster Logs** | severity>=WARNING from k8s_cluster resources |
| **OOMKill** | OOMKilled pattern matches in container logs |
| **Crash Loops** | CrashLoopBackOff pattern matches |
| **Image Pull** | ImagePullBackOff, ErrImagePull, Failed to pull image |
| **Scheduling** | FailedScheduling, Insufficient, Unschedulable |
| **TLS/Cert** | x509, certificate, TLS handshake errors |
| **Connectivity** | connection refused, context deadline exceeded, i/o timeout |
| **GKE Infra** | severity>=WARNING from gke_cluster resources |

## Speed design

- K8s client QPS cranked to 100 / burst 200 (default is 5/10)
- All checks fire concurrently via goroutines
- GCP and K8s clients initialise in parallel (3 concurrent inits)
- 45-second overall timeout (tuneable)
- Event deduplication to avoid flooding output
- Cloud Logging queries use PageSize=5 -- just enough to confirm a problem exists
- GKE and logging clients are non-fatal -- k8s checks always run

## Usage

```bash
# Basic -- auto-detects cluster from kubeconfig + first GKE cluster in project
GKEDIAG_PROJECT_ID=my-project go run main.go

# Specific cluster
GKEDIAG_PROJECT_ID=my-project \
GKEDIAG_CLUSTER_NAME=prod-cluster \
GKEDIAG_CLUSTER_LOCATION=australia-southeast1 \
  go run main.go

# JSON output (pipe to jq, feed to BigQuery, etc.)
GKEDIAG_OUTPUT=json go run main.go | jq .

# Tune thresholds
GKEDIAG_TIMEOUT=30 \
GKEDIAG_LOG_LOOKBACK_MIN=30 \
GKEDIAG_PENDING_THRESH_SEC=60 \
GKEDIAG_RESTART_THRESH=5 \
  go run main.go
```

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `GKEDIAG_PROJECT_ID` | `$GOOGLE_CLOUD_PROJECT` / `$GCLOUD_PROJECT` / `$CLOUDSDK_CORE_PROJECT` | GCP project ID |
| `GKEDIAG_CLUSTER_NAME` | auto-detect | GKE cluster name |
| `GKEDIAG_CLUSTER_LOCATION` | auto-detect | Cluster zone or region |
| `GKEDIAG_TIMEOUT` | `45` | Overall scan timeout in seconds |
| `GKEDIAG_EVENT_WINDOW_SEC` | `300` | How far back to look for Warning events (seconds) |
| `GKEDIAG_LOG_LOOKBACK_MIN` | `10` | Cloud Logging lookback window (minutes) |
| `GKEDIAG_PENDING_THRESH_SEC` | `120` | Seconds before a Pending pod is flagged |
| `GKEDIAG_RESTART_THRESH` | `3` | Container restart count threshold |
| `GKEDIAG_OUTPUT` | `human` | Output format: `human` or `json` |
| `KUBECONFIG` | `~/.kube/config` | Kubeconfig path override |
| `NO_COLOR` | unset | Set to disable ANSI colours |

## Authentication

Uses Google Application Default Credentials (ADC). Authenticate via any of:
- `gcloud auth application-default login` (local dev)
- Workload Identity (in-cluster on GKE)
- Service account key file via `GOOGLE_APPLICATION_CREDENTIALS`
- Attached service account on GCE/Cloud Run

Required IAM roles:
- `roles/container.viewer` (GKE cluster/node pool inspection)
- `roles/logging.viewer` (Cloud Logging queries)

## Exit codes

| Code | Meaning |
|------|---------|
| `0` | No critical issues |
| `1` | Client init failure |
| `2` | Critical issues detected |

## Build

```bash
go build -o gke-diag .
```

## Integration ideas

- Run as a CronJob in-cluster posting results to Slack/PagerDuty
- Pipe JSON output into BigQuery or your Splunk HEC
- Wrap in a Cloud Scheduler job hitting a Cloud Run instance
- Chain with your AI Debug Layer for automated root cause analysis
