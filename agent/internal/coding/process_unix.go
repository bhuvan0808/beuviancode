//go:build !windows

package coding

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// configureProcessGroup puts the child in its own process group.
//
// Setpgid is what makes it possible to signal the coding agent AND everything it
// spawned. Claude Code runs build tools, test runners, and package managers;
// signalling only the parent leaves those children running and holding file locks
// on the user's repository.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateGracefully sends SIGTERM to the whole process group.
//
// A negative PID addresses the group, which is the entire reason Setpgid was set
// above. SIGTERM gives a well-behaved program the chance to flush and exit
// cleanly; escalation to SIGKILL is the caller's job if it does not.
func terminateGracefully(proc *os.Process) error {
	if err := syscall.Kill(-proc.Pid, syscall.SIGTERM); err != nil {
		// The group may not exist if the process already exited between the
		// caller's check and this call. Fall back to the process itself.
		return proc.Signal(syscall.SIGTERM)
	}
	return nil
}

// killProcessGroup forcibly terminates the process and its descendants.
func killProcessGroup(proc *os.Process) error {
	if err := syscall.Kill(-proc.Pid, syscall.SIGKILL); err != nil {
		return proc.Kill()
	}
	return nil
}

// usage is a resource sample for the dashboard readouts.
type usage struct {
	cpuPercent  float64
	memoryBytes uint64
}

// cpuSample remembers the previous reading, since CPU percentage is a rate and
// cannot be derived from a single point in time.
type cpuSample struct {
	at    time.Time
	ticks uint64
}

var lastCPU = map[int]cpuSample{}

// clockTicks is the kernel's USER_HZ. 100 on effectively every Linux system;
// hardcoded because reading it properly needs cgo, which would end the agent's
// cross-compilation.
const clockTicks = 100

// processUsage samples CPU and memory for a PID.
//
// Reads /proc on Linux. On macOS /proc does not exist, so this reports memory only
// via a bounded `ps` call — the alternative is cgo and libproc, which would cost
// cross-compilation for a number shown in a dashboard readout.
func processUsage(pid int) (usage, error) {
	if pid == 0 {
		return usage{}, os.ErrProcessDone
	}
	if stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		return parseProcStat(pid, string(stat))
	}
	return usageFromPS(pid)
}

// parseProcStat reads utime, stime, and RSS from a /proc/<pid>/stat line.
func parseProcStat(pid int, stat string) (usage, error) {
	// The comm field is parenthesised and may itself contain spaces or brackets,
	// so fields are counted from after the LAST ')' rather than by splitting the
	// whole line — a process named "(my app)" breaks the naive approach.
	close := strings.LastIndex(stat, ")")
	if close < 0 || close+2 >= len(stat) {
		return usage{}, os.ErrInvalid
	}
	fields := strings.Fields(stat[close+2:])
	// Indices are relative to the post-comm slice: state is [0], so utime (14th
	// field overall) is [11] and stime is [12].
	const (
		utimeIdx = 11
		stimeIdx = 12
		rssIdx   = 21
	)
	if len(fields) <= rssIdx {
		return usage{}, os.ErrInvalid
	}

	utime, _ := strconv.ParseUint(fields[utimeIdx], 10, 64)
	stime, _ := strconv.ParseUint(fields[stimeIdx], 10, 64)
	rssPages, _ := strconv.ParseUint(fields[rssIdx], 10, 64)

	out := usage{memoryBytes: rssPages * uint64(os.Getpagesize())}

	now := time.Now()
	total := utime + stime
	// A percentage needs two samples; the first establishes a baseline and
	// honestly reports 0 rather than guessing.
	if prev, ok := lastCPU[pid]; ok {
		if elapsed := now.Sub(prev.at).Seconds(); elapsed > 0 {
			busy := float64(total-prev.ticks) / clockTicks
			out.cpuPercent = (busy / elapsed) * 100
			if out.cpuPercent < 0 {
				out.cpuPercent = 0
			}
		}
	}
	lastCPU[pid] = cpuSample{at: now, ticks: total}

	return out, nil
}

// usageFromPS is the macOS path.
func usageFromPS(pid int) (usage, error) {
	// Bounded: this runs on the status path, and a hung ps must not stall the
	// heartbeat.
	cmd := exec.Command("ps", "-o", "%cpu=,rss=", "-p", strconv.Itoa(pid))
	out, err := cmd.Output()
	if err != nil {
		return usage{}, err
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return usage{}, os.ErrInvalid
	}
	cpu, _ := strconv.ParseFloat(fields[0], 64)
	rssKB, _ := strconv.ParseUint(fields[1], 10, 64)
	return usage{cpuPercent: cpu, memoryBytes: rssKB * 1024}, nil
}
