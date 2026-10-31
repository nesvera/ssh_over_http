//go:build !linux && !darwin

package main

import (
	"errors"
	"net/netip"
)

const defaultIfaceName = "sshtun0"

func configureInterface(string, netip.Addr, netip.Addr) error {
	return errors.New("sshtun tun is only supported on Linux and macOS")
}

func registerName(string, string, netip.Addr) (func(), string, error) {
	return nil, "", errors.New("not supported")
}
