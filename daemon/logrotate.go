package daemon

import (
	"context"
	"io"
	"log"
	"os"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
	"github.com/ehsan200/em-xray/internal/paths"
)

// logRotateTick is how often log sizes are checked against the cap.
const logRotateTick = time.Minute

// LogRotator keeps xray's access/error logs from devouring the disk. When a file
// exceeds the configured cap it is rolled to "<path>.prev" (previous generation)
// and truncated to zero — xray keeps its open handle and simply appends from the
// start again, so no restart is needed. Peak on-disk usage per log is ~2× cap.
type LogRotator struct {
	store *xray.Store
	paths paths.Paths
	log   *log.Logger
}

func NewLogRotator(store *xray.Store, p paths.Paths, logger *log.Logger) *LogRotator {
	return &LogRotator{store: store, paths: p, log: logger}
}

func (r *LogRotator) Start(ctx context.Context) {
	go func() {
		tk := time.NewTicker(logRotateTick)
		defer tk.Stop()
		r.check()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				r.check()
			}
		}
	}()
}

func (r *LogRotator) check() {
	maxMB := r.store.LogMaxMB()
	if maxMB <= 0 {
		return // rotation disabled
	}
	limit := int64(maxMB) << 20
	for _, path := range []string{r.paths.AccessLog(), r.paths.ErrorLog()} {
		fi, err := os.Stat(path)
		if err != nil || fi.Size() <= limit {
			continue
		}
		if err := rollLog(path); err != nil {
			r.log.Printf("logrotate %s: %v", path, err)
		} else {
			r.log.Printf("logrotate: %s reached %d MB → rolled to %s.prev", path, fi.Size()>>20, path)
		}
	}
}

// rollLog copies path to path+".prev" (overwriting the older generation) then
// truncates path to zero. xray's O_APPEND handle keeps writing from offset 0.
func rollLog(path string) error {
	if err := copyFile(path, path+".prev"); err != nil {
		return err
	}
	return os.Truncate(path, 0)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
