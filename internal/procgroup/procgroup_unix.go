//go:build !windows

package procgroup

import (
	"errors"
	"os/exec"
	"syscall"
)

func setGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// Terminate asks the process group led by pid to stop (SIGTERM).
func Terminate(pid int) error { return syscall.Kill(-pid, syscall.SIGTERM) }

// Kill forcefully stops the process group led by pid (SIGKILL).
func Kill(pid int) error { return syscall.Kill(-pid, syscall.SIGKILL) }

// Alive reports whether a process with this pid exists.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func homeEnv(home string) []string { return []string{"HOME=" + home} }
func tmpEnv(tmp string) []string   { return []string{"TMPDIR=" + tmp} }
func platformEnv() []string        { return []string{"LANG=C.UTF-8"} }
