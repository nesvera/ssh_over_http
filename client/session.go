package main

import (
	"os"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

func runShell(client *ssh.Client) error {
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Stdin, sess.Stdout, sess.Stderr = os.Stdin, os.Stdout, os.Stderr

	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		w, h, err := term.GetSize(fd)
		if err != nil || w == 0 || h == 0 {
			w, h = 80, 24
		}
		termName := os.Getenv("TERM")
		if termName == "" {
			termName = "xterm-256color"
		}
		modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}
		if err := sess.RequestPty(termName, h, w, modes); err != nil {
			return err
		}
		state, err := term.MakeRaw(fd)
		if err != nil {
			return err
		}
		defer term.Restore(fd, state)
		stop := watchResize(fd, sess)
		defer stop()
	}

	if err := sess.Shell(); err != nil {
		return err
	}
	return sess.Wait()
}

func runExec(client *ssh.Client, cmd string) error {
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Stdin, sess.Stdout, sess.Stderr = os.Stdin, os.Stdout, os.Stderr
	return sess.Run(cmd)
}
