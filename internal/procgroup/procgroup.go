// Package procgroup hides the platform differences involved in running a
// child process together with everything it spawns, and later stopping that
// whole tree. Astrion's local execution provider and the supervisor both use
// it so that cancelling a job never leaves an orphaned agent CLI behind.
//
// Unix: the child leads a new process group; signals go to the group.
// Windows: the child gets a new process group; the tree is stopped with
// taskkill /T. Windows has no portable graceful-stop signal for console
// processes, so Terminate is immediately forceful there.
package procgroup

import (
	"os"
	"os/exec"
	"path/filepath"
)

// Writable reports whether the current user can create files in dir.
func Writable(dir string) error {
	f, err := os.CreateTemp(dir, ".astrion-probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// BaseEnv returns the minimal environment for a child process: PATH plus a
// private home and temp directory, plus the variables the platform needs for
// basic operation (on Windows, networking and crypto fail without
// SystemRoot). Nothing else from the parent environment is inherited.
func BaseEnv(home, tmp string) []string {
	env := []string{"PATH=" + os.Getenv("PATH")}
	if home != "" {
		env = append(env, homeEnv(filepath.Clean(home))...)
	}
	if tmp != "" {
		env = append(env, tmpEnv(filepath.Clean(tmp))...)
	}
	return append(env, platformEnv()...)
}

// Start configures cmd to run in its own process group and starts it.
func Start(cmd *exec.Cmd) error {
	setGroup(cmd)
	return cmd.Start()
}
