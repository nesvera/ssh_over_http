package main

import "net/netip"

// macOS only allows utun interfaces; "utun" picks the first free number.
const defaultIfaceName = "utun"

func configureInterface(name string, local, peer netip.Addr) error {
	// A point-to-point address also installs the host route to peer.
	return runCommands([]string{"ifconfig", name, "inet", local.String(), peer.String(), "up"})
}
