package procgroup

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWritable(t *testing.T) {
	if err := Writable(t.TempDir()); err != nil {
		t.Fatalf("temp dir should be writable: %v", err)
	}
	if err := Writable("/definitely/does/not/exist"); err == nil {
		t.Fatal("missing dir must not be writable")
	}
}

func TestBaseEnvIsMinimal(t *testing.T) {
	t.Setenv("ASTRION_SHOULD_NOT_LEAK", "secret")
	env := BaseEnv(t.TempDir(), t.TempDir())
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "ASTRION_SHOULD_NOT_LEAK") {
		t.Fatal("parent environment leaked into child env")
	}
	if !strings.Contains(joined, "PATH=") || !strings.Contains(joined, "HOME=") {
		t.Fatalf("missing PATH/HOME: %v", env)
	}
}

// TestKillStopsWholeGroup starts a shell that spawns a long-running child and
// verifies Kill stops both, i.e. no orphan survives cancellation.
func TestKillStopsWholeGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	pidFile := t.TempDir() + "/child.pid"
	cmd := exec.Command("/bin/sh", "-c", "sleep 60 & echo $! > "+pidFile+"; wait")
	if err := Start(cmd); err != nil {
		t.Fatal(err)
	}
	var child int
	for i := 0; i < 50 && child == 0; i++ {
		if b, err := os.ReadFile(pidFile); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			child, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if child == 0 || !Alive(child) {
		t.Fatal("child did not start")
	}
	if err := Kill(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	for i := 0; i < 50 && Alive(child); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	// A killed-but-unreaped child of a dead shell is reparented and reaped by init.
	if Alive(child) {
		if b, _ := os.ReadFile("/proc/" + strconv.Itoa(child) + "/stat"); !strings.Contains(string(b), ") Z") {
			t.Fatalf("grandchild %d survived Kill", child)
		}
	}
}
