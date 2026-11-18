//go:build darwin

package provider

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func seatbeltProcessGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func seatbeltKill(ctx context.Context, i *seatbeltInstance) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Verify the live leader identity before using a persisted PID: a recycled
	// PID must never authorize termination of an unrelated process group.
	group := 0
	killed := make(map[int]bool)
	if i.cmd != nil {
		pid := i.cmd.Process.Pid
		identity, err := exec.CommandContext(ctx, "/bin/ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
		if err == nil && strings.Contains(string(identity), i.dir+"/run.sh") {
			group = pid
			if err = syscall.Kill(-pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
				return err
			}
		}
	}
	out, err := exec.CommandContext(ctx, "/bin/ps", "eww", "-axo", "pid=,command=").Output()
	if err != nil {
		return fmt.Errorf("inventory job processes: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		found := false
		for _, field := range fields[1:] {
			if field == i.marker {
				found = true
				break
			}
		}
		if found {
			pid, e := strconv.Atoi(fields[0])
			if e == nil {
				killed[pid] = true
				if e = syscall.Kill(pid, syscall.SIGKILL); e != nil && e != syscall.ESRCH {
					return e
				}
			}
		}
	}
	// Wait until every observed member is gone or a zombie (which cannot write).
	// A leader can exit before its children, so inspect the whole process group.
	for {
		out, err := exec.CommandContext(ctx, "/bin/ps", "-axo", "pid=,pgid=,stat=").Output()
		if err != nil {
			return err
		}
		alive := false
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Fields(line)
			if len(f) != 3 {
				continue
			}
			pid, _ := strconv.Atoi(f[0])
			pgid, _ := strconv.Atoi(f[1])
			if ((group > 0 && pgid == group) || killed[pid]) && !strings.HasPrefix(f[2], "Z") {
				alive = true
			}
		}
		if !alive {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
