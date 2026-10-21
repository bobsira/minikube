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
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
		t.Fatal("deployment progress deadline must allow the full 25-minute cold-start budget")
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
}

func TestWindowsCleanup(t *testing.T) {
	const scenarioEnv = "MINIKUBE_TEST_WINDOWS_CLEANUP"
	if scenario := os.Getenv(scenarioEnv); scenario != "" {
		*binaryPath = filepath.Join(t.TempDir(), "nonexistent-minikube")
		*cleanup = scenario != "disabled"
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
				return
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || !strings.Contains(string(output), "failed cleanup") {
				t.Fatalf("cleanup failure must fail the test process: %v\n%s", err, output)
			}
		})
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
