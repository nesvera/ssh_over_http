//go:build !unix

package main

import "golang.org/x/crypto/ssh"

func watchResize(int, *ssh.Session) (stop func()) { return func() {} }
