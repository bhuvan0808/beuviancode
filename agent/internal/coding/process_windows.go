//go:build windows

package coding

import (
	"os"
	"os/exec"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// configureProcessGroup puts the child in its own process group.
//
// CREATE_NEW_PROCESS_GROUP is what makes it possible to terminate the coding agent
// AND anything it spawned. Claude Code runs build tools, test runners, and package
// managers; without a group, killing only the parent leaves those children running
// and holding file locks on the user's repository.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP,
	}
}

// terminateGracefully asks the process to exit.
//
// Windows has no SIGTERM. CTRL_BREAK_EVENT is the closest equivalent for a console
// process and is deliverable to a process group, which is why the group was created
// above. A console program that installs a handler gets to clean up; one that does
// not is terminated by the caller's escalation path.
func terminateGracefully(proc *os.Process) error {
	// The signal goes to the group, whose ID equals the lead process's PID.
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(proc.Pid))
}

// killProcessGroup forcibly terminates the process and its descendants.
//
// TerminateProcess on the parent alone would orphan children. Enumerating the
// tree via a process snapshot is the reliable way to reach them on Windows, since
// there is no equivalent of killing a negative PID.
func killProcessGroup(proc *os.Process) error {
	pids := descendantPIDs(uint32(proc.Pid))

	// Children first, so a parent cannot spawn a replacement while we work down
	// the tree.
	for i := len(pids) - 1; i >= 0; i-- {
		terminatePID(pids[i])
	}
	return proc.Kill()
}

// descendantPIDs returns the PIDs below root, breadth-first.
func descendantPIDs(root uint32) []uint32 {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()

	// One pass to build the parent map, then walk it. Re-scanning per level would
	// be O(n*depth) over every process on the machine.
	children := map[uint32][]uint32{}

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return nil
	}
	for {
		children[entry.ParentProcessID] = append(children[entry.ParentProcessID], entry.ProcessID)
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			break
		}
	}

	var out []uint32
	queue := []uint32{root}
	// Bounded so a PID-reuse cycle in the parent map cannot loop forever.
	for len(queue) > 0 && len(out) < 512 {
		current := queue[0]
		queue = queue[1:]
		for _, child := range children[current] {
			out = append(out, child)
			queue = append(queue, child)
		}
	}
	return out
}

func terminatePID(pid uint32) {
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	_ = windows.TerminateProcess(handle, 1)
}

// processMemoryCounters mirrors the Win32 PROCESS_MEMORY_COUNTERS structure.
//
// Declared here because golang.org/x/sys/windows does not expose it. SIZE_T is
// pointer-width, so uintptr is the correct Go type — using uint32 would silently
// misread every field after the first on 64-bit Windows.
type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

var (
	// K32GetProcessMemoryInfo lives in kernel32 on Windows 7+ and avoids the
	// psapi.dll versioning mess entirely.
	kernel32                    = windows.NewLazySystemDLL("kernel32.dll")
	procGetProcessMemoryInfoK32 = kernel32.NewProc("K32GetProcessMemoryInfo")
)

// getProcessMemoryInfo fills counters for the given process handle.
func getProcessMemoryInfo(handle windows.Handle, counters *processMemoryCounters) error {
	counters.cb = uint32(unsafe.Sizeof(*counters))
	ret, _, err := procGetProcessMemoryInfoK32.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(counters)),
		uintptr(counters.cb),
	)
	if ret == 0 {
		return err
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
	at     time.Time
	kernel uint64
	user   uint64
}

var lastCPU = map[int]cpuSample{}

// processUsage samples CPU and memory for a PID.
//
// Returns an error rather than zeroes when the process is gone, so the caller can
// omit the fields instead of reporting a confident 0% for a dead process.
func processUsage(pid int) (usage, error) {
	if pid == 0 {
		return usage{}, os.ErrProcessDone
	}

	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return usage{}, err
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	var mem processMemoryCounters
	if err := getProcessMemoryInfo(handle, &mem); err != nil {
		return usage{}, err
	}

	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		// Memory is still useful even without a CPU figure, so report what we have
		// rather than failing the whole sample.
		return usage{memoryBytes: uint64(mem.workingSetSize)}, nil
	}

	now := time.Now()
	kernelTicks := filetimeTo100ns(kernel)
	userTicks := filetimeTo100ns(user)

	out := usage{memoryBytes: uint64(mem.workingSetSize)}

	// A percentage needs two samples. The first call establishes a baseline and
	// reports 0, which is honest: we do not yet know the rate.
	if prev, ok := lastCPU[pid]; ok {
		elapsed := now.Sub(prev.at).Seconds()
		if elapsed > 0 {
			// FILETIME ticks are 100ns units.
			busy := float64((kernelTicks-prev.kernel)+(userTicks-prev.user)) / 1e7
			out.cpuPercent = (busy / elapsed) * 100
			if out.cpuPercent < 0 {
				out.cpuPercent = 0
			}
		}
	}
	lastCPU[pid] = cpuSample{at: now, kernel: kernelTicks, user: userTicks}

	return out, nil
}

func filetimeTo100ns(ft windows.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}
