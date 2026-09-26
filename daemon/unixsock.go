package daemon

import (
	"encoding/json"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
)

// clearStaleSockets removes unix-socket files left by an xray that died
// without cleaning up (killed, or its daemon was). xray never unlinks an
// existing path before binding, so one leftover file fails every start with
// "bind: address already in use". A socket something still answers on is
// left alone unless reap (terminating a leftover emx xray) frees it.
func clearStaleSockets(configPath string, logger *log.Logger, reap func() []int) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return
	}
	var cfg struct {
		Inbounds []struct {
			Listen string `json:"listen"`
		} `json:"inbounds"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return
	}
	var held []string
	for _, in := range cfg.Inbounds {
		if !strings.HasPrefix(in.Listen, "/") { // "@" abstract sockets leave no file
			continue
		}
		path := xray.UnixSocketPath(in.Listen)
		if removeIfStaleSocket(path) {
			logger.Printf("removed stale unix socket %s", path)
		} else if isSocket(path) {
			held = append(held, path)
		}
	}
	if len(held) == 0 {
		return
	}
	if reap != nil {
		if pids := reap(); len(pids) > 0 {
			logger.Printf("terminated leftover xray %v holding %v", pids, held)
		}
	}
	for _, path := range held {
		if removeIfStaleSocket(path) || !isSocket(path) {
			logger.Printf("freed unix socket %s", path)
		} else {
			logger.Printf("unix socket %s is held by another process — xray can't bind it", path)
		}
	}
}

// removeIfStaleSocket unlinks path when it is a socket nobody accepts on.
func removeIfStaleSocket(path string) bool {
	if !isSocket(path) {
		return false
	}
	if c, err := net.DialTimeout("unix", path, 500*time.Millisecond); err == nil {
		_ = c.Close()
		return false
	}
	return os.Remove(path) == nil
}

func isSocket(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}
