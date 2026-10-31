package main

import (
	"net/netip"
	"os"
	"path/filepath"
)

// macOS only allows utun interfaces; "utun" picks the first free number.
const defaultIfaceName = "utun"

func configureInterface(name string, local, peer netip.Addr) error {
	// A point-to-point address also installs the host route to peer.
	return runCommands([]string{"ifconfig", name, "inet", local.String(), peer.String(), "up"})
}

// registerName sends queries for name to server through a per-domain
// resolver file, which mDNSResponder picks up immediately.
func registerName(_, name string, server netip.Addr) (cleanup func(), via string, err error) {
	path := filepath.Join("/etc/resolver", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, "", err
	}
	if err := os.WriteFile(path, []byte(hostsTag+"\nnameserver "+server.String()+"\n"), 0o644); err != nil {
		return nil, "", err
	}
	return func() { os.Remove(path) }, path, nil
}
