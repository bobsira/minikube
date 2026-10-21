/*
Copyright 2026 The Kubernetes Authors All rights reserved.

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
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestWindowsDNSAnswer(t *testing.T) {
	const name = "kubernetes.default.svc.cluster.local"
	for _, tc := range []struct {
		name   string
		output string
		want   bool
	}{
		{"answer", "Server: kube-dns\nAddress: 10.96.0.10\n\nName: " + name + "\nAddress: 10.96.0.1\n", true},
		{"windows line endings", "Name:\tKUBERNETES.DEFAULT.SVC.CLUSTER.LOCAL.\r\nAddress:\t10.96.0.1\r\n", true},
		{"multiple addresses", "Name: " + name + "\nAddresses: fd00::1\n  10.96.0.1\n", true},
		{"server address only", "Server: kube-dns\nAddress: 10.96.0.1\n", false},
		{"NXDOMAIN", "Server: kube-dns\nAddress: 10.96.0.10\n*** kube-dns can't find " + name + ": Non-existent domain\n", false},
		{"wrong name", "Name: other.default.svc.cluster.local\nAddress: 10.96.0.1\n", false},
		{"wrong address", "Name: " + name + "\nAddress: 10.96.0.10\n", false},
		{"address before name", "Address: 10.96.0.1\nName: " + name + "\n", false},
		{"different answer blocks", "Name: " + name + "\nAddress: 10.96.0.10\nName: other\nAddress: 10.96.0.1\n", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := windowsDNSAnswerMatches(tc.output, name, "10.96.0.1"); got != tc.want {
				t.Errorf("windowsDNSAnswerMatches(%q) = %t, want %t", tc.output, got, tc.want)
			}
		})
	}
}

func TestWindowsWebResponse(t *testing.T) {
	const body = "<html><body><H1>Windows Container Web Server</H1><p>callerCount 1</body></html>"
	for _, tc := range []struct {
		name   string
		status int
		body   string
		wantOK bool
	}{
		{"expected server", http.StatusOK, body, true},
		{"empty response", http.StatusOK, "", false},
		{"unrelated server", http.StatusOK, "<html>Welcome to nginx</html>", false},
		{"error with matching body", http.StatusInternalServerError, body, false},
		{"redirect", http.StatusFound, body, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := windowsWebResponseError(tc.status, tc.body); (err == nil) != tc.wantOK {
				t.Errorf("windowsWebResponseError() = %v, want success=%t", err, tc.wantOK)
			}
		})
	}
}

func TestWindowsDiagnosticPod(t *testing.T) {
	for _, tc := range []struct {
		namespace string
		label     string
		value     string
		want      bool
	}{
		{"kube-system", "k8s-app", "kube-proxy-windows", true},
		{"kube-flannel", "app", "flannel", true},
		{"default", "app", "win-webserver", true},
		{"default", "app", "windows-image-preparation", true},
		{"kube-system", "k8s-app", "kube-proxy", false},
		{"other", "app", "win-webserver", false},
		{"default", "app", "unrelated", false},
	} {
		t.Run(tc.namespace+"/"+tc.value, func(t *testing.T) {
			pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Namespace: tc.namespace,
				Labels:    map[string]string{tc.label: tc.value},
			}}
			if got := windowsDiagnosticPod(pod); got != tc.want {
				t.Errorf("windowsDiagnosticPod() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestWindowsWorkloadReadiness(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "windows-node-workload.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	decoder := yaml.NewYAMLOrJSONDecoder(f, 4096)
	var service corev1.Service
	if err := decoder.Decode(&service); err != nil {
		t.Fatal(err)
	}
	var deployment appsv1.Deployment
	if err := decoder.Decode(&deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 2 {
		t.Fatal("Windows workload must retain two replicas")
	}
	if deployment.Spec.ProgressDeadlineSeconds == nil || *deployment.Spec.ProgressDeadlineSeconds != 1500 {
		t.Fatal("deployment progress deadline must retain the full 25-minute readiness budget")
	}
	if len(deployment.Spec.Template.Spec.Containers) != 1 {
		t.Fatal("expected one web-server container")
	}
	probe := deployment.Spec.Template.Spec.Containers[0].ReadinessProbe
	if probe == nil || probe.HTTPGet == nil {
		t.Fatal("Windows workload requires HTTP readiness")
	}
	if probe.HTTPGet.Path != "/" || probe.HTTPGet.Port.IntVal != 80 {
		t.Errorf("unexpected readiness endpoint: %+v", probe.HTTPGet)
	}
	if probe.PeriodSeconds != 5 || probe.TimeoutSeconds != 5 {
		t.Errorf("unexpected readiness timing: %+v", probe)
	}

	preparation, err := os.Open(filepath.Join("testdata", "windows-node-image-preparation.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer preparation.Close()
	var pod corev1.Pod
	if err := yaml.NewYAMLOrJSONDecoder(preparation, 4096).Decode(&pod); err != nil {
		t.Fatal(err)
	}
	if pod.Name != "windows-image-preparation" || pod.Labels["app"] != pod.Name {
		t.Fatalf("unexpected image preparation pod metadata: %+v", pod.ObjectMeta)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever || len(pod.Spec.Containers) != 1 {
		t.Fatal("image preparation must run a single container to completion without restarts")
	}
	container := pod.Spec.Containers[0]
	workload := deployment.Spec.Template.Spec
	imagePattern := regexp.MustCompile(`^mcr\.microsoft\.com/windows/servercore:ltsc2025@sha256:[0-9a-f]{64}$`)
	if !imagePattern.MatchString(container.Image) || container.Image != workload.Containers[0].Image {
		t.Fatal("image preparation and workload must use the same digest-pinned Server Core image")
	}
	if container.ImagePullPolicy != corev1.PullIfNotPresent || workload.Containers[0].ImagePullPolicy != corev1.PullIfNotPresent {
		t.Fatal("image preparation and workload must reuse the cached image")
	}
	if !reflect.DeepEqual(container.Command, []string{"cmd.exe"}) || !reflect.DeepEqual(container.Args, []string{"/c", "exit", "0"}) {
		t.Fatal("image preparation must successfully start and exit the Windows container")
	}
	if pod.Spec.NodeSelector["kubernetes.io/os"] != "windows" || !reflect.DeepEqual(pod.Spec.NodeSelector, workload.NodeSelector) ||
		!reflect.DeepEqual(pod.Spec.Tolerations, workload.Tolerations) {
		t.Fatal("image preparation must target the same Windows node as the workload")
	}
}

func TestWindowsCleanup(t *testing.T) {
	const scenarioEnv = "MINIKUBE_TEST_WINDOWS_CLEANUP"
	if scenario := os.Getenv(scenarioEnv); scenario != "" {
		*binaryPath = filepath.Join(t.TempDir(), "nonexistent-minikube")
		*cleanup = scenario != "disabled"
		*testdataDir = t.TempDir()
		script := "param($Profile, $MinikubeHome, $Since, [switch]$VerifyDeleted)\nWrite-Output 'mock-host-cleanup'\n"
		if err := os.WriteFile(filepath.Join(*testdataDir, "windows-node-cleanup-diagnostics.ps1"), []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		cleanupWindowsNode(t, "cleanup-unit-test")
		return
	}

	for _, scenario := range []string{"failure", "disabled"} {
		t.Run(scenario, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsCleanup$", "-test.v")
			cmd.Env = append(os.Environ(), scenarioEnv+"="+scenario)
			output, err := cmd.CombinedOutput()
			if scenario == "disabled" {
				if err != nil || !strings.Contains(string(output), "skipping cleanup") {
					t.Fatalf("disabled cleanup: %v\n%s", err, output)
				}
				if strings.Contains(string(output), "mock-host-cleanup") {
					t.Fatal("disabled cleanup must not run host diagnostics")
				}
				return
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || !strings.Contains(string(output), "failed cleanup") {
				t.Fatalf("cleanup failure must fail the test process: %v\n%s", err, output)
			}
			if runtime.GOOS == "windows" && !strings.Contains(string(output), "mock-host-cleanup") {
				t.Fatal("failed cleanup must still collect host diagnostics")
			}
		})
	}
}

func TestWindowsCleanupCancellation(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires Windows process termination")
	}
	const scenarioEnv = "MINIKUBE_TEST_WINDOWS_CLEANUP_CANCEL"
	const pidFileEnv = "MINIKUBE_TEST_WINDOWS_CLEANUP_CHILD_PID"
	switch os.Getenv(scenarioEnv) {
	case "child":
		time.Sleep(45 * time.Second)
		return
	case "parent":
		cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsCleanupCancellation$")
		cmd.Env = append(os.Environ(), scenarioEnv+"=child")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer cmd.Process.Kill()
		if err := os.WriteFile(os.Getenv(pidFileEnv), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(45 * time.Second)
		return
	}

	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWindowsCleanupCancellation$")
	cmd.Env = append(os.Environ(), scenarioEnv+"=parent", pidFileEnv+"="+pidFile)
	startedAt := time.Now()
	rr, err := runWindowsCleanupCommand(t, cmd)
	elapsed := time.Since(startedAt)
	if err == nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) || elapsed > 12*time.Second {
		t.Errorf("cleanup cancellation: error=%v, context=%v, elapsed=%s\n%s", err, ctx.Err(), elapsed, rr.Output())
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("helper did not start its child: %v\n%s", err, rr.Output())
	}
	pid, err := strconv.ParseInt(string(data), 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	child, err := process.NewProcess(int32(pid))
	if err != nil {
		if !errors.Is(err, process.ErrorProcessNotRunning) {
			t.Fatal(err)
		}
		return
	}
	defer child.Kill()
	if running, err := child.IsRunning(); err != nil || running {
		t.Errorf("cleanup child still running=%t: %v", running, err)
	}
}

func TestWindowsWorkflowFailureHandling(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", ".github", "workflows", "functional_extra.yml"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name            string `json:"name"`
				If              string `json:"if"`
				ContinueOnError bool   `json:"continue-on-error"`
				Run             string `json:"run"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := yaml.NewYAMLOrJSONDecoder(f, 4096).Decode(&workflow); err != nil {
		t.Fatal(err)
	}
	required := map[string]bool{
		"Run TestWindowsNode":             false,
		"Copy results from VM":            false,
		"Collect Windows node VM metrics": false,
		"Generate Gopogh HTML Report":     false,
		"Cleanup VM":                      false,
	}
	for _, step := range workflow.Jobs["windows-node-selfhosted"].Steps {
		if _, ok := required[step.Name]; !ok {
			continue
		}
		required[step.Name] = true
		if step.Name == "Run TestWindowsNode" {
			if step.ContinueOnError {
				t.Error("test process failures must fail the CI job, including parent-test cleanup failures")
			}
			if !strings.Contains(step.Run, `"-test.timeout=120m"`) {
				t.Error("Windows test process must allow image preparation and cleanup within 120 minutes")
			}
		} else if !strings.HasPrefix(step.If, "always()") {
			t.Errorf("%s must still run after the test step fails", step.Name)
		}
	}
	for name, found := range required {
		if !found {
			t.Errorf("missing workflow step %q", name)
		}
	}
}

func TestWindowsCleanupDiagnosticsScript(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires Windows PowerShell")
	}
	scriptPath, err := filepath.Abs(filepath.Join("testdata", "windows-node-cleanup-diagnostics.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		vmName     string
		path       string
		vmError    bool
		eventError bool
		noEvents   bool
		want       string
		wantError  bool
	}{
		{name: "removed", want: "Verified removal"},
		{name: "Linux VM remains", vmName: "cleanup-unit-test", want: "VM: cleanup-unit-test", wantError: true},
		{name: "Windows VM remains", vmName: "cleanup-unit-test-m02", want: "VM: cleanup-unit-test-m02", wantError: true},
		{name: "profile remains", path: filepath.Join("profiles", "cleanup-unit-test"), want: "Cleanup left resources: Path:", wantError: true},
		{name: "Linux machine remains", path: filepath.Join("machines", "cleanup-unit-test"), want: "Cleanup left resources: Path:", wantError: true},
		{name: "Windows machine remains", path: filepath.Join("machines", "cleanup-unit-test-m02"), want: "Cleanup left resources: Path:", wantError: true},
		{name: "inventory fails", vmError: true, want: "Cannot verify VM removal", wantError: true},
		{name: "unrelated VM", vmName: "cleanup-unit-test-other", want: "Verified removal"},
		{name: "events unavailable", eventError: true, want: "Hyper-V event diagnostics failed"},
		{name: "no recent events", noEvents: true, want: "No recent events"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if tc.path != "" {
				if err := os.MkdirAll(filepath.Join(home, tc.path), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			script := fmt.Sprintf(`
		$ErrorActionPreference = 'Stop'
		function Get-VM {
		    param($ErrorAction)
		    if ($%t) { throw 'mock VM query failure' }
		    if ('%s') {
		        [pscustomobject]@{ Name = '%s'; Id = [guid]'11111111-1111-1111-1111-111111111111'; State = 'Stopping'; AutomaticCheckpointsEnabled = $true }
		    }
		}
		function Get-VMSnapshot {
		    param($VM, $ErrorAction)
		    [pscustomobject]@{ Name = 'mock-checkpoint'; SnapshotType = 'Standard' }
		}
		function Get-VMHardDiskDrive {
		    param($VM, $ErrorAction)
		    [pscustomobject]@{ Path = 'mock-disk.avhdx' }
		}
		function Get-WinEvent {
		    param($FilterHashtable, $MaxEvents, $ErrorAction)
		    if ($%t) { throw 'mock event access failure' }
		    if ($%t) {
		        throw [System.Management.Automation.ErrorRecord]::new(
		            [Exception]::new('no events'), 'NoMatchingEventsFound', 'ObjectNotFound', $null)
		    }
		    if ($MaxEvents -ne 100 -or $FilterHashtable.StartTime -ne ([DateTimeOffset]'2026-10-21T12:00:00Z').UtcDateTime) {
		        throw 'unexpected event filter'
		    }
		    [pscustomobject]@{ Id = 1; Message = 'cleanup-unit-test event fixture' }
		    [pscustomobject]@{ Id = 2; Message = 'unrelated event fixture' }
		}
		& '%s' -Profile cleanup-unit-test -MinikubeHome '%s' -Since '2026-10-21T12:00:00Z' -VerifyDeleted
		`, tc.vmError, tc.vmName, tc.vmName, tc.eventError, tc.noEvents, strings.ReplaceAll(scriptPath, "'", "''"), strings.ReplaceAll(home, "'", "''"))
			output, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
			if (err != nil) != tc.wantError || !strings.Contains(string(output), tc.want) {
				t.Fatalf("cleanup diagnostics: %v\n%s", err, output)
			}
			if strings.Contains(string(output), "unrelated event fixture") {
				t.Error("cleanup diagnostics must filter unrelated events")
			}
			if !tc.eventError && !tc.noEvents && !strings.Contains(string(output), "cleanup-unit-test event fixture") {
				t.Errorf("cleanup diagnostics must retain profile events:\n%s", output)
			}
			if tc.vmName == "cleanup-unit-test" || tc.vmName == "cleanup-unit-test-m02" {
				if !strings.Contains(string(output), "mock-checkpoint") || !strings.Contains(string(output), "mock-disk.avhdx") {
					t.Error("remaining VM diagnostics must include checkpoints and attached disks")
				}
			}
		})
	}
}
func TestWindowsDiagnosticsScript(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires Windows PowerShell")
	}
	logPath := filepath.Join(t.TempDir(), "containerd log.txt")
	if err := os.WriteFile(logPath, []byte("native-log-fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scriptPath, err := filepath.Abs(filepath.Join("testdata", "windows-node-diagnostics.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args string
		path string
		want string
	}{
		{"baseline", "", logPath, `"processors"`},
		{"logs", "-IncludeLogs -Since '2026-10-20T14:35:00Z'", logPath, "native-log-fixture"},
		{"missing log", "-IncludeLogs", logPath + ".missing", "Windows runtime log is unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := fmt.Sprintf(`
	$ErrorActionPreference = 'Stop'
	$logPath = '%s'
	function Get-CimInstance {
	    param($ClassName, $Filter)
	    switch ($ClassName) {
	        'Win32_Processor' { [pscustomobject]@{ Name = 'Test CPU'; NumberOfLogicalProcessors = 2; MaxClockSpeed = 2000 } }
	        'Win32_OperatingSystem' { [pscustomobject]@{ TotalVisibleMemorySize = 6000000; FreePhysicalMemory = 4000000 } }
	        'Win32_Service' { [pscustomobject]@{ Name = 'containerd'; State = 'Running'; PathName = 'containerd.exe --log-file "' + $logPath + '"' } }
	        default { throw "Unexpected CIM class: $ClassName" }
	    }
	}
	function Get-Process {
	    [pscustomobject]@{ Id = 1; ProcessName = 'containerd'; CPU = 1; StartTime = [datetime]'2026-10-20'; WorkingSet64 = 1000 }
	}
	function Get-ItemProperty {
	    param($LiteralPath)
	    [pscustomobject]@{ AppStdout = $logPath; AppStderr = $logPath }
	}
	& '%s' %s
	`, strings.ReplaceAll(tc.path, "'", "''"), strings.ReplaceAll(scriptPath, "'", "''"), tc.args)
			output, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
			if err != nil || !strings.Contains(string(output), tc.want) {
				t.Fatalf("diagnostics script: %v\n%s", err, output)
			}
			if tc.name == "baseline" && strings.Contains(string(output), "native-log-fixture") {
				t.Error("baseline diagnostics must not read native logs")
			}
			if tc.name == "logs" && !strings.Contains(string(output), "Test started at 2026-10-20T14:35:00.0000000Z") {
				t.Error("native logs must identify the current test's UTC start time")
			}
		})
	}
}
