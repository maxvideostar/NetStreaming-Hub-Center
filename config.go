package main

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ModCfg struct {
	Enabled bool `json:"enabled"`
}

type SuiteCfg struct {
	Listen       string             `json:"listen"`
	Username     string             `json:"username"`
	PassHash     string             `json:"pass_hash"`
	Secret       string             `json:"secret"`        // semnare cookie sesiune
	InternalPass string             `json:"internal_pass"` // parola internă pusă în fiecare modul
	Modules      map[string]*ModCfg `json:"modules"`
}

var (
	cfgMu   sync.RWMutex
	cfg     *SuiteCfg
	cfgPath string
	dataDir string
)

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hashPassword(pw string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	key, _ := pbkdf2.Key(sha256.New, pw, salt, 120000, 32)
	return "pbkdf2$120000$" + hex.EncodeToString(salt) + "$" + base64.StdEncoding.EncodeToString(key)
}

func checkPassword(hash, pw string) bool {
	p := strings.Split(hash, "$")
	if len(p) != 4 || p[0] != "pbkdf2" {
		return false
	}
	iter, err := strconv.Atoi(p[1])
	if err != nil || iter < 1 {
		return false
	}
	salt, err1 := hex.DecodeString(p[2])
	want, err2 := base64.StdEncoding.DecodeString(p[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return filepath.Dir(exe)
}

func loadConfig(path string) error {
	cfgPath = path
	c := &SuiteCfg{}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if err := json.Unmarshal(b, c); err != nil {
			return err
		}
	}
	if c.Listen == "" {
		c.Listen = ":8090"
	}
	if c.Username == "" {
		c.Username = "admin"
	}
	if c.PassHash == "" {
		c.PassHash = hashPassword("admin")
	}
	if c.Secret == "" {
		c.Secret = randHex(32)
	}
	if c.InternalPass == "" {
		c.InternalPass = randHex(16)
	}
	if c.Modules == nil {
		c.Modules = map[string]*ModCfg{}
	}
	for _, m := range modules {
		if c.Modules[m.ID] == nil {
			c.Modules[m.ID] = &ModCfg{}
		}
	}
	cfgMu.Lock()
	cfg = c
	cfgMu.Unlock()
	return saveConfig()
}

func getCfg() SuiteCfg {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	c := *cfg
	c.Modules = map[string]*ModCfg{}
	for k, v := range cfg.Modules {
		vv := *v
		c.Modules[k] = &vv
	}
	return c
}

func updateCfg(f func(c *SuiteCfg)) error {
	cfgMu.Lock()
	f(cfg)
	cfgMu.Unlock()
	return saveConfig()
}

func saveConfig() error {
	cfgMu.RLock()
	b, _ := json.MarshalIndent(cfg, "", "  ")
	cfgMu.RUnlock()
	return writeFileAtomic(cfgPath, b, 0600)
}

func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	os.MkdirAll(filepath.Dir(path), 0755)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---- cookie de sesiune semnat (supraviețuiește repornirii) ----

const sessCookie = "nts_suite"
const sessTTL = 7 * 24 * time.Hour

func makeSession(user string) string {
	c := getCfg()
	exp := strconv.FormatInt(time.Now().Add(sessTTL).Unix(), 10)
	payload := base64.RawURLEncoding.EncodeToString([]byte(user)) + "." + exp
	mac := hmac.New(sha256.New, []byte(c.Secret+c.PassHash))
	mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}

func checkSession(tok string) bool {
	p := strings.Split(tok, ".")
	if len(p) != 3 {
		return false
	}
	c := getCfg()
	mac := hmac.New(sha256.New, []byte(c.Secret+c.PassHash))
	mac.Write([]byte(p[0] + "." + p[1]))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(p[2])) != 1 {
		return false
	}
	exp, err := strconv.ParseInt(p[1], 10, 64)
	return err == nil && time.Now().Unix() < exp
}
