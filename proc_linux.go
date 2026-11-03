//go:build linux

package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type platProc struct{ pgid int }

func platPrepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
}

func platAttach(cmd *exec.Cmd) *platProc { return &platProc{pgid: cmd.Process.Pid} }
func platDone(*platProc)                 {}

func softKill(cmd *exec.Cmd, pl *platProc) {
	if pl != nil {
		syscall.Kill(-pl.pgid, syscall.SIGTERM)
	} else {
		cmd.Process.Signal(syscall.SIGTERM)
	}
}

func hardKill(cmd *exec.Cmd, pl *platProc) {
	if pl != nil {
		syscall.Kill(-pl.pgid, syscall.SIGKILL)
	}
	cmd.Process.Kill()
}

const clkTck = 100.0

// arborele de procese (modulul + ffmpeg-urile pornite de el)
func procTree(root int) []int {
	ents, _ := os.ReadDir("/proc")
	parent := map[int]int{}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:])
		if len(f) > 2 {
			pp, _ := strconv.Atoi(f[1])
			parent[pid] = pp
		}
	}
	out := []int{root}
	for changed := true; changed; {
		changed = false
		in := map[int]bool{}
		for _, p := range out {
			in[p] = true
		}
		for pid, pp := range parent {
			if in[pp] && !in[pid] {
				out = append(out, pid)
				changed = true
			}
		}
	}
	return out
}

func procStats(pid int, _ *platProc) (cpuSec float64, mem uint64) {
	page := uint64(os.Getpagesize())
	for _, p := range procTree(pid) {
		ps := strconv.Itoa(p)
		if b, err := os.ReadFile("/proc/" + ps + "/stat"); err == nil {
			s := string(b)
			if i := strings.LastIndexByte(s, ')'); i > 0 {
				f := strings.Fields(s[i+1:])
				if len(f) > 13 {
					u, _ := strconv.ParseFloat(f[11], 64)
					k, _ := strconv.ParseFloat(f[12], 64)
					cpuSec += (u + k) / clkTck
				}
			}
		}
		if b, err := os.ReadFile("/proc/" + ps + "/statm"); err == nil {
			f := strings.Fields(string(b))
			if len(f) > 1 {
				r, _ := strconv.ParseUint(f[1], 10, 64)
				mem += r * page
			}
		}
	}
	return
}

var (
	hostMu                    sync.Mutex
	hostCPU                   float64
	hostMemTotal, hostMemUsed uint64
	prevIdle, prevTotal       float64
	_                         = time.Now
)

func sampleHost() {
	if b, err := os.ReadFile("/proc/stat"); err == nil {
		line := strings.SplitN(string(b), "\n", 2)[0]
		f := strings.Fields(line)
		var tot, idle float64
		for i, v := range f[1:] {
			x, _ := strconv.ParseFloat(v, 64)
			tot += x
			if i == 3 || i == 4 {
				idle += x
			}
		}
		hostMu.Lock()
		if prevTotal > 0 && tot > prevTotal {
			hostCPU = (1 - (idle-prevIdle)/(tot-prevTotal)) * 100
		}
		prevIdle, prevTotal = idle, tot
		hostMu.Unlock()
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		var tot, avail uint64
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Fields(l)
			if len(f) < 2 {
				continue
			}
			v, _ := strconv.ParseUint(f[1], 10, 64)
			switch f[0] {
			case "MemTotal:":
				tot = v * 1024
			case "MemAvailable:":
				avail = v * 1024
			}
		}
		hostMu.Lock()
		hostMemTotal, hostMemUsed = tot, tot-avail
		hostMu.Unlock()
	}
}

func hostStats() (float64, uint64, uint64) {
	hostMu.Lock()
	defer hostMu.Unlock()
	return hostCPU, hostMemUsed, hostMemTotal
}

func firewallSetup()     {}
func openBrowser(string) {}
