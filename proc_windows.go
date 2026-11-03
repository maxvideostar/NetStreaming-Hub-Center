//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type platProc struct {
	job windows.Handle
}

const createNoWindow = 0x08000000

func platPrepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true,
		CreationFlags: createNoWindow | syscall.CREATE_NEW_PROCESS_GROUP}
}

// job object: la oprire/închiderea suitei se închid și ffmpeg-urile pornite de modul
func platAttach(cmd *exec.Cmd) *platProc {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return &platProc{}
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err == nil {
		windows.AssignProcessToJobObject(job, h)
		windows.CloseHandle(h)
	}
	return &platProc{job: job}
}

func platDone(pl *platProc) {
	if pl != nil && pl.job != 0 {
		windows.CloseHandle(pl.job) // KILL_ON_JOB_CLOSE -> curăță eventualele ffmpeg rămase
		pl.job = 0
	}
}

func softKill(cmd *exec.Cmd, pl *platProc) {
	if pl != nil && pl.job != 0 {
		windows.TerminateJobObject(pl.job, 1)
		return
	}
	cmd.Process.Kill()
}

func hardKill(cmd *exec.Cmd, pl *platProc) { softKill(cmd, pl); cmd.Process.Kill() }

type jobAccounting struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

type pmc struct {
	cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

var (
	k32                = windows.NewLazySystemDLL("kernel32.dll")
	pGetProcMemInfo    = k32.NewProc("K32GetProcessMemoryInfo")
	pGetSystemTimes    = k32.NewProc("GetSystemTimes")
	pGlobalMemStatusEx = k32.NewProc("GlobalMemoryStatusEx")
)

func procTree(root uint32) []uint32 {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return []uint32{root}
	}
	defer windows.CloseHandle(snap)
	parent := map[uint32]uint32{}
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		parent[e.ProcessID] = e.ParentProcessID
	}
	out := []uint32{root}
	for changed := true; changed; {
		changed = false
		in := map[uint32]bool{}
		for _, p := range out {
			in[p] = true
		}
		for pid, pp := range parent {
			if in[pp] && !in[pid] && pid != 0 {
				out = append(out, pid)
				changed = true
			}
		}
	}
	return out
}

func procStats(pid int, pl *platProc) (cpuSec float64, mem uint64) {
	if pl != nil && pl.job != 0 {
		var a jobAccounting
		if windows.QueryInformationJobObject(pl.job, windows.JobObjectBasicAccountingInformation,
			uintptr(unsafe.Pointer(&a)), uint32(unsafe.Sizeof(a)), nil) == nil {
			cpuSec = float64(a.TotalUserTime+a.TotalKernelTime) / 1e7
		}
	}
	for _, p := range procTree(uint32(pid)) {
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_VM_READ, false, p)
		if err != nil {
			h, err = windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, p)
			if err != nil {
				continue
			}
		}
		var c pmc
		c.cb = uint32(unsafe.Sizeof(c))
		if r, _, _ := pGetProcMemInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&c)), uintptr(c.cb)); r != 0 {
			mem += uint64(c.WorkingSetSize)
		}
		windows.CloseHandle(h)
	}
	return
}

var (
	hostMu                    sync.Mutex
	hostCPU                   float64
	hostMemTotal, hostMemUsed uint64
	prevIdle, prevTotal       uint64
)

type memStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func ft(f windows.Filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }

func sampleHost() {
	var idle, kern, user windows.Filetime
	if r, _, _ := pGetSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kern)), uintptr(unsafe.Pointer(&user))); r != 0 {
		i, t := ft(idle), ft(kern)+ft(user) // kernel include idle
		hostMu.Lock()
		if prevTotal > 0 && t > prevTotal {
			hostCPU = (1 - float64(i-prevIdle)/float64(t-prevTotal)) * 100
		}
		prevIdle, prevTotal = i, t
		hostMu.Unlock()
	}
	var m memStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	if r, _, _ := pGlobalMemStatusEx.Call(uintptr(unsafe.Pointer(&m))); r != 0 {
		hostMu.Lock()
		hostMemTotal, hostMemUsed = m.TotalPhys, m.TotalPhys-m.AvailPhys
		hostMu.Unlock()
	}
}

func hostStats() (float64, uint64, uint64) {
	hostMu.Lock()
	defer hostMu.Unlock()
	return hostCPU, hostMemUsed, hostMemTotal
}

// reguli firewall pentru suită + toate modulele (porturi pe program, orice profil)
func firewallSetup() {
	exe, _ := os.Executable()
	progs := map[string]string{"nTs-Suite": exe}
	for _, m := range modules {
		progs["nTs-Suite "+m.Name] = m.BinPath()
	}
	progs["nTs-Suite ffmpeg"] = filepath.Join(dataDir, "bin", "ffmpeg.exe")
	for name, prog := range progs {
		run("netsh", "advfirewall", "firewall", "delete", "rule", "name="+name)
		for _, proto := range []string{"TCP", "UDP"} {
			run("netsh", "advfirewall", "firewall", "add", "rule", "name="+name, "dir=in", "action=allow",
				"program="+prog, "protocol="+proto, "profile=any", "enable=yes")
		}
	}
}

func run(name string, args ...string) {
	c := exec.Command(name, args...)
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	c.Run()
}

func openBrowser(url string) {
	exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}
