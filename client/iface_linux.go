package main

import "net/netip"

// The kernel replaces %d with the first free number.
const defaultIfaceName = "sshtun%d"

func configureInterface(name string, local, peer netip.Addr) error {
	return runCommands(
		[]string{"ip", "addr", "add", local.String() + "/32", "dev", name},
		[]string{"ip", "link", "set", "dev", name, "up"},
		[]string{"ip", "route", "add", peer.String() + "/32", "dev", name, "src", local.String()},
	)
}
