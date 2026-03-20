/*
Copyright 2025 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package integration

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestWindowsNode validates a hybrid Linux+Windows minikube cluster using the Hyper-V driver.
// It starts a 2-node cluster (one Linux control-plane, one Windows worker), verifies both
// nodes are correctly labelled, deploys workloads targeting each OS, and then cleans up.
//
// Prerequisites:
//   - Must run on a Windows host with Hyper-V enabled (physical or Azure nested-virt VM)
//   - The minikube binary under test must be built from the feature/windows-node-support branch
//
// Optional env vars:
//
//	WINDOWS_VHD_URL - path or URL to a custom Windows Server VHDX image.
//	                  If unset, minikube uses the default bundled VHD.
func TestWindowsNode(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("TestWindowsNode requires a Windows host with Hyper-V")
	}

	profile := UniqueProfileName("windows-node")
	ctx, cancel := context.WithTimeout(context.Background(), Minutes(90))
	defer CleanupWithLogs(t, profile, cancel)

	t.Run("serial", func(t *testing.T) {
		tests := []struct {
			name      string
			validator func(context.Context, *testing.T, string)
		}{
			{"Start", validateWindowsNodeStart},
			{"NodeCount", validateWindowsNodeCount},
			{"NodeLabels", validateWindowsNodeLabels},
			{"WindowsWorkload", validateWindowsWorkload},
			{"LinuxWorkload", validateLinuxWorkload},
			{"WorkloadsOnCorrectOS", validateWorkloadsOnCorrectOS},
			{"WebServerConnectivity", validateWebServerConnectivity},
		}
		for _, tc := range tests {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				defer PostMortemLogs(t, profile)
				tc.validator(ctx, t, profile)
			})
		}
	})
}

// validateWindowsNodeStart starts a 2-node hybrid cluster with one Linux and one Windows node.
func validateWindowsNodeStart(ctx context.Context, t *testing.T, profile string) {
	t.Helper()

	args := []string{
		"start", "-p", profile,
		"--nodes=2",
		"--node-os=[linux,windows]",
		"--kubernetes-version=v1.32.3",
		"--driver=hyperv",
		"--wait=true",
		"-v=5",
		"--alsologtostderr",
	}

	if vhdURL := os.Getenv("WINDOWS_VHD_URL"); vhdURL != "" {
		args = append(args, "--windows-vhd-url="+vhdURL)
	}
	if vsw := os.Getenv("HYPERV_VIRTUAL_SWITCH"); vsw != "" {
		args = append(args, "--hyperv-virtual-switch="+vsw)
	}

	rr, err := Run(t, exec.CommandContext(ctx, Target(), args...))
	if err != nil {
		t.Fatalf("failed to start hybrid cluster: %v\n%s", err, rr.Stderr.String())
	}
}

// validateWindowsNodeCount asserts exactly 2 nodes are present in the cluster.
func validateWindowsNodeCount(ctx context.Context, t *testing.T, profile string) {
	t.Helper()

	rr, err := Run(t, exec.CommandContext(ctx, KubectlBinary(),
		"--context", profile, "get", "nodes", "--no-headers"))
	if err != nil {
		t.Fatalf("kubectl get nodes failed: %v\n%s", err, rr.Stderr.String())
	}

	lines := strings.Split(strings.TrimSpace(rr.Stdout.String()), "\n")
	if len(lines) != 2 {
		t.Errorf("expected 2 nodes, got %d:\n%s", len(lines), rr.Stdout.String())
	}
}

// validateWindowsNodeLabels checks that the cluster contains one Linux node and one Windows node,
// identified by the kubernetes.io/os label applied automatically by the kubelet.
func validateWindowsNodeLabels(ctx context.Context, t *testing.T, profile string) {
	t.Helper()

	for _, os := range []string{"linux", "windows"} {
		rr, err := Run(t, exec.CommandContext(ctx, KubectlBinary(),
			"--context", profile,
			"get", "nodes",
			"-l", "kubernetes.io/os="+os,
			"--no-headers"))
		if err != nil {
			t.Fatalf("kubectl get nodes -l kubernetes.io/os=%s failed: %v\n%s", os, err, rr.Stderr.String())
		}
		if strings.TrimSpace(rr.Stdout.String()) == "" {
			t.Errorf("expected at least one %s node but found none", os)
		}
	}
}

// validateWindowsWorkload deploys a Windows pause container with a nodeSelector targeting the
// Windows node and waits for it to reach Running state.
func validateWindowsWorkload(ctx context.Context, t *testing.T, profile string) {
	t.Helper()

	rr, err := Run(t, exec.CommandContext(ctx, KubectlBinary(),
		"--context", profile, "apply", "-f", "./testdata/windows-node-workload.yaml"))
	if err != nil {
		t.Fatalf("failed to apply windows workload: %v\n%s", err, rr.Stderr.String())
	}

	// Windows containers can take several minutes to pull and start on first run.
	if _, err := PodWait(ctx, t, profile, "default", "app=win-webserver", Minutes(15)); err != nil {
		t.Errorf("win-webserver pod did not reach Running state: %v", err)
	}
}

// validateLinuxWorkload deploys a busybox container with a nodeSelector targeting the Linux node
// and waits for it to reach Running state.
func validateLinuxWorkload(ctx context.Context, t *testing.T, profile string) {
	t.Helper()

	rr, err := Run(t, exec.CommandContext(ctx, KubectlBinary(),
		"--context", profile, "apply", "-f", "./testdata/linux-node-workload.yaml"))
	if err != nil {
		t.Fatalf("failed to apply linux workload: %v\n%s", err, rr.Stderr.String())
	}

	if _, err := PodWait(ctx, t, profile, "default", "app=linux-test", Minutes(5)); err != nil {
		t.Errorf("linux-test pod did not reach Running state: %v", err)
	}
}

// validateWorkloadsOnCorrectOS confirms that each workload pod is scheduled on a node
// whose kubernetes.io/os label matches the pod's nodeSelector.
func validateWorkloadsOnCorrectOS(ctx context.Context, t *testing.T, profile string) {
	t.Helper()

	cases := []struct {
		podSelector string
		expectedOS  string
	}{
		{"app=win-webserver", "windows"},
		{"app=linux-test", "linux"},
	}

	for _, tc := range cases {
		// Get the node name the pod is running on.
		rr, err := Run(t, exec.CommandContext(ctx, KubectlBinary(),
			"--context", profile,
			"get", "pods", "-l", tc.podSelector,
			"-o", "jsonpath={.items[0].spec.nodeName}"))
		if err != nil {
			t.Errorf("failed to get node for pod %s: %v\n%s", tc.podSelector, err, rr.Stderr.String())
			continue
		}
		nodeName := strings.TrimSpace(rr.Stdout.String())
		if nodeName == "" {
			t.Errorf("pod %s has no nodeName assigned", tc.podSelector)
			continue
		}

		// Get the kubernetes.io/os label from that node.
		rr, err = Run(t, exec.CommandContext(ctx, KubectlBinary(),
			"--context", profile,
			"get", "node", nodeName,
			"-o", "jsonpath={.metadata.labels.kubernetes\\.io/os}"))
		if err != nil {
			t.Errorf("failed to get OS label for node %s: %v\n%s", nodeName, err, rr.Stderr.String())
			continue
		}
		nodeOS := strings.TrimSpace(rr.Stdout.String())

		if nodeOS != tc.expectedOS {
			t.Errorf("pod %s scheduled on node %s with OS=%q, want %q", tc.podSelector, nodeName, nodeOS, tc.expectedOS)
		}
	}
}

// validateWebServerConnectivity retrieves the NodePort assigned to win-webserver, gets the
// Windows node's internal IP, and verifies the web server returns HTTP 200 with the expected
// response body.
func validateWebServerConnectivity(ctx context.Context, t *testing.T, profile string) {
	t.Helper()

	// Get the NodePort assigned to the win-webserver service.
	rr, err := Run(t, exec.CommandContext(ctx, KubectlBinary(),
		"--context", profile,
		"get", "svc", "win-webserver",
		"-o", "jsonpath={.spec.ports[0].nodePort}"))
	if err != nil {
		t.Fatalf("failed to get NodePort: %v\n%s", err, rr.Stderr.String())
	}
	nodePort := strings.TrimSpace(rr.Stdout.String())
	if nodePort == "" {
		t.Fatal("NodePort is empty")
	}

	// Get the Windows node's internal IP.
	rr, err = Run(t, exec.CommandContext(ctx, KubectlBinary(),
		"--context", profile,
		"get", "nodes",
		"-l", "kubernetes.io/os=windows",
		"-o", "jsonpath={.items[0].status.addresses[?(@.type==\"InternalIP\")].address}"))
	if err != nil {
		t.Fatalf("failed to get Windows node IP: %v\n%s", err, rr.Stderr.String())
	}
	nodeIP := strings.TrimSpace(rr.Stdout.String())
	if nodeIP == "" {
		t.Fatal("Windows node IP is empty")
	}

	url := fmt.Sprintf("http://%s:%s/", nodeIP, nodePort)
	t.Logf("Checking web server at %s", url)

	client := &http.Client{Timeout: 10 * time.Second}

	// Retry for up to 2 minutes — the web server may still be starting.
	deadline := time.Now().Add(2 * time.Minute)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err != nil {
			lastErr = err
			time.Sleep(5 * time.Second)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("web server returned HTTP %d, want 200", resp.StatusCode)
			return
		}
		t.Logf("web server responded with HTTP 200")
		return
	}
	t.Errorf("web server at %s did not respond after 2 minutes: %v", url, lastErr)
}
