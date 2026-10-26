//go:build unix

package main

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// watchResize forwards terminal size changes to the remote PTY.
func watchResize(fd int, sess *ssh.Session) (stop func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		for range ch {
			if w, h, err := term.GetSize(fd); err == nil {
				sess.WindowChange(h, w)
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(ch)
	}
}
