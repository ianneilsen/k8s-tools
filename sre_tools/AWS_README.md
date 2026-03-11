# eks-diag — EKS Cluster Diagnostic Scanner

Fast, parallel cluster health scanner with AWS CloudWatch log cross-referencing.
Designed for sysadmins who need answers in seconds, not minutes.

## What it checks

### Kubernetes Resources (all parallel)
| Check | What it catches |
|-------|----------------|
| **Nodes** | NotReady, MemoryPressure, DiskPressure, PIDPressure, NetworkUnavailable, low allocatable capacity |
| **Pods** | CrashLoopBackOff, OOMKilled, ImagePullBackOff, stuck Pending, Failed/Unknown phase, high restart counts |
| **Deployments** | Zero-ready outages, partial availability, stalled rollouts (ProgressDeadlineExceeded) |
| **DaemonSets** | Under-scheduled, unavailable, mis-scheduled pods |
| **StatefulSets** | Partial readiness, stuck updates (revision mismatch) |
| **Services** | Empty endpoints with selectors, LoadBalancer without external IP |
| **PVCs** | Pending claims, lost claims |
| **Events** | Aggregated Warning events (OOMKilling, Evicted, FailedMount, etc.) |
| **ResourceQuotas** | Quotas at ≥90% utilisation |

### AWS EKS / CloudWatch (parallel with k8s checks)
| Check | What it catches |
|-------|----------------|
| **Cluster Health** | Non-ACTIVE status, EKS-reported health issues, disabled control plane log types |
| **Control Plane Logs** | Errors, OOMKill, crash loops, scheduling failures, cert/TLS issues, connectivity problems |
| **Application Logs** | Same pattern set via Container Insights |
| **Host Logs** | Node-level errors and connectivity |
| **Data Plane Logs** | Kubelet/kube-proxy level issues |
| **Performance Logs** | Resource saturation signals |

## Speed design

- K8s client QPS cranked to 100 / burst 200 (default is 5/10)
- All checks fire concurrently via goroutines
- AWS and K8s clients initialise in parallel
- 45-second overall timeout (tuneable)
- Event deduplication to avoid flooding output
- CloudWatch queries use `Limit: 5` — just enough to confirm a problem exists

## Usage

```bash
# Basic — auto-detects cluster from kubeconfig + first EKS cluster in region
go run main.go

# Specific cluster
EKSDIAG_CLUSTER_NAME=my-prod-cluster go run main.go

# JSON output (pipe to jq, feed to Splunk, etc.)
EKSDIAG_OUTPUT=json go run main.go | jq .

# Tune thresholds
EKSDIAG_TIMEOUT=30 \
EKSDIAG_LOG_LOOKBACK_MIN=30 \
EKSDIAG_PENDING_THRESH_SEC=60 \
EKSDIAG_RESTART_THRESH=5 \
  go run main.go
```

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `EKSDIAG_CLUSTER_NAME` | auto-detect | EKS cluster name for CloudWatch log queries |
| `EKSDIAG_TIMEOUT` | `45` | Overall scan timeout in seconds |
| `EKSDIAG_EVENT_WINDOW_SEC` | `300` | How far back to look for Warning events (seconds) |
| `EKSDIAG_LOG_LOOKBACK_MIN` | `10` | CloudWatch log lookback window (minutes) |
| `EKSDIAG_PENDING_THRESH_SEC` | `120` | Seconds before a Pending pod is flagged |
| `EKSDIAG_RESTART_THRESH` | `3` | Container restart count threshold |
| `EKSDIAG_OUTPUT` | `human` | Output format: `human` or `json` |
| `AWS_REGION` | SDK default | AWS region override |
| `KUBECONFIG` | `~/.kube/config` | Kubeconfig path override |

## Exit codes

| Code | Meaning |
|------|---------|
| `0` | No critical issues |
| `1` | Client init failure |
| `2` | Critical issues detected |

## Build

```bash
go build -o eks-diag .
```

## Integration ideas

- Run as a CronJob in-cluster posting results to Slack/PagerDuty
- Pipe JSON output into your Splunk HEC or Grafana Loki
- Wrap in a systemd timer on a bastion host for periodic sweeps
- Chain with your AI Debug Layer for automated root cause analysis
