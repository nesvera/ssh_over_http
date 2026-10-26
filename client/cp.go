package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/term"
)

// location is one side of a copy: a local path, or device:path on a target.
type location struct {
	device string // empty for local paths
	path   string
}

// parseLocation follows scp: "dev1:file" is remote unless a slash comes before
// the colon (so "./a:b" and "/tmp/a:b" stay local).
func parseLocation(s string) location {
	i := strings.IndexByte(s, ':')
	if i <= 0 || strings.Contains(s[:i], "/") {
		return location{path: s}
	}
	return location{device: s[:i], path: s[i+1:]}
}

// runCopy copies one regular file between this machine and a device over
// SFTP, the same transport modern OpenSSH scp uses.
func runCopy(ctx context.Context, cfg *config, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: sshtun cp <src> <dst>   (one side as <device>:<path>)")
	}
	src, dst := parseLocation(args[0]), parseLocation(args[1])
	if (src.device == "") == (dst.device == "") {
		return errors.New("cp: exactly one of src and dst must be <device>:<path>")
	}
	device := src.device + dst.device

	client, err := connect(ctx, cfg, device)
	if err != nil {
		return err
	}
	defer client.Close()
	sc, err := sftp.NewClient(client, sftp.UseConcurrentWrites(true), sftp.UseConcurrentReads(true))
	if err != nil {
		return fmt.Errorf("start sftp: %w", err)
	}
	defer sc.Close()

	if dst.device != "" {
		return upload(sc, src.path, dst.path)
	}
	return download(sc, src.path, dst.path)
}

func upload(sc *sftp.Client, localPath, remotePath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s: not a regular file (tar directories first)", localPath)
	}

	// Like scp: an empty path means the home directory, and copying into a
	// directory keeps the source file name.
	if remotePath == "" {
		remotePath = "."
	}
	if st, err := sc.Stat(remotePath); err == nil && st.IsDir() {
		remotePath = path.Join(remotePath, filepath.Base(localPath))
	}

	rf, err := sc.OpenFile(remotePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("%s: %w", remotePath, err)
	}
	p := startProgress(filepath.Base(localPath), info.Size())
	_, err = rf.ReadFromWithConcurrency(&countingReader{r: f, n: &p.done}, 0)
	p.stop()
	if err != nil {
		rf.Close()
		return fmt.Errorf("upload %s: %w", remotePath, err)
	}
	if err := rf.Chmod(info.Mode().Perm()); err != nil {
		rf.Close()
		return err
	}
	return rf.Close()
}

func download(sc *sftp.Client, remotePath, localPath string) error {
	rf, err := sc.Open(remotePath)
	if err != nil {
		return fmt.Errorf("%s: %w", remotePath, err)
	}
	defer rf.Close()
	info, err := rf.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s: not a regular file", remotePath)
	}

	if localPath == "" {
		localPath = "."
	}
	if st, err := os.Stat(localPath); err == nil && st.IsDir() {
		localPath = filepath.Join(localPath, path.Base(remotePath))
	}

	lf, err := os.OpenFile(localPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	p := startProgress(path.Base(remotePath), info.Size())
	_, err = rf.WriteTo(&countingWriter{w: lf, n: &p.done})
	p.stop()
	if err != nil {
		lf.Close()
		return fmt.Errorf("download %s: %w", remotePath, err)
	}
	return lf.Close()
}

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// progress prints an scp-style status line to stderr when it is a terminal.
type progress struct {
	name  string
	total int64
	done  atomic.Int64
	start time.Time
	quit  chan struct{}
	ended chan struct{}
}

func startProgress(name string, total int64) *progress {
	p := &progress{name: name, total: total, start: time.Now(), quit: make(chan struct{}), ended: make(chan struct{})}
	if !term.IsTerminal(int(os.Stderr.Fd())) {
		close(p.ended)
		return p
	}
	go func() {
		defer close(p.ended)
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-p.quit:
				p.print()
				fmt.Fprintln(os.Stderr)
				return
			case <-t.C:
				p.print()
			}
		}
	}()
	return p
}

func (p *progress) print() {
	done := p.done.Load()
	pct := 100.0
	if p.total > 0 {
		pct = float64(done) * 100 / float64(p.total)
	}
	rate := float64(done) / max(time.Since(p.start).Seconds(), 1e-3)
	fmt.Fprintf(os.Stderr, "\r%-30s %5.1f%% %10s %10s/s", p.name, pct, humanBytes(float64(done)), humanBytes(rate))
}

func (p *progress) stop() {
	close(p.quit)
	<-p.ended
}

func humanBytes(n float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", n, units[i])
}
