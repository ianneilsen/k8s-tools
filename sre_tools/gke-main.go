package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	container "cloud.google.com/go/container/apiv1"
	"cloud.google.com/go/container/apiv1/containerpb"
	logging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// ---------------------------------------------------------------------------
// ANSI colour codes -- disabled when NO_COLOR is set or stdout is not a tty
// ---------------------------------------------------------------------------

var (
	colReset   = "\033[0m"
	colRed     = "\033[1;31m"
	colYellow  = "\033[1;33m"
	colGreen   = "\033[1;32m"
	colCyan    = "\033[1;36m"
	colBlue    = "\033[1;34m"
	colMagenta = "\033[1;35m"
	colWhite   = "\033[1;37m"
	colDim     = "\033[2m"
	colBold    = "\033[1m"
)

func initColours() {
	// Respect NO_COLOR convention (https://no-color.org)
	if _, set := os.LookupEnv("NO_COLOR"); set {
		disableColours()
		return
	}
	if os.Getenv("TERM") == "dumb" {
		disableColours()
		return
	}
	fi, err := os.Stdout.Stat()
	if err != nil || (fi.Mode()&os.ModeCharDevice) == 0 {
		disableColours()
	}
}

func disableColours() {
	colReset = ""
	colRed = ""
	colYellow = ""
	colGreen = ""
	colCyan = ""
	colBlue = ""
	colMagenta = ""
	colWhite = ""
	colDim = ""
	colBold = ""
}

// ---------------------------------------------------------------------------
// Severity levels -- ordered so sorting gives you critical -> info
// ---------------------------------------------------------------------------

type Severity int

const (
	SevCritical Severity = iota
	SevWarning
	SevInfo
)

func (s Severity) String() string {
	switch s {
	case SevCritical:
		return "CRITICAL"
	case SevWarning:
		return "WARNING"
	case SevInfo:
		return "INFO"
	default:
		return "UNKNOWN"
	}
}

func (s Severity) Coloured() string {
	switch s {
	case SevCritical:
		return colRed + "CRIT" + colReset
	case SevWarning:
		return colYellow + "WARN" + colReset
	case SevInfo:
		return colCyan + "INFO" + colReset
	default:
		return "UNKN"
	}
}

// ---------------------------------------------------------------------------
// Issue -- every diagnostic finding is one of these
// ---------------------------------------------------------------------------

type Issue struct {
	Severity  Severity
	Category  string // e.g. "Node", "Pod", "Deployment", "GKE/Logs"
	Resource  string // namespace/name or node name
	Message   string
	Timestamp time.Time
}

// ---------------------------------------------------------------------------
// Config -- tuneable knobs, overridable via env vars
// ---------------------------------------------------------------------------

type Config struct {
	TimeoutSeconds  int
	EventWindow     time.Duration
	LogLookbackMins int
	PendingThresh   time.Duration
	RestartThresh   int32
	ProjectID       string // GCP project ID (required for logging)
	ClusterName     string // auto-detected if empty
	ClusterLocation string // zone or region, auto-detected if empty
}

func loadConfig() Config {
	cfg := Config{
		TimeoutSeconds:  envInt("GKEDIAG_TIMEOUT", 45),
		EventWindow:     time.Duration(envInt("GKEDIAG_EVENT_WINDOW_SEC", 300)) * time.Second,
		LogLookbackMins: envInt("GKEDIAG_LOG_LOOKBACK_MIN", 10),
		PendingThresh:   time.Duration(envInt("GKEDIAG_PENDING_THRESH_SEC", 120)) * time.Second,
		RestartThresh:   int32(envInt("GKEDIAG_RESTART_THRESH", 3)),
		ProjectID:       os.Getenv("GKEDIAG_PROJECT_ID"),
		ClusterName:     os.Getenv("GKEDIAG_CLUSTER_NAME"),
		ClusterLocation: os.Getenv("GKEDIAG_CLUSTER_LOCATION"),
	}
	// Fall back to standard GCP env vars
	if cfg.ProjectID == "" {
		cfg.ProjectID = os.Getenv("GOOGLE_CLOUD_PROJECT")
	}
	if cfg.ProjectID == "" {
		cfg.ProjectID = os.Getenv("GCLOUD_PROJECT")
	}
	if cfg.ProjectID == "" {
		cfg.ProjectID = os.Getenv("CLOUDSDK_CORE_PROJECT")
	}
	return cfg
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	var n int
	_, err := fmt.Sscanf(v, "%d", &n)
	if err != nil {
		return fallback
	}
	return n
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	initColours()
	cfg := loadConfig()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()

	started := time.Now()

	// ------------------------------------------------------------------
	// Build clients concurrently -- k8s + GCP at the same time
	// ------------------------------------------------------------------
	var (
		clientset     *kubernetes.Clientset
		gkeClient     *container.ClusterManagerClient
		loggingClient *logging.Client
		k8sErr        error
		gkeErr        error
		logErr        error
		wgInit        sync.WaitGroup
	)

	wgInit.Add(3)
	go func() {
		defer wgInit.Done()
		clientset, k8sErr = getClientset()
	}()
	go func() {
		defer wgInit.Done()
		gkeClient, gkeErr = container.NewClusterManagerClient(ctx, option.WithTelemetryDisabled())
	}()
	go func() {
		defer wgInit.Done()
		loggingClient, logErr = logging.NewClient(ctx, option.WithTelemetryDisabled())
	}()
	wgInit.Wait()

	if k8sErr != nil {
		fmt.Fprintf(os.Stderr, "%sFATAL%s Kubernetes client init failed: %v\n", colRed, colReset, k8sErr)
		os.Exit(1)
	}

	// GKE client is non-fatal -- we just skip cluster-level checks
	if gkeErr != nil {
		fmt.Fprintf(os.Stderr, "%sWARN%s  GKE client init failed (%v) -- skipping GKE cluster health checks\n", colYellow, colReset, gkeErr)
	} else {
		defer gkeClient.Close()
	}

	// Logging client is non-fatal -- we just skip log checks
	if logErr != nil {
		fmt.Fprintf(os.Stderr, "%sWARN%s  Cloud Logging client init failed (%v) -- skipping log checks\n", colYellow, colReset, logErr)
	} else {
		defer loggingClient.Close()
	}

	// Auto-detect cluster name/location/project if not provided
	if gkeErr == nil && (cfg.ClusterName == "" || cfg.ClusterLocation == "" || cfg.ProjectID == "") {
		detectClusterInfo(ctx, gkeClient, &cfg)
	}

	// ------------------------------------------------------------------
	// Fan-out all checks in parallel
	// ------------------------------------------------------------------
	issues := make(chan Issue, 256)
	var wg sync.WaitGroup

	// Kubernetes resource checks -- all fire concurrently
	k8sChecks := []struct {
		name string
		fn   func(context.Context, *kubernetes.Clientset, Config, chan<- Issue)
	}{
		{"Nodes", checkNodes},
		{"Pods", checkPods},
		{"Deployments", checkDeployments},
		{"DaemonSets", checkDaemonSets},
		{"StatefulSets", checkStatefulSets},
		{"Services", checkServices},
		{"PVCs", checkPVCs},
		{"Events", checkEvents},
		{"ResourceQuotas", checkResourceQuotas},
	}

	for _, c := range k8sChecks {
		wg.Add(1)
		go func(name string, fn func(context.Context, *kubernetes.Clientset, Config, chan<- Issue)) {
			defer wg.Done()
			fn(ctx, clientset, cfg, issues)
		}(c.name, c.fn)
	}

	// GKE cluster-level health check
	if gkeErr == nil && cfg.ProjectID != "" && cfg.ClusterName != "" && cfg.ClusterLocation != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			checkGKEClusterHealth(ctx, gkeClient, cfg, issues)
		}()

		// GKE node pool checks
		wg.Add(1)
		go func() {
			defer wg.Done()
			checkGKENodePools(ctx, gkeClient, cfg, issues)
		}()
	}

	// Cloud Logging checks
	if logErr == nil && cfg.ProjectID != "" && cfg.ClusterName != "" {
		logFilters := []struct {
			filter   string
			category string
			desc     string
		}{
			{
				filter:   `resource.type="k8s_container" severity>=ERROR`,
				category: "GKE/ContainerLogs",
				desc:     "container errors",
			},
			{
				filter:   `resource.type="k8s_node" severity>=ERROR`,
				category: "GKE/NodeLogs",
				desc:     "node-level errors",
			},
			{
				filter:   `resource.type="k8s_cluster" severity>=WARNING`,
				category: "GKE/ClusterLogs",
				desc:     "cluster control plane warnings",
			},
			{
				filter:   `resource.type="k8s_container" jsonPayload.message=~"OOMKilled|OOMKill|oom-kill"`,
				category: "GKE/OOMKill",
				desc:     "OOMKill events",
			},
			{
				filter:   `resource.type="k8s_container" jsonPayload.message=~"CrashLoopBackOff"`,
				category: "GKE/CrashLoop",
				desc:     "crash loops",
			},
			{
				filter:   `resource.type="k8s_container" jsonPayload.message=~"ImagePullBackOff|ErrImagePull|Failed to pull image"`,
				category: "GKE/ImagePull",
				desc:     "image pull failures",
			},
			{
				filter:   `resource.type="k8s_container" jsonPayload.message=~"FailedScheduling|Insufficient|Unschedulable"`,
				category: "GKE/Scheduling",
				desc:     "scheduling failures",
			},
			{
				filter:   `resource.type="k8s_container" jsonPayload.message=~"x509|certificate|TLS handshake"`,
				category: "GKE/TLS",
				desc:     "certificate/TLS errors",
			},
			{
				filter:   `resource.type="k8s_container" jsonPayload.message=~"connection refused|context deadline exceeded|i/o timeout"`,
				category: "GKE/Connectivity",
				desc:     "connectivity issues",
			},
			{
				filter:   `resource.type="gke_cluster" severity>=WARNING`,
				category: "GKE/InfraLogs",
				desc:     "GKE infrastructure warnings",
			},
		}

		for _, lf := range logFilters {
			wg.Add(1)
			go func(filter, category, desc string) {
				defer wg.Done()
				checkCloudLogs(ctx, loggingClient, cfg, filter, category, desc, issues)
			}(lf.filter, lf.category, lf.desc)
		}
	}

	// Drain channel once all goroutines finish
	go func() {
		wg.Wait()
		close(issues)
	}()

	// Collect results
	var allIssues []Issue
	for issue := range issues {
		allIssues = append(allIssues, issue)
	}

	elapsed := time.Since(started)

	// ------------------------------------------------------------------
	// Output
	// ------------------------------------------------------------------
	outputFormat := os.Getenv("GKEDIAG_OUTPUT")
	switch strings.ToLower(outputFormat) {
	case "json":
		printJSON(allIssues, elapsed, cfg)
	default:
		printHuman(allIssues, elapsed, cfg)
	}

	if countBySev(allIssues, SevCritical) > 0 {
		os.Exit(2)
	}
}

// ---------------------------------------------------------------------------
// Output formatters
// ---------------------------------------------------------------------------

func printHuman(issues []Issue, elapsed time.Duration, cfg Config) {
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Severity != issues[j].Severity {
			return issues[i].Severity < issues[j].Severity
		}
		return issues[i].Category < issues[j].Category
	})

	fmt.Printf("\n%s=== GKE Cluster Diagnostic Report ===%s\n", colBold, colReset)
	if cfg.ProjectID != "" {
		fmt.Printf("  Project  : %s%s%s\n", colCyan, cfg.ProjectID, colReset)
	}
	if cfg.ClusterName != "" {
		fmt.Printf("  Cluster  : %s%s%s\n", colCyan, cfg.ClusterName, colReset)
	}
	if cfg.ClusterLocation != "" {
		fmt.Printf("  Location : %s%s%s\n", colCyan, cfg.ClusterLocation, colReset)
	}
	fmt.Printf("  Scanned  : %s\n", time.Now().Format(time.RFC3339))
	fmt.Printf("  Duration : %s%s%s\n\n", colGreen, elapsed.Round(time.Millisecond), colReset)

	crit := countBySev(issues, SevCritical)
	warn := countBySev(issues, SevWarning)
	info := countBySev(issues, SevInfo)

	if len(issues) == 0 {
		fmt.Printf("  %sOK%s  All checks passed -- no issues detected.\n\n", colGreen, colReset)
		return
	}

	fmt.Printf("  Summary: %s%d critical%s  %s%d warning%s  %s%d info%s\n\n",
		colRed, crit, colReset,
		colYellow, warn, colReset,
		colCyan, info, colReset,
	)

	currentCat := ""
	for _, iss := range issues {
		if iss.Category != currentCat {
			currentCat = iss.Category
			fmt.Printf("%s--- %s ---%s\n", colMagenta, currentCat, colReset)
		}
		ts := ""
		if !iss.Timestamp.IsZero() {
			ts = fmt.Sprintf(" %s[%s]%s", colDim, iss.Timestamp.Format("15:04:05"), colReset)
		}
		fmt.Printf("  %s  %-40s %s%s\n",
			iss.Severity.Coloured(),
			iss.Resource,
			iss.Message, ts,
		)
	}
	fmt.Println()
}

type jsonOutput struct {
	Project   string         `json:"project,omitempty"`
	Cluster   string         `json:"cluster,omitempty"`
	Location  string         `json:"location,omitempty"`
	ScannedAt string         `json:"scanned_at"`
	Duration  string         `json:"duration"`
	Summary   map[string]int `json:"summary"`
	Issues    []jsonIssue    `json:"issues"`
}

type jsonIssue struct {
	Severity  string `json:"severity"`
	Category  string `json:"category"`
	Resource  string `json:"resource"`
	Message   string `json:"message"`
	Timestamp string `json:"timestamp,omitempty"`
}

func printJSON(issues []Issue, elapsed time.Duration, cfg Config) {
	out := jsonOutput{
		Project:   cfg.ProjectID,
		Cluster:   cfg.ClusterName,
		Location:  cfg.ClusterLocation,
		ScannedAt: time.Now().Format(time.RFC3339),
		Duration:  elapsed.Round(time.Millisecond).String(),
		Summary: map[string]int{
			"critical": countBySev(issues, SevCritical),
			"warning":  countBySev(issues, SevWarning),
			"info":     countBySev(issues, SevInfo),
		},
	}
	for _, iss := range issues {
		ji := jsonIssue{
			Severity: iss.Severity.String(),
			Category: iss.Category,
			Resource: iss.Resource,
			Message:  iss.Message,
		}
		if !iss.Timestamp.IsZero() {
			ji.Timestamp = iss.Timestamp.Format(time.RFC3339)
		}
		out.Issues = append(out.Issues, ji)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

func countBySev(issues []Issue, sev Severity) int {
	n := 0
	for _, i := range issues {
		if i.Severity == sev {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Kubernetes client bootstrap
// ---------------------------------------------------------------------------

func getClientset() (*kubernetes.Clientset, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			kubeconfig = filepath.Join(os.Getenv("HOME"), ".kube", "config")
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("kubeconfig load failed: %w", err)
		}
	}
	// Increase QPS for faster parallel API calls
	config.QPS = 100
	config.Burst = 200
	return kubernetes.NewForConfig(config)
}

// ---------------------------------------------------------------------------
// GKE cluster auto-detection + health
// ---------------------------------------------------------------------------

func detectClusterInfo(ctx context.Context, client *container.ClusterManagerClient, cfg *Config) {
	if cfg.ProjectID == "" {
		// Cannot detect without a project
		return
	}

	// List all clusters in the project to find one that matches or pick the first
	parent := fmt.Sprintf("projects/%s/locations/-", cfg.ProjectID)
	resp, err := client.ListClusters(ctx, &containerpb.ListClustersRequest{Parent: parent})
	if err != nil || len(resp.Clusters) == 0 {
		return
	}

	// If cluster name already set, find matching location
	if cfg.ClusterName != "" {
		for _, c := range resp.Clusters {
			if c.Name == cfg.ClusterName {
				if cfg.ClusterLocation == "" {
					cfg.ClusterLocation = c.Location
				}
				return
			}
		}
		return
	}

	// Otherwise pick the first cluster
	cfg.ClusterName = resp.Clusters[0].Name
	cfg.ClusterLocation = resp.Clusters[0].Location
}

func clusterResourceName(cfg Config) string {
	return fmt.Sprintf("projects/%s/locations/%s/clusters/%s", cfg.ProjectID, cfg.ClusterLocation, cfg.ClusterName)
}

func checkGKEClusterHealth(ctx context.Context, client *container.ClusterManagerClient, cfg Config, ch chan<- Issue) {
	name := clusterResourceName(cfg)
	cluster, err := client.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		ch <- Issue{Severity: SevWarning, Category: "GKE/Cluster", Resource: cfg.ClusterName, Message: fmt.Sprintf("Failed to describe cluster: %v", err)}
		return
	}

	// Cluster status
	switch cluster.Status {
	case containerpb.Cluster_RUNNING:
		// healthy
	case containerpb.Cluster_PROVISIONING:
		ch <- Issue{Severity: SevInfo, Category: "GKE/Cluster", Resource: cfg.ClusterName, Message: "Cluster is provisioning"}
	case containerpb.Cluster_RECONCILING:
		ch <- Issue{Severity: SevInfo, Category: "GKE/Cluster", Resource: cfg.ClusterName, Message: "Cluster is reconciling (update in progress)"}
	case containerpb.Cluster_STOPPING:
		ch <- Issue{Severity: SevCritical, Category: "GKE/Cluster", Resource: cfg.ClusterName, Message: "Cluster is stopping"}
	case containerpb.Cluster_ERROR:
		ch <- Issue{Severity: SevCritical, Category: "GKE/Cluster", Resource: cfg.ClusterName, Message: fmt.Sprintf("Cluster is in ERROR state: %s", cluster.StatusMessage)}
	case containerpb.Cluster_DEGRADED:
		ch <- Issue{Severity: SevCritical, Category: "GKE/Cluster", Resource: cfg.ClusterName, Message: fmt.Sprintf("Cluster is DEGRADED: %s", cluster.StatusMessage)}
	default:
		if cluster.StatusMessage != "" {
			ch <- Issue{Severity: SevWarning, Category: "GKE/Cluster", Resource: cfg.ClusterName, Message: fmt.Sprintf("Cluster status %s: %s", cluster.Status, cluster.StatusMessage)}
		}
	}

	// Kubernetes version info
	if cluster.CurrentMasterVersion != "" {
		ch <- Issue{
			Severity: SevInfo,
			Category: "GKE/Cluster",
			Resource: cfg.ClusterName,
			Message:  fmt.Sprintf("Master running k8s %s (node default: %s)", cluster.CurrentMasterVersion, cluster.CurrentNodeVersion),
		}
	}

	// Check cluster conditions (GKE-reported issues)
	for _, cond := range cluster.Conditions {
		sev := SevWarning
		if cond.CanonicalCode != 0 { // non-OK gRPC code
			sev = SevCritical
		}
		ch <- Issue{
			Severity: sev,
			Category: "GKE/Cluster",
			Resource: cfg.ClusterName,
			Message:  fmt.Sprintf("Condition: %s (code: %d)", cond.Message, cond.CanonicalCode),
		}
	}

	// Check logging and monitoring config
	if cluster.LoggingService == "none" {
		ch <- Issue{Severity: SevWarning, Category: "GKE/Cluster", Resource: cfg.ClusterName, Message: "Cluster logging is disabled -- enable for observability"}
	}
	if cluster.MonitoringService == "none" {
		ch <- Issue{Severity: SevWarning, Category: "GKE/Cluster", Resource: cfg.ClusterName, Message: "Cluster monitoring is disabled -- enable for observability"}
	}

	// Check if Binary Authorization is configured
	if cluster.BinaryAuthorization == nil || cluster.BinaryAuthorization.EvaluationMode == containerpb.BinaryAuthorization_DISABLED {
		ch <- Issue{Severity: SevInfo, Category: "GKE/Cluster", Resource: cfg.ClusterName, Message: "Binary Authorization is disabled"}
	}

	// Check Workload Identity
	if cluster.WorkloadIdentityConfig == nil || cluster.WorkloadIdentityConfig.WorkloadPool == "" {
		ch <- Issue{Severity: SevInfo, Category: "GKE/Cluster", Resource: cfg.ClusterName, Message: "Workload Identity not configured -- recommended for GCP API access"}
	}

	// Check network policy
	if cluster.NetworkPolicy == nil || !cluster.NetworkPolicy.Enabled {
		ch <- Issue{Severity: SevInfo, Category: "GKE/Cluster", Resource: cfg.ClusterName, Message: "Network policy enforcement is disabled"}
	}

	// Check shielded nodes
	if cluster.ShieldedNodes == nil || !cluster.ShieldedNodes.Enabled {
		ch <- Issue{Severity: SevInfo, Category: "GKE/Cluster", Resource: cfg.ClusterName, Message: "Shielded GKE nodes not enabled"}
	}

	// Master authorized networks
	if cluster.MasterAuthorizedNetworksConfig == nil || !cluster.MasterAuthorizedNetworksConfig.Enabled {
		ch <- Issue{Severity: SevInfo, Category: "GKE/Cluster", Resource: cfg.ClusterName, Message: "Master authorized networks not configured -- control plane is publicly accessible"}
	}
}

func checkGKENodePools(ctx context.Context, client *container.ClusterManagerClient, cfg Config, ch chan<- Issue) {
	name := clusterResourceName(cfg)
	cluster, err := client.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		return // already reported in checkGKEClusterHealth
	}

	for _, np := range cluster.NodePools {
		resource := fmt.Sprintf("%s/%s", cfg.ClusterName, np.Name)

		// Node pool status
		switch np.Status {
		case containerpb.NodePool_RUNNING:
			// healthy
		case containerpb.NodePool_PROVISIONING:
			ch <- Issue{Severity: SevInfo, Category: "GKE/NodePool", Resource: resource, Message: "Node pool is provisioning"}
		case containerpb.NodePool_RECONCILING:
			ch <- Issue{Severity: SevInfo, Category: "GKE/NodePool", Resource: resource, Message: "Node pool is reconciling"}
		case containerpb.NodePool_STOPPING:
			ch <- Issue{Severity: SevCritical, Category: "GKE/NodePool", Resource: resource, Message: "Node pool is stopping"}
		case containerpb.NodePool_ERROR:
			ch <- Issue{Severity: SevCritical, Category: "GKE/NodePool", Resource: resource, Message: fmt.Sprintf("Node pool ERROR: %s", np.StatusMessage)}
		case containerpb.NodePool_RUNNING_WITH_ERROR:
			ch <- Issue{Severity: SevWarning, Category: "GKE/NodePool", Resource: resource, Message: fmt.Sprintf("Node pool running with errors: %s", np.StatusMessage)}
		}

		// Node pool conditions
		for _, cond := range np.Conditions {
			sev := SevWarning
			if cond.CanonicalCode != 0 {
				sev = SevCritical
			}
			ch <- Issue{
				Severity: sev,
				Category: "GKE/NodePool",
				Resource: resource,
				Message:  fmt.Sprintf("Condition: %s (code: %d)", cond.Message, cond.CanonicalCode),
			}
		}

		// Autoscaling info
		if np.Autoscaling != nil && np.Autoscaling.Enabled {
			if np.InitialNodeCount > 0 && np.Autoscaling.MaxNodeCount > 0 {
				// We can't get current count from the GKE API directly,
				// but we can flag if max is reached based on node count
				ch <- Issue{
					Severity: SevInfo,
					Category: "GKE/NodePool",
					Resource: resource,
					Message:  fmt.Sprintf("Autoscaling enabled: min=%d max=%d", np.Autoscaling.MinNodeCount, np.Autoscaling.MaxNodeCount),
				}
			}
		}

		// Check node pool upgrade settings
		if np.UpgradeSettings != nil {
			if np.UpgradeSettings.MaxSurge == 0 && np.UpgradeSettings.MaxUnavailable == 0 {
				ch <- Issue{
					Severity: SevInfo,
					Category: "GKE/NodePool",
					Resource: resource,
					Message:  "Upgrade settings: maxSurge=0, maxUnavailable=0 -- upgrades may be slow",
				}
			}
		}

		// Check node config for security settings
		if np.Config != nil {
			// Integrity monitoring
			if np.Config.ShieldedInstanceConfig != nil {
				if !np.Config.ShieldedInstanceConfig.EnableIntegrityMonitoring {
					ch <- Issue{Severity: SevInfo, Category: "GKE/NodePool", Resource: resource, Message: "Integrity monitoring not enabled on nodes"}
				}
				if !np.Config.ShieldedInstanceConfig.EnableSecureBoot {
					ch <- Issue{Severity: SevInfo, Category: "GKE/NodePool", Resource: resource, Message: "Secure boot not enabled on nodes"}
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Cloud Logging checks
// ---------------------------------------------------------------------------

func checkCloudLogs(ctx context.Context, client *logging.Client, cfg Config, filter, category, desc string, ch chan<- Issue) {
	lookback := time.Duration(cfg.LogLookbackMins) * time.Minute
	cutoff := time.Now().Add(-lookback)

	// Build the full filter with cluster scoping and time window
	fullFilter := fmt.Sprintf(
		`%s AND resource.labels.cluster_name="%s" AND timestamp>="%s"`,
		filter, cfg.ClusterName, cutoff.Format(time.RFC3339),
	)

	req := &loggingpb.ListLogEntriesRequest{
		ResourceNames: []string{fmt.Sprintf("projects/%s", cfg.ProjectID)},
		Filter:        fullFilter,
		PageSize:      5, // just enough to confirm the problem
		OrderBy:       "timestamp desc",
	}

	it := client.ListLogEntries(ctx, req)
	var entries []*loggingpb.LogEntry
	for {
		entry, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// Permission errors or disabled APIs
			if strings.Contains(err.Error(), "PERMISSION_DENIED") || strings.Contains(err.Error(), "403") {
				ch <- Issue{
					Severity: SevWarning,
					Category: category,
					Resource: cfg.ClusterName,
					Message:  "Insufficient permissions to read Cloud Logging -- check IAM roles",
				}
				return
			}
			ch <- Issue{
				Severity: SevWarning,
				Category: category,
				Resource: cfg.ClusterName,
				Message:  fmt.Sprintf("Failed to query Cloud Logging: %v", err),
			}
			return
		}
		entries = append(entries, entry)
		if len(entries) >= 5 {
			break
		}
	}

	if len(entries) > 0 {
		sev := SevWarning
		switch desc {
		case "OOMKill events", "crash loops", "scheduling failures":
			sev = SevCritical
		}

		// Extract sample from first entry
		sample := extractLogMessage(entries[0])

		var ts time.Time
		if entries[0].Timestamp != nil {
			ts = entries[0].Timestamp.AsTime()
		}

		ch <- Issue{
			Severity:  sev,
			Category:  category,
			Resource:  cfg.ClusterName,
			Message:   fmt.Sprintf("Found %s in last %dm (%d+ hits). Sample: %s", desc, cfg.LogLookbackMins, len(entries), sample),
			Timestamp: ts,
		}
	}
}

// extractLogMessage pulls a readable message from a Cloud Logging entry.
func extractLogMessage(entry *loggingpb.LogEntry) string {
	// Try textPayload first (simplest)
	if entry.GetTextPayload() != "" {
		return truncate(entry.GetTextPayload(), 200)
	}

	// Try jsonPayload
	if jp := entry.GetJsonPayload(); jp != nil {
		fields := jp.GetFields()
		// Common message field names in k8s logs
		for _, key := range []string{"message", "msg", "log", "textPayload"} {
			if v, ok := fields[key]; ok {
				if sv := v.GetStringValue(); sv != "" {
					return truncate(sv, 200)
				}
			}
		}
		// Fall back to string representation
		return truncate(jp.String(), 200)
	}

	// Proto payload as last resort
	if pp := entry.GetProtoPayload(); pp != nil {
		return truncate(fmt.Sprintf("proto: %s", pp.GetTypeUrl()), 200)
	}

	return "(no message)"
}

// ---------------------------------------------------------------------------
// Kubernetes checks -- all accept Config for tuneable thresholds
// ---------------------------------------------------------------------------

func checkNodes(ctx context.Context, cs *kubernetes.Clientset, cfg Config, ch chan<- Issue) {
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		emitAPIError(ctx, ch, "Node", err)
		return
	}

	for _, node := range nodes.Items {
		name := node.Name
		var problems []string
		ready := false

		for _, cond := range node.Status.Conditions {
			switch cond.Type {
			case corev1.NodeReady:
				if cond.Status == corev1.ConditionTrue {
					ready = true
				} else {
					problems = append(problems, fmt.Sprintf("Ready=%s (%s)", cond.Status, cond.Reason))
				}
			case corev1.NodeMemoryPressure, corev1.NodeDiskPressure, corev1.NodePIDPressure, corev1.NodeNetworkUnavailable:
				if cond.Status == corev1.ConditionTrue {
					problems = append(problems, fmt.Sprintf("%s (%s)", cond.Type, cond.Reason))
				}
			}
		}

		if !ready {
			ch <- Issue{Severity: SevCritical, Category: "Node", Resource: name, Message: fmt.Sprintf("NotReady -- %s", strings.Join(problems, "; "))}
		} else if len(problems) > 0 {
			ch <- Issue{Severity: SevWarning, Category: "Node", Resource: name, Message: fmt.Sprintf("Pressure conditions: %s", strings.Join(problems, "; "))}
		}

		// Check allocatable vs capacity for resource saturation hints
		for _, res := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage} {
			cap := node.Status.Capacity[res]
			alloc := node.Status.Allocatable[res]
			if cap.Cmp(alloc) != 0 {
				capVal := cap.Value()
				allocVal := alloc.Value()
				if capVal > 0 {
					pctUsable := float64(allocVal) / float64(capVal) * 100
					if pctUsable < 70 {
						ch <- Issue{
							Severity: SevWarning,
							Category: "Node",
							Resource: name,
							Message:  fmt.Sprintf("Only %.0f%% of %s capacity is allocatable (%s/%s) -- check system-reserved settings", pctUsable, res, alloc.String(), cap.String()),
						}
					}
				}
			}
		}

		// GKE-specific: check for containerd vs docker runtime (informational)
		if ri := node.Status.NodeInfo.ContainerRuntimeVersion; ri != "" {
			if strings.HasPrefix(ri, "docker://") {
				ch <- Issue{Severity: SevInfo, Category: "Node", Resource: name, Message: fmt.Sprintf("Running deprecated Docker runtime: %s -- migrate to containerd", ri)}
			}
		}
	}
}

func checkPods(ctx context.Context, cs *kubernetes.Clientset, cfg Config, ch chan<- Issue) {
	pods, err := cs.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		emitAPIError(ctx, ch, "Pod", err)
		return
	}

	now := time.Now()
	for _, pod := range pods.Items {
		ns := pod.Namespace
		name := pod.Name
		resource := ns + "/" + name

		switch pod.Status.Phase {
		case corev1.PodRunning:
			for _, cs := range pod.Status.ContainerStatuses {
				if !cs.Ready {
					stateStr := containerStateStr(cs.State)
					sev := SevWarning
					if strings.Contains(stateStr, "CrashLoopBackOff") {
						sev = SevCritical
					}
					ch <- Issue{Severity: sev, Category: "Pod", Resource: resource, Message: fmt.Sprintf("Container %s not ready: %s", cs.Name, stateStr)}
				}
				if cs.RestartCount > cfg.RestartThresh {
					sev := SevWarning
					if cs.RestartCount > cfg.RestartThresh*3 {
						sev = SevCritical
					}
					ch <- Issue{Severity: sev, Category: "Pod", Resource: resource, Message: fmt.Sprintf("Container %s restart count: %d", cs.Name, cs.RestartCount)}
				}
				// Detect OOMKilled from last termination state
				if cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.Reason == "OOMKilled" {
					ch <- Issue{Severity: SevCritical, Category: "Pod", Resource: resource, Message: fmt.Sprintf("Container %s was OOMKilled (exit %d)", cs.Name, cs.LastTerminationState.Terminated.ExitCode)}
				}
			}

			// Also check init containers
			for _, cs := range pod.Status.InitContainerStatuses {
				if cs.State.Waiting != nil {
					ch <- Issue{Severity: SevWarning, Category: "Pod", Resource: resource, Message: fmt.Sprintf("Init container %s waiting: %s", cs.Name, cs.State.Waiting.Reason)}
				}
			}

		case corev1.PodPending:
			if pod.Status.StartTime != nil && now.Sub(pod.Status.StartTime.Time) > cfg.PendingThresh {
				reason := pendingReason(pod)
				ch <- Issue{Severity: SevWarning, Category: "Pod", Resource: resource, Message: fmt.Sprintf("Pending >%s: %s", cfg.PendingThresh, reason)}
			}

		case corev1.PodFailed:
			msg := "Failed"
			if pod.Status.Reason != "" {
				msg = pod.Status.Reason
			}
			if pod.Status.Message != "" {
				msg += ": " + pod.Status.Message
			}
			ch <- Issue{Severity: SevCritical, Category: "Pod", Resource: resource, Message: msg}

		case corev1.PodUnknown:
			ch <- Issue{Severity: SevCritical, Category: "Pod", Resource: resource, Message: "Phase Unknown -- possible kubelet connectivity loss"}
		}
	}
}

func checkDeployments(ctx context.Context, cs *kubernetes.Clientset, cfg Config, ch chan<- Issue) {
	deployments, err := cs.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		emitAPIError(ctx, ch, "Deployment", err)
		return
	}
	for _, d := range deployments.Items {
		if d.Spec.Replicas == nil {
			continue
		}
		desired := *d.Spec.Replicas
		ready := d.Status.ReadyReplicas
		resource := d.Namespace + "/" + d.Name

		if ready == 0 && desired > 0 {
			ch <- Issue{Severity: SevCritical, Category: "Deployment", Resource: resource, Message: fmt.Sprintf("0/%d replicas ready -- complete outage", desired)}
		} else if ready < desired {
			ch <- Issue{Severity: SevWarning, Category: "Deployment", Resource: resource, Message: fmt.Sprintf("%d/%d replicas ready", ready, desired)}
		}

		// Detect stuck rollouts
		for _, cond := range d.Status.Conditions {
			if cond.Type == "Progressing" && cond.Status == "False" && cond.Reason == "ProgressDeadlineExceeded" {
				ch <- Issue{Severity: SevCritical, Category: "Deployment", Resource: resource, Message: "Rollout stalled -- ProgressDeadlineExceeded"}
			}
		}
	}
}

func checkDaemonSets(ctx context.Context, cs *kubernetes.Clientset, cfg Config, ch chan<- Issue) {
	dsList, err := cs.AppsV1().DaemonSets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		emitAPIError(ctx, ch, "DaemonSet", err)
		return
	}
	for _, ds := range dsList.Items {
		resource := ds.Namespace + "/" + ds.Name
		desired := ds.Status.DesiredNumberScheduled

		if desired > 0 && ds.Status.CurrentNumberScheduled < desired {
			ch <- Issue{Severity: SevCritical, Category: "DaemonSet", Resource: resource, Message: fmt.Sprintf("%d/%d scheduled", ds.Status.CurrentNumberScheduled, desired)}
		}
		if ds.Status.NumberUnavailable > 0 {
			ch <- Issue{Severity: SevWarning, Category: "DaemonSet", Resource: resource, Message: fmt.Sprintf("%d pods unavailable", ds.Status.NumberUnavailable)}
		}
		if ds.Status.NumberMisscheduled > 0 {
			ch <- Issue{Severity: SevWarning, Category: "DaemonSet", Resource: resource, Message: fmt.Sprintf("%d pods mis-scheduled", ds.Status.NumberMisscheduled)}
		}
	}
}

func checkStatefulSets(ctx context.Context, cs *kubernetes.Clientset, cfg Config, ch chan<- Issue) {
	stsList, err := cs.AppsV1().StatefulSets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		emitAPIError(ctx, ch, "StatefulSet", err)
		return
	}
	for _, sts := range stsList.Items {
		if sts.Spec.Replicas == nil {
			continue
		}
		desired := *sts.Spec.Replicas
		ready := sts.Status.ReadyReplicas
		resource := sts.Namespace + "/" + sts.Name

		if ready == 0 && desired > 0 {
			ch <- Issue{Severity: SevCritical, Category: "StatefulSet", Resource: resource, Message: fmt.Sprintf("0/%d replicas ready", desired)}
		} else if ready < desired {
			ch <- Issue{Severity: SevWarning, Category: "StatefulSet", Resource: resource, Message: fmt.Sprintf("%d/%d replicas ready", ready, desired)}
		}

		// Detect stuck updates
		if sts.Status.UpdateRevision != "" && sts.Status.CurrentRevision != sts.Status.UpdateRevision {
			if sts.Status.UpdatedReplicas < desired {
				ch <- Issue{Severity: SevWarning, Category: "StatefulSet", Resource: resource, Message: fmt.Sprintf("Update in progress: %d/%d updated (current=%s target=%s)", sts.Status.UpdatedReplicas, desired, sts.Status.CurrentRevision, sts.Status.UpdateRevision)}
			}
		}
	}
}

func checkServices(ctx context.Context, cs *kubernetes.Clientset, cfg Config, ch chan<- Issue) {
	svcList, err := cs.CoreV1().Services(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		emitAPIError(ctx, ch, "Service", err)
		return
	}

	epList, err := cs.CoreV1().Endpoints(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		// Non-fatal, just skip endpoint cross-ref
		epList = nil
	}

	epMap := make(map[string]int) // ns/name -> ready address count
	if epList != nil {
		for _, ep := range epList.Items {
			key := ep.Namespace + "/" + ep.Name
			count := 0
			for _, subset := range ep.Subsets {
				count += len(subset.Addresses)
			}
			epMap[key] = count
		}
	}

	for _, svc := range svcList.Items {
		if svc.Spec.Type == corev1.ServiceTypeExternalName {
			continue
		}
		resource := svc.Namespace + "/" + svc.Name
		key := svc.Namespace + "/" + svc.Name

		if svc.Spec.Selector != nil && len(svc.Spec.Selector) > 0 {
			if count, exists := epMap[key]; exists && count == 0 {
				ch <- Issue{Severity: SevWarning, Category: "Service", Resource: resource, Message: "Has selector but 0 ready endpoints"}
			}
		}

		// LoadBalancer without external IP
		if svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
			if len(svc.Status.LoadBalancer.Ingress) == 0 {
				ch <- Issue{Severity: SevWarning, Category: "Service", Resource: resource, Message: "LoadBalancer type but no external IP/hostname assigned"}
			}
		}
	}
}

func checkPVCs(ctx context.Context, cs *kubernetes.Clientset, cfg Config, ch chan<- Issue) {
	pvcList, err := cs.CoreV1().PersistentVolumeClaims(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		emitAPIError(ctx, ch, "PVC", err)
		return
	}
	for _, pvc := range pvcList.Items {
		resource := pvc.Namespace + "/" + pvc.Name
		if pvc.Status.Phase == corev1.ClaimPending {
			ch <- Issue{Severity: SevWarning, Category: "PVC", Resource: resource, Message: "Claim pending -- check StorageClass and provisioner"}
		} else if pvc.Status.Phase == corev1.ClaimLost {
			ch <- Issue{Severity: SevCritical, Category: "PVC", Resource: resource, Message: "Claim lost -- bound PV no longer exists"}
		}
	}
}

func checkEvents(ctx context.Context, cs *kubernetes.Clientset, cfg Config, ch chan<- Issue) {
	cutoff := time.Now().Add(-cfg.EventWindow)

	events, err := cs.CoreV1().Events(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: "type=Warning",
		Limit:         500,
	})
	if err != nil {
		emitAPIError(ctx, ch, "Event", err)
		return
	}

	// Aggregate events by reason to avoid flooding
	type eventKey struct {
		kind   string
		reason string
	}
	agg := make(map[eventKey]int)

	for _, ev := range events.Items {
		ts := eventTimestamp(ev)
		if ts.IsZero() || ts.Before(cutoff) {
			continue
		}

		key := eventKey{kind: ev.InvolvedObject.Kind, reason: ev.Reason}
		agg[key]++

		// Only emit first occurrence per key
		if agg[key] == 1 {
			sev := SevWarning
			// Escalate well-known critical reasons
			switch ev.Reason {
			case "OOMKilling", "Evicted", "FailedMount", "FailedAttachVolume", "NodeNotReady":
				sev = SevCritical
			}
			ch <- Issue{
				Severity:  sev,
				Category:  "Event",
				Resource:  fmt.Sprintf("%s/%s", ev.InvolvedObject.Kind, ev.InvolvedObject.Name),
				Message:   fmt.Sprintf("[%s] %s (ns: %s)", ev.Reason, truncate(ev.Message, 150), ev.Namespace),
				Timestamp: ts,
			}
		}
	}
}

func checkResourceQuotas(ctx context.Context, cs *kubernetes.Clientset, cfg Config, ch chan<- Issue) {
	quotaList, err := cs.CoreV1().ResourceQuotas(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		emitAPIError(ctx, ch, "ResourceQuota", err)
		return
	}
	for _, q := range quotaList.Items {
		resource := q.Namespace + "/" + q.Name
		for resName, hard := range q.Status.Hard {
			used, exists := q.Status.Used[resName]
			if !exists {
				continue
			}
			hardVal := hard.Value()
			usedVal := used.Value()
			if hardVal > 0 {
				pct := float64(usedVal) / float64(hardVal) * 100
				if pct >= 90 {
					ch <- Issue{
						Severity: SevWarning,
						Category: "ResourceQuota",
						Resource: resource,
						Message:  fmt.Sprintf("%s at %.0f%% (%s/%s)", resName, pct, used.String(), hard.String()),
					}
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func emitAPIError(ctx context.Context, ch chan<- Issue, category string, err error) {
	if ctx.Err() != nil {
		return // context cancelled/timed out, don't report
	}
	ch <- Issue{Severity: SevCritical, Category: category, Resource: "-", Message: fmt.Sprintf("API list failed: %v", err)}
}

func containerStateStr(state corev1.ContainerState) string {
	switch {
	case state.Waiting != nil:
		return fmt.Sprintf("Waiting: %s", state.Waiting.Reason)
	case state.Running != nil:
		return "Running (not ready)"
	case state.Terminated != nil:
		return fmt.Sprintf("Terminated: exit=%d reason=%s", state.Terminated.ExitCode, state.Terminated.Reason)
	default:
		return "Unknown"
	}
}

func pendingReason(pod corev1.Pod) string {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse {
			return fmt.Sprintf("Unschedulable: %s", cond.Message)
		}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil {
			return cs.State.Waiting.Reason
		}
	}
	return "unknown"
}

func eventTimestamp(ev corev1.Event) time.Time {
	if ev.EventTime.Time != (time.Time{}) {
		return ev.EventTime.Time
	}
	if ev.LastTimestamp.Time != (time.Time{}) {
		return ev.LastTimestamp.Time
	}
	if ev.FirstTimestamp.Time != (time.Time{}) {
		return ev.FirstTimestamp.Time
	}
	return time.Time{}
}

func truncate(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

// Ensure timestamppb is used (needed for Cloud Logging entry timestamps)
var _ = timestamppb.Now
