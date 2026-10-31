package main

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"strings"

	"golang.org/x/net/dns/dnsmessage"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
)

// MagicDNS-style names for `sshtun tun`: device dev1 becomes dev1.sshtun.
//
// The netstack answers DNS on UDP port 53 at the device's own address, and
// only for that one name. registerName (per OS) then tells the system to send
// queries for the name there, so each sshtun process owns exactly one name
// and several can run side by side.

const dnsTTL = 60 // seconds; addresses only change when sshtun restarts

var dnsLabel = regexp.MustCompile(`^(?i)[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// deviceName returns the DNS name for device, or an error if the device ID
// can't be used as a DNS label.
func deviceName(device, domain string) (string, error) {
	if !dnsLabel.MatchString(device) {
		return "", fmt.Errorf("device ID %q is not a valid DNS label", device)
	}
	return strings.ToLower(device + "." + strings.Trim(domain, ".")), nil
}

// serveDNS answers queries arriving on conn until it is closed.
func serveDNS(conn *gonet.UDPConn, name string, addr netip.Addr) {
	buf := make([]byte, 1500)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			return
		}
		if resp := dnsAnswer(buf[:n], name, addr); resp != nil {
			conn.WriteTo(resp, from)
		}
	}
}

// dnsAnswer builds the response to one query, or returns nil to ignore it.
// name gets an A record; its other record types get an empty answer
// (NODATA) so resolvers don't wait for an AAAA that will never come; every
// other name is NXDOMAIN.
func dnsAnswer(query []byte, name string, addr netip.Addr) []byte {
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil || h.Response {
		return nil
	}
	q, err := p.Question()
	if err != nil {
		return nil
	}
	m := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:               h.ID,
			Response:         true,
			OpCode:           h.OpCode,
			Authoritative:    true,
			RecursionDesired: h.RecursionDesired,
		},
		Questions: []dnsmessage.Question{q},
	}
	switch {
	case h.OpCode != 0:
		m.RCode = dnsmessage.RCodeNotImplemented
	case !strings.EqualFold(q.Name.String(), name+"."):
		m.RCode = dnsmessage.RCodeNameError
	case q.Type == dnsmessage.TypeA && q.Class == dnsmessage.ClassINET:
		m.Answers = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: dnsTTL},
			Body:   &dnsmessage.AResource{A: addr.As4()},
		}}
	}
	resp, err := m.Pack()
	if err != nil {
		return nil
	}
	return resp
}

const (
	hostsFile = "/etc/hosts"
	hostsTag  = "# added by sshtun tun"
)

// addHostsEntry maps name to addr in /etc/hosts, for systems where queries
// for a single name can't be routed to our resolver. The returned function
// removes the entry again.
func addHostsEntry(name string, addr netip.Addr) (func(), error) {
	if err := editHosts(name, fmt.Sprintf("%s\t%s\t%s", addr, name, hostsTag)); err != nil {
		return nil, err
	}
	return func() { editHosts(name, "") }, nil
}

// editHosts drops any line sshtun added for name (including ones left by a
// killed sshtun), then appends line unless it is empty. The file is rewritten
// in place because Docker bind-mounts /etc/hosts, so it can't be replaced.
func editHosts(name, line string) error {
	data, err := os.ReadFile(hostsFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var kept []string
	for _, l := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if f := strings.Fields(l); strings.HasSuffix(l, hostsTag) && len(f) > 1 && f[1] == name {
			continue
		}
		kept = append(kept, l)
	}
	if line != "" {
		kept = append(kept, line)
	}
	return os.WriteFile(hostsFile, []byte(strings.Join(kept, "\n")+"\n"), 0o644)
}
