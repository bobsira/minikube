package node

import (
	"bytes"
	"errors"
	"os/exec"
	"strings"

	"k8s.io/klog/v2"
)

var powershell string

var (
	ErrPowerShellNotFound = errors.New("powershell was not found in the path")
	ErrNotAdministrator   = errors.New("hyper-v commands have to be run as an Administrator")
	ErrNotInstalled       = errors.New("hyper-V PowerShell Module is not available")
)

func init() {
	powershell, _ = exec.LookPath("powershell.exe")
}

func cmdOut(args ...string) (string, error) {
	args = append([]string{"-NoProfile", "-NonInteractive"}, args...)
	cmd := exec.Command(powershell, args...)
	klog.Infof("[executing ==>] : %v %v", powershell, strings.Join(args, " "))
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	klog.Infof("[stdout =====>] : %s", stdout.String())
	klog.Infof("[stderr =====>] : %s", stderr.String())
	return stdout.String(), err
}

func cmd(args ...string) error {
	_, err := cmdOut(args...)
	return err
}
