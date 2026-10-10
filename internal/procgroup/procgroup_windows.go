//go:build windows

package procgroup

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

const (
	processQueryLimitedInformation = 0x1000
	stillActive                    = 259
)

func setGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

func taskkill(pid int) error {
	if pid <= 0 || !Alive(pid) {
		return nil
	}
	// /T stops the whole tree rooted at pid; /F is required for console
	// processes, which ignore the polite WM_CLOSE request.
	return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
}

// Terminate stops the process tree rooted at pid. Windows offers no graceful
// equivalent of SIGTERM for console processes, so this is forceful.
func Terminate(pid int) error { return taskkill(pid) }

// Kill forcefully stops the process tree rooted at pid.
func Kill(pid int) error { return taskkill(pid) }

// Alive reports whether a process with this pid is still running.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		// Access denied means the process exists but belongs to someone else.
		return err == syscall.ERROR_ACCESS_DENIED
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

func homeEnv(home string) []string {
	return []string{"HOME=" + home, "USERPROFILE=" + home, "APPDATA=" + home, "LOCALAPPDATA=" + home}
}

func tmpEnv(tmp string) []string { return []string{"TEMP=" + tmp, "TMP=" + tmp} }

func platformEnv() []string {
	var env []string
	for _, k := range []string{"SystemRoot", "SystemDrive", "windir", "ComSpec", "PATHEXT", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}
