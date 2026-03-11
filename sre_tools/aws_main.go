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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/eks"
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
	// Also disable if TERM=dumb or stdout is not a terminal
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
	Category  string // e.g. "Node", "Pod", "Deployment", "EKS/Logs"
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
	ClusterName     string // auto-detected if empty
	AWSRegion       string // auto-detected from SDK default chain
}

func loadConfig() Config {
	cfg := Config{
		TimeoutSeconds:  envInt("EKSDIAG_TIMEOUT", 45),
		EventWindow:     time.Duration(envInt("EKSDIAG_EVENT_WINDOW_SEC", 300)) * time.Second,
		LogLookbackMins: envInt("EKSDIAG_LOG_LOOKBACK_MIN", 10),
		PendingThresh:   time.Duration(envInt("EKSDIAG_PENDING_THRESH_SEC", 120)) * time.Second,
		RestartThresh:   int32(envInt("EKSDIAG_RESTART_THRESH", 3)),
		ClusterName:     os.Getenv("EKSDIAG_CLUSTER_NAME"),
		AWSRegion:       os.Getenv("AWS_REGION"),
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
	// Build clients concurrently -- k8s + AWS at the same time
	// ------------------------------------------------------------------
	var (
		clientset *kubernetes.Clientset
		awsCfg    aws.Config
		k8sErr    error
		awsErr    error
		wgInit    sync.WaitGroup
	)

	wgInit.Add(2)
	go func() {
		defer wgInit.Done()
		clientset, k8sErr = getClientset()
	}()
	go func() {
		defer wgInit.Done()
		opts := []func(*awsconfig.LoadOptions) error{}
		if cfg.AWSRegion != "" {
			opts = append(opts, awsconfig.WithRegion(cfg.AWSRegion))
		}
		awsCfg, awsErr = awsconfig.LoadDefaultConfig(ctx, opts...)
	}()
	wgInit.Wait()

	if k8sErr != nil {
		fmt.Fprintf(os.Stderr, "%sFATAL%s Kubernetes client init failed: %v\n", colRed, colReset, k8sErr)
		os.Exit(1)
	}

	// AWS is non-fatal -- we just skip log checks
	var cwClient *cloudwatchlogs.Client
	var eksClient *eks.Client
	if awsErr != nil {
		fmt.Fprintf(os.Stderr, "%sWARN%s  AWS SDK init failed (%v) -- skipping CloudWatch log checks\n", colYellow, colReset, awsErr)
	} else {
		cwClient = cloudwatchlogs.NewFromConfig(awsCfg)
		eksClient = eks.NewFromConfig(awsCfg)
	}

	// Auto-detect cluster name if not provided
	if cfg.ClusterName == "" && eksClient != nil {
		cfg.ClusterName = detectClusterName(ctx, eksClient)
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

	// AWS EKS / CloudWatch checks
	if cwClient != nil && cfg.ClusterName != "" {
		logGroups := []struct {
			suffix   string
			category string
		}{
			{"/aws/eks/" + cfg.ClusterName + "/cluster", "EKS/ControlPlane"},
			{"/aws/containerinsights/" + cfg.ClusterName + "/application", "EKS/AppLogs"},
			{"/aws/containerinsights/" + cfg.ClusterName + "/host", "EKS/HostLogs"},
			{"/aws/containerinsights/" + cfg.ClusterName + "/dataplane", "EKS/DataPlane"},
			{"/aws/containerinsights/" + cfg.ClusterName + "/performance", "EKS/Performance"},
		}
		for _, lg := range logGroups {
			wg.Add(1)
			go func(group, cat string) {
				defer wg.Done()
				checkCloudWatchLogs(ctx, cwClient, group, cat, cfg, issues)
			}(lg.suffix, lg.category)
		}

		// EKS cluster-level health check
		wg.Add(1)
		go func() {
			defer wg.Done()
			checkEKSClusterHealth(ctx, eksClient, cfg.ClusterName, issues)
		}()
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
	outputFormat := os.Getenv("EKSDIAG_OUTPUT")
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

	fmt.Printf("\n%s=== EKS Cluster Diagnostic Report ===%s\n", colBold, colReset)
	if cfg.ClusterName != "" {
		fmt.Printf("  Cluster  : %s%s%s\n", colCyan, cfg.ClusterName, colReset)
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
	Cluster   string         `json:"cluster,omitempty"`
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
		Cluster:   cfg.ClusterName,
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
// EKS cluster auto-detection + health
// ---------------------------------------------------------------------------

func detectClusterName(ctx context.Context, client *eks.Client) string {
	out, err := client.ListClusters(ctx, &eks.ListClustersInput{MaxResults: aws.Int32(1)})
	if err != nil || len(out.Clusters) == 0 {
		return ""
	}
	return out.Clusters[0]
}

func checkEKSClusterHealth(ctx context.Context, client *eks.Client, clusterName string, ch chan<- Issue) {
	out, err := client.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: &clusterName})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		ch <- Issue{Severity: SevWarning, Category: "EKS/Cluster", Resource: clusterName, Message: fmt.Sprintf("Failed to describe cluster: %v", err)}
		return
	}

	cluster := out.Cluster
	if cluster.Status != "ACTIVE" {
		ch <- Issue{Severity: SevCritical, Category: "EKS/Cluster", Resource: clusterName, Message: fmt.Sprintf("Cluster status is %s (expected ACTIVE)", cluster.Status)}
	}

	// Check control plane logging is enabled
	if cluster.Logging != nil {
		for _, logSetup := range cluster.Logging.ClusterLogging {
			if !aws.ToBool(logSetup.Enabled) {
				types := make([]string, 0, len(logSetup.Types))
				for _, t := range logSetup.Types {
					types = append(types, string(t))
				}
				ch <- Issue{
					Severity: SevInfo,
					Category: "EKS/Cluster",
					Resource: clusterName,
					Message:  fmt.Sprintf("Control plane log types disabled: %s -- enable for better observability", strings.Join(types, ", ")),
				}
			}
		}
	}

	// Platform version / Kubernetes version
	if cluster.PlatformVersion != nil && cluster.Version != nil {
		ch <- Issue{
			Severity: SevInfo,
			Category: "EKS/Cluster",
			Resource: clusterName,
			Message:  fmt.Sprintf("Running k8s %s (platform %s)", *cluster.Version, *cluster.PlatformVersion),
		}
	}

	// Check for cluster health issues reported by EKS
	if cluster.Health != nil {
		for _, hi := range cluster.Health.Issues {
			ch <- Issue{
				Severity: SevWarning,
				Category: "EKS/Cluster",
				Resource: clusterName,
				Message:  fmt.Sprintf("EKS health issue: code=%s message=%s", string(hi.Code), aws.ToString(hi.Message)),
			}
		}
	}
}

// ---------------------------------------------------------------------------
// CloudWatch log checks
// ---------------------------------------------------------------------------

func checkCloudWatchLogs(ctx context.Context, client *cloudwatchlogs.Client, logGroup, category string, cfg Config, ch chan<- Issue) {
	startTime := time.Now().Add(-time.Duration(cfg.LogLookbackMins) * time.Minute).UnixMilli()

	// Filter for error-level patterns across common EKS log formats
	filterPatterns := []struct {
		pattern string
		desc    string
	}{
		{`?ERROR ?error ?"level":"error" ?"severity":"error"`, "errors"},
		{`?OOMKilled ?"reason":"OOMKilled"`, "OOMKill events"},
		{`?"Failed to pull image" ?"ImagePullBackOff" ?"ErrImagePull"`, "image pull failures"},
		{`?"CrashLoopBackOff"`, "crash loops"},
		{`?"FailedScheduling" ?"Insufficient" ?"Unschedulable"`, "scheduling failures"},
		{`?"CERTIFICATE" ?"x509" ?"TLS handshake"`, "certificate/TLS errors"},
		{`?"connection refused" ?"context deadline exceeded" ?"i/o timeout"`, "connectivity issues"},
	}

	for _, fp := range filterPatterns {
		select {
		case <-ctx.Done():
			return
		default:
		}

		input := &cloudwatchlogs.FilterLogEventsInput{
			LogGroupName:  aws.String(logGroup),
			StartTime:     aws.Int64(startTime),
			FilterPattern: aws.String(fp.pattern),
			Limit:         aws.Int32(5), // just enough to confirm the problem
		}

		out, err := client.FilterLogEvents(ctx, input)
		if err != nil {
			// Log group may not exist -- not all clusters have all groups
			if strings.Contains(err.Error(), "ResourceNotFoundException") {
				return
			}
			if ctx.Err() != nil {
				return
			}
			ch <- Issue{
				Severity: SevWarning,
				Category: category,
				Resource: logGroup,
				Message:  fmt.Sprintf("Failed to query logs: %v", err),
			}
			return
		}

		if len(out.Events) > 0 {
			sev := SevWarning
			// OOMKill, crash loops, and scheduling failures are critical
			switch fp.desc {
			case "OOMKill events", "crash loops", "scheduling failures":
				sev = SevCritical
			}

			// Extract a sample message (first event, truncated)
			sample := truncate(aws.ToString(out.Events[0].Message), 200)

			ch <- Issue{
				Severity:  sev,
				Category:  category,
				Resource:  logGroup,
				Message:   fmt.Sprintf("Found %s in last %dm (%d+ hits). Sample: %s", fp.desc, cfg.LogLookbackMins, len(out.Events), sample),
				Timestamp: time.UnixMilli(aws.ToInt64(out.Events[0].Timestamp)),
			}
		}
	}
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
	// Check container waiting reasons
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
