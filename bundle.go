package main

import (
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// Executabilele modulelor sunt incluse comprimat în bundle/<bin>[.exe].gz
// (build.sh le compilează din cele 5 repo-uri) și se extrag în data/bin.
//
//go:embed bundle
var bundleFS embed.FS

var extractMu sync.Mutex

func bundleName(m *Module) string {
	n := m.Bin
	if runtime.GOOS == "windows" {
		n += ".exe"
	}
	return "bundle/" + n + ".gz"
}

func bundled(m *Module) bool {
	_, err := fs.Stat(bundleFS, bundleName(m))
	return err == nil
}

func binAvailable(m *Module) bool {
	if bundled(m) {
		return true
	}
	_, err := os.Stat(m.BinPath())
	return err == nil
}

// ensureBin extrage executabilul dacă lipsește sau dacă versiunea inclusă e diferită.
func ensureBin(m *Module) error {
	extractMu.Lock()
	defer extractMu.Unlock()
	dst := m.BinPath()
	if !bundled(m) {
		if _, err := os.Stat(dst); err != nil {
			return fmt.Errorf("executabilul %s nu este inclus în această versiune (pune-l manual în %s)", m.Bin, filepath.Dir(dst))
		}
		return nil
	}
	gz, err := bundleFS.ReadFile(bundleName(m))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(gz)
	want := hex.EncodeToString(sum[:])
	stamp := dst + ".sha"
	if old, err := os.ReadFile(stamp); err == nil && string(old) == want {
		if _, err := os.Stat(dst); err == nil {
			return nil
		}
	}
	os.MkdirAll(filepath.Dir(dst), 0755)
	zr, err := gzip.NewReader(bytesReader(gz))
	if err != nil {
		return err
	}
	tmp := dst + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, zr); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	os.Remove(dst + ".old")
	if _, err := os.Stat(dst); err == nil {
		// pe Windows un .exe care rulează nu poate fi suprascris, dar poate fi redenumit
		if err := os.Rename(dst, dst+".old"); err != nil {
			os.Remove(tmp)
			return fmt.Errorf("nu pot actualiza %s: %v", dst, err)
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	os.WriteFile(stamp, []byte(want), 0644)
	return nil
}

type byteReader struct {
	b []byte
	i int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

func bytesReader(b []byte) io.Reader { return &byteReader{b: b} }
