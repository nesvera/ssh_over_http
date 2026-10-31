package main

import (
	"net/netip"
	"os/exec"
)

// The kernel replaces %d with the first free number.
const defaultIfaceName = "sshtun%d"

func configureInterface(name string, local, peer netip.Addr) error {
	return runCommands(
		[]string{"ip", "addr", "add", local.String() + "/32", "dev", name},
		[]string{"ip", "link", "set", "dev", name, "up"},
		[]string{"ip", "route", "add", peer.String() + "/32", "dev", name, "src", local.String()},
	)
}

// registerName sends queries for name to server. With systemd-resolved that
// is a per-link routing domain, which disappears with the interface. Without
// it, resolv.conf can't route one name to one server (and taking it over
// would clash with other sshtun processes), so fall back to /etc/hosts.
func registerName(ifname, name string, server netip.Addr) (cleanup func(), via string, err error) {
	if _, err := exec.LookPath("resolvectl"); err == nil {
		err := runCommands(
			[]string{"resolvectl", "dns", ifname, server.String()},
			[]string{"resolvectl", "domain", ifname, "~" + name},
		)
		if err == nil {
			return func() {}, "systemd-resolved", nil
		}
	}
	cleanup, err = addHostsEntry(name, server)
	return cleanup, hostsFile, err
}
