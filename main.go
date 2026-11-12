package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/kardianos/service"
)

const version = "1.0.6"

type program struct{}

func (p *program) Start(s service.Service) error {
	go p.run()
	return nil
}

func (p *program) run() {
	if runtime.GOOS == "windows" {
		firewallSetup() // la fiecare pornire (fără drepturi de admin pur și simplu nu are efect)
	}
	go statsLoop()
	c := getCfg()
	for _, m := range modules {
		if c.Modules[m.ID].Enabled {
			procs[m.ID].Start()
		}
	}
	go portWatch()
	if err := serveOn(c.Listen); err != nil {
		log.Printf("EROARE: nu pot asculta pe %s: %v", c.Listen, err)
		os.Exit(1)
	}
	log.Printf("nTs-Suite %s – panou: http://localhost:%s  (date: %s)", version, portOf(c.Listen, "8090"), dataDir)
	if service.Interactive() && os.Getenv("NTS_NOBROWSER") == "" {
		openBrowser("http://localhost:" + portOf(c.Listen, "8090") + "/")
	}
}

var (
	srvMu  sync.Mutex
	curSrv *http.Server
)

// serveOn pornește panoul pe addr; vechiul listener se închide după câteva secunde
// (schimbarea portului suitei se aplică imediat, fără repornire).
func serveOn(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s := &http.Server{Handler: handler(), ReadHeaderTimeout: 15 * time.Second}
	srvMu.Lock()
	old := curSrv
	curSrv = s
	srvMu.Unlock()
	go func() {
		if err := s.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("EROARE http: %v", err)
		}
	}()
	if old != nil {
		go func() {
			time.Sleep(4 * time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			old.Shutdown(ctx)
			cancel()
		}()
	}
	return nil
}

func (p *program) Stop(s service.Service) error {
	log.Printf("oprire – închid modulele")
	var wg sync.WaitGroup
	for _, m := range modules {
		wg.Add(1)
		go func(pr *Proc) {
			defer wg.Done()
			pr.mu.Lock()
			act := pr.active
			pr.mu.Unlock()
			if act {
				pr.Stop() // setarea "activat" rămâne – modulul pornește din nou cu suita
			}
		}(procs[m.ID])
	}
	wg.Wait()
	srvMu.Lock()
	hs := curSrv
	srvMu.Unlock()
	if hs != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		hs.Shutdown(ctx)
		cancel()
	}
	return nil
}

func main() {
	dataFlag := flag.String("data", filepath.Join(exeDir(), "data"), "director date (config, module, jurnale)")
	flag.Usage = func() {
		fmt.Printf(`nTs-Suite %s – ProxyHub + RTMP Media Server + VideoEncoder + Rec-Martori + nTs-relay
                 într-un singur panou (implicit http://localhost:8090)

Utilizare: nts-suite [-data DIR] [comandă]

Comenzi:
  (fără)                  pornește în consolă
  install                 instalează ca serviciu (Windows Service / systemd) și pornește
  uninstall               oprește și dezinstalează serviciul
  start|stop|restart      controlează serviciul
  status                  starea serviciului
  passwd <user> <parola>  resetează contul panoului
  firewall                (Windows) refă regulile de firewall
  version
`, version)
	}
	flag.Parse()
	dataDir, _ = filepath.Abs(*dataFlag)
	os.MkdirAll(dataDir, 0755)

	svcCfg := &service.Config{
		Name:        "nTs-Suite",
		DisplayName: "nTs-Suite",
		Description: "nTs-Suite – ProxyHub, RTMP Media Server, VideoEncoder, Rec-Martori, nTs-relay (panou unic)",
		Arguments:   []string{"-data", dataDir},
		Option: service.KeyValue{
			"Restart": "always", "OnFailure": "restart", "OnFailureDelayDuration": "5s",
		},
	}
	if runtime.GOOS == "linux" {
		svcCfg.Dependencies = []string{"After=network-online.target", "Wants=network-online.target"}
		svcCfg.Option["LimitNOFILE"] = 65536
		svcCfg.Option["KillMode"] = "mixed"
	}
	prg := &program{}
	svc, err := service.New(prg, svcCfg)
	if err != nil {
		fmt.Println("eroare serviciu:", err)
		os.Exit(1)
	}

	if err := loadConfig(filepath.Join(dataDir, "suite.json")); err != nil {
		fmt.Println("config:", err)
		os.Exit(1)
	}

	if cmd := flag.Arg(0); cmd != "" {
		switch cmd {
		case "version":
			fmt.Println(version)
		case "passwd":
			if flag.NArg() < 3 {
				fmt.Println("utilizare: nts-suite passwd <user> <parola>")
				os.Exit(2)
			}
			updateCfg(func(c *SuiteCfg) { c.Username, c.PassHash = flag.Arg(1), hashPassword(flag.Arg(2)) })
			fmt.Println("cont actualizat (repornește serviciul dacă rulează)")
		case "firewall":
			initProcs()
			firewallSetup()
			fmt.Println("firewall: OK")
		case "status":
			st, err := svc.Status()
			if err != nil {
				fmt.Println("status:", err)
				os.Exit(1)
			}
			fmt.Println(map[service.Status]string{service.StatusRunning: "pornit", service.StatusStopped: "oprit"}[st])
		case "install":
			if err := service.Control(svc, "install"); err != nil {
				fmt.Println("install:", err)
				os.Exit(1)
			}
			initProcs()
			firewallSetup()
			if err := service.Control(svc, "start"); err != nil {
				fmt.Println("start:", err)
				os.Exit(1)
			}
			fmt.Printf("instalat și pornit – deschide http://localhost:%s\n", portOf(getCfg().Listen, "8090"))
		case "uninstall", "start", "stop", "restart":
			if cmd == "uninstall" {
				service.Control(svc, "stop")
			}
			if err := service.Control(svc, cmd); err != nil {
				fmt.Printf("%s: %v\n", cmd, err)
				os.Exit(1)
			}
			fmt.Printf("%s: OK\n", cmd)
		default:
			flag.Usage()
			os.Exit(2)
		}
		return
	}

	initProcs()
	os.MkdirAll(filepath.Join(dataDir, "logs"), 0755)
	lf, err := os.OpenFile(filepath.Join(dataDir, "logs", "suite.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err == nil {
		log.SetOutput(io.MultiWriter(os.Stderr, lf))
	}
	if err := svc.Run(); err != nil {
		log.Println(err)
	}
}
