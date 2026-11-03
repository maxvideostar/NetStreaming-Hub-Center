package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---------------- jurnal circular ----------------

type Ring struct {
	mu    sync.Mutex
	lines []string
	part  []byte
	max   int
	file  *os.File
	fpath string
	fsize int64
}

func newRing(max int, path string) *Ring {
	r := &Ring{max: max, fpath: path}
	r.openFile()
	return r
}

func (r *Ring) openFile() {
	if r.fpath == "" {
		return
	}
	os.MkdirAll(filepath.Dir(r.fpath), 0755)
	if fi, err := os.Stat(r.fpath); err == nil && fi.Size() > 10<<20 {
		os.Rename(r.fpath, r.fpath+".old")
	}
	f, err := os.OpenFile(r.fpath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err == nil {
		r.file = f
		if fi, err := f.Stat(); err == nil {
			r.fsize = fi.Size()
		}
	}
}

func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ts := time.Now().Format("2006-01-02 15:04:05 ")
	r.part = append(r.part, p...)
	for {
		i := bytes.IndexAny(r.part, "\n\r")
		if i < 0 {
			if len(r.part) > 8192 {
				i = len(r.part)
			} else {
				break
			}
		}
		line := strings.TrimRight(string(r.part[:i]), "\r\n")
		if i < len(r.part) {
			r.part = r.part[i+1:]
		} else {
			r.part = r.part[:0]
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		l := ts + line
		r.lines = append(r.lines, l)
		if len(r.lines) > r.max {
			r.lines = r.lines[len(r.lines)-r.max:]
		}
		if r.file != nil {
			n, _ := r.file.WriteString(l + "\n")
			r.fsize += int64(n)
			if r.fsize > 10<<20 {
				r.file.Close()
				r.file = nil
				r.openFile()
			}
		}
	}
	return len(p), nil
}

func (r *Ring) Tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > len(r.lines) {
		n = len(r.lines)
	}
	out := make([]string, n)
	copy(out, r.lines[len(r.lines)-n:])
	return out
}

// ---------------- proces supravegheat ----------------

type Proc struct {
	mod  *Module
	logs *Ring

	mu       sync.Mutex
	want     bool
	active   bool
	cmd      *exec.Cmd
	plat     *platProc
	state    string
	pid      int
	started  time.Time
	restarts int
	lastErr  string
	stopSig  chan struct{}
	done     chan struct{}

	// statistici
	cpu   float64
	mem   uint64
	cpuT  float64
	cpuAt time.Time
}

var procs = map[string]*Proc{}

func initProcs() {
	for _, m := range modules {
		procs[m.ID] = &Proc{mod: m, state: "oprit",
			logs: newRing(3000, filepath.Join(dataDir, "logs", m.ID+".log"))}
	}
}

func (p *Proc) Wanted() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.want
}

func (p *Proc) logf(f string, a ...any) {
	fmt.Fprintf(p.logs, "[suite] "+f+"\n", a...)
}

var startMu sync.Mutex

func (p *Proc) Start() error {
	startMu.Lock()
	defer startMu.Unlock()
	p.mu.Lock()
	act := p.active
	p.mu.Unlock()
	if act {
		p.mu.Lock()
		p.want = true
		p.mu.Unlock()
		return nil
	}
	os.MkdirAll(p.mod.Dir(), 0755)
	p.mod.prepare(p.mod) // creează configurația implicită ca porturile să fie cunoscute
	if err := portConflict(p.mod); err != nil {
		p.mu.Lock()
		p.state, p.lastErr = "eroare", err.Error()
		p.mu.Unlock()
		p.logf("%v", err)
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.want, p.active = true, true
	p.stopSig = make(chan struct{})
	p.done = make(chan struct{})
	p.lastErr = ""
	go p.supervise(p.stopSig, p.done)
	return nil
}

func (p *Proc) Stop() {
	p.mu.Lock()
	if !p.active {
		p.want = false
		p.state, p.lastErr = "oprit", ""
		p.mu.Unlock()
		return
	}
	p.want = false
	close(p.stopSig)
	done, cmd, pl := p.done, p.cmd, p.plat
	p.state = "se oprește"
	p.mu.Unlock()
	if cmd != nil {
		softKill(cmd, pl)
		select {
		case <-done:
			return
		case <-time.After(6 * time.Second):
		}
		hardKill(cmd, pl)
	}
	select {
	case <-done:
	case <-time.After(8 * time.Second):
	}
}

func (p *Proc) Restart() {
	p.Stop()
	time.Sleep(300 * time.Millisecond)
	p.Start()
}

func (p *Proc) supervise(stop, done chan struct{}) {
	defer func() {
		p.mu.Lock()
		p.active, p.cmd, p.plat, p.pid = false, nil, nil, 0
		if !p.want || p.state != "eroare" {
			p.state = "oprit"
		}
		p.mu.Unlock()
		close(done)
	}()
	m := p.mod
	backoff := 2 * time.Second
	for {
		select {
		case <-stop:
			return
		default:
		}
		cmd, pl, err := p.spawn()
		if err != nil {
			p.mu.Lock()
			p.state, p.lastErr = "eroare", err.Error()
			p.mu.Unlock()
			p.logf("nu pot porni: %v", err)
		} else {
			start := time.Now()
			err = cmd.Wait()
			platDone(pl)
			m.clearCookies()
			p.mu.Lock()
			p.cmd, p.plat, p.pid = nil, nil, 0
			wanted := p.want
			p.mu.Unlock()
			if !wanted {
				p.logf("oprit")
				return
			}
			msg := "s-a închis"
			if err != nil {
				msg = err.Error()
			}
			p.logf("procesul s-a oprit neașteptat (%s) – repornesc în %s", msg, backoff)
			p.mu.Lock()
			p.state, p.lastErr = "repornește", msg
			p.restarts++
			p.mu.Unlock()
			if time.Since(start) > time.Minute {
				backoff = 2 * time.Second
			}
		}
		select {
		case <-stop:
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (p *Proc) spawn() (*exec.Cmd, *platProc, error) {
	m := p.mod
	if err := ensureBin(m); err != nil {
		return nil, nil, err
	}
	os.MkdirAll(m.Dir(), 0755)
	if err := m.prepare(m); err != nil {
		return nil, nil, fmt.Errorf("configurare: %v", err)
	}
	cmd := exec.Command(m.BinPath(), m.args(m)...)
	cmd.Dir = m.Dir()
	env := os.Environ()
	binDir := filepath.Join(dataDir, "bin")
	for i, e := range env {
		if strings.HasPrefix(strings.ToUpper(e), "PATH=") {
			env[i] = e[:5] + binDir + string(os.PathListSeparator) + e[5:]
		}
	}
	cmd.Env = append(env, m.env...)
	cmd.Stdout = p.logs
	cmd.Stderr = p.logs
	cmd.Stdin = nil
	platPrepare(cmd)
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	pl := platAttach(cmd)
	p.mu.Lock()
	p.cmd, p.plat, p.pid = cmd, pl, cmd.Process.Pid
	p.started = time.Now()
	p.state = "pornit"
	p.cpuT, p.cpuAt = 0, time.Time{}
	p.mu.Unlock()
	p.logf("pornit (PID %d) %s %s", cmd.Process.Pid, filepath.Base(m.BinPath()), strings.Join(m.args(m), " "))
	return cmd, pl, nil
}

// portConflict – același port folosit de alt modul pornit
func portConflict(m *Module) error {
	mine := map[string]bool{}
	for _, pt := range m.publicPorts(m) {
		mine[pt] = true
	}
	mine[m.panelPort(m)] = true
	sp := portOf(getCfg().Listen, "8090")
	for pt := range mine {
		if pt == sp {
			return fmt.Errorf("portul %s este folosit de nTs-Suite – schimbă-l în configurația %s", pt, m.Name)
		}
	}
	for _, o := range modules {
		if o == m {
			continue
		}
		op := procs[o.ID]
		op.mu.Lock()
		act := op.active
		op.mu.Unlock()
		if !act {
			continue
		}
		ports := append(o.publicPorts(o), o.panelPort(o))
		for _, pt := range ports {
			if mine[pt] {
				return fmt.Errorf("portul %s este deja folosit de %s – schimbă portul într-unul din module", pt, o.Name)
			}
		}
	}
	return nil
}

// ---------------- statistici ----------------

func statsLoop() {
	for {
		sampleHost()
		for _, m := range modules {
			p := procs[m.ID]
			p.mu.Lock()
			pid, pl := p.pid, p.plat
			p.mu.Unlock()
			if pid == 0 {
				p.mu.Lock()
				p.cpu, p.mem = 0, 0
				p.mu.Unlock()
				continue
			}
			cpuSec, mem := procStats(pid, pl)
			now := time.Now()
			p.mu.Lock()
			if !p.cpuAt.IsZero() && cpuSec >= p.cpuT {
				p.cpu = (cpuSec - p.cpuT) / now.Sub(p.cpuAt).Seconds() * 100
			}
			p.cpuT, p.cpuAt, p.mem = cpuSec, now, mem
			p.mu.Unlock()
		}
		time.Sleep(2 * time.Second)
	}
}

type ProcInfo struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Desc     string   `json:"desc"`
	Enabled  bool     `json:"enabled"`
	State    string   `json:"state"`
	PID      int      `json:"pid"`
	Uptime   int64    `json:"uptime"`
	Restarts int      `json:"restarts"`
	CPU      float64  `json:"cpu"`
	Mem      uint64   `json:"mem"`
	LastErr  string   `json:"last_err"`
	Ports    []string `json:"ports"`
	Panel    string   `json:"panel"`
	Bundled  bool     `json:"bundled"`
}

func (p *Proc) Info(enabled bool) ProcInfo {
	m := p.mod
	p.mu.Lock()
	defer p.mu.Unlock()
	pi := ProcInfo{ID: m.ID, Name: m.Name, Desc: m.Desc, Enabled: enabled, State: p.state, PID: p.pid,
		Restarts: p.restarts, CPU: p.cpu, Mem: p.mem, LastErr: p.lastErr, Ports: m.publicPorts(m),
		Panel: m.panelPort(m), Bundled: binAvailable(m)}
	if p.pid != 0 {
		pi.Uptime = int64(time.Since(p.started).Seconds())
	}
	return pi
}

var _ = io.Discard
