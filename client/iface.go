package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.zx2c4.com/wireguard/tun"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// The tun command gives a device an IP address on this machine. Packets the
// kernel routes to that address arrive on a TUN interface and are fed to a
// userspace TCP/IP stack (gVisor netstack) that plays the device's part:
//
//   - TCP to port 22 is accepted locally and each connection is carried over
//     its own tunnel to the device's sshd, exactly like `sshtun shell`.
//   - ICMP echo is answered only after the proxy round-trips a ping to the
//     device's agent, so ping reports real reachability and latency.
//   - DNS on UDP port 53 answers the device's name, dev1.sshtun (dns.go).
//   - Everything else is refused (TCP RST, ICMP port unreachable) or dropped.
//
// No IP packets cross the tunnel, so the device needs no TUN, privileges or
// protocol changes; only the client needs root to create the interface.

const (
	ifaceMTU = 1500
	// tunOffset is headroom before each packet that wireguard's tun package
	// needs (virtio-net header on Linux, address family on macOS).
	tunOffset    = 16
	sshPort      = 22
	maxPings     = 16 // ICMP echoes in flight; beyond this they are dropped
	pingDeadline = 5 * time.Second
)

func runTun(ctx context.Context, cfg *config, args []string) error {
	fs := flag.NewFlagSet("tun", flag.ContinueOnError)
	deviceIP := fs.String("ip", "10.77.0.2", "IPv4 address the device appears at")
	localIP := fs.String("local", "10.77.0.1", "IPv4 address of this machine on the link")
	name := fs.String("name", defaultIfaceName, "interface name")
	domain := fs.String("domain", "sshtun", "DNS domain for the device's name (<device>.<domain>); empty disables names")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: sshtun tun [-ip addr] [-local addr] [-name ifname] <device>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	device := fs.Arg(0)
	peer, err := parseIPv4(*deviceIP)
	if err != nil {
		return err
	}
	local, err := parseIPv4(*localIP)
	if err != nil {
		return err
	}
	if peer == local {
		return errors.New("-ip and -local must differ")
	}
	var dnsName string
	if *domain != "" {
		if dnsName, err = deviceName(device, *domain); err != nil {
			return fmt.Errorf("%w; use -domain '' to run without a name", err)
		}
	}

	hc, err := httpClient(cfg)
	if err != nil {
		return err
	}
	// Fail before touching the network config if the device is unreachable.
	if err := pingDevice(ctx, hc, cfg, device); err != nil {
		return fmt.Errorf("device %s: %w", device, err)
	}

	dev, err := tun.CreateTUN(*name, ifaceMTU)
	if err != nil {
		return fmt.Errorf("create interface (needs root or CAP_NET_ADMIN): %w", err)
	}
	// Closing the TUN removes the interface along with its address and route.
	defer dev.Close()
	go func() {
		for range dev.Events() {
		}
	}()
	ifname, err := dev.Name()
	if err != nil {
		return err
	}
	if err := configureInterface(ifname, local, peer); err != nil {
		return err
	}

	l := &link{
		dev:    dev,
		cfg:    cfg,
		hc:     hc,
		device: device,
		addr:   tcpip.AddrFrom4(peer.As4()),
		pings:  make(chan struct{}, maxPings),
	}
	if err := l.startStack(); err != nil {
		return err
	}
	defer l.stack.Close()

	where := peer.String()
	if dnsName != "" {
		dns, err := gonet.DialUDP(l.stack, &tcpip.FullAddress{NIC: 1, Addr: l.addr, Port: 53}, nil, ipv4.ProtocolNumber)
		if err != nil {
			return fmt.Errorf("dns: %w", err)
		}
		defer dns.Close()
		go serveDNS(dns, dnsName, peer)
		unregister, via, err := registerName(ifname, dnsName, peer)
		if err != nil {
			return fmt.Errorf("register %s: %w", dnsName, err)
		}
		defer unregister()
		where = fmt.Sprintf("%s (%s, via %s)", dnsName, peer, via)
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	go l.writeLoop(ctx)
	errc := make(chan error, 1)
	go func() { errc <- l.readLoop() }()

	fmt.Fprintf(os.Stderr, "%s: device %s is at %s; ssh on port %d, ping; Ctrl-C to remove\n", ifname, device, where, sshPort)
	select {
	case <-ctx.Done():
		fmt.Fprintf(os.Stderr, "%s: removed\n", ifname)
		return nil
	case err := <-errc:
		return fmt.Errorf("%s: %w", ifname, err)
	}
}

// link connects one TUN interface to a netstack that impersonates the device.
type link struct {
	dev    tun.Device
	cfg    *config
	hc     *http.Client
	device string
	addr   tcpip.Address // the device's address on the link

	stack *stack.Stack
	ep    *channel.Endpoint
	pings chan struct{}

	wmu sync.Mutex // serialises writes to dev
}

func (l *link) startStack() error {
	l.stack = stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	sack := tcpip.TCPSACKEnabled(true)
	l.stack.SetTransportProtocolOption(tcp.ProtocolNumber, &sack)

	const nic = 1
	l.ep = channel.New(512, ifaceMTU, "")
	if err := l.stack.CreateNIC(nic, l.ep); err != nil {
		return fmt.Errorf("netstack: create NIC: %v", err)
	}
	addr := tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: l.addr.WithPrefix()}
	if err := l.stack.AddProtocolAddress(nic, addr, stack.AddressProperties{}); err != nil {
		return fmt.Errorf("netstack: add address: %v", err)
	}
	l.stack.AddRoute(tcpip.Route{Destination: header.IPv4EmptySubnet, NIC: nic})

	fwd := tcp.NewForwarder(l.stack, 0, 64, l.handleTCP)
	l.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)
	return nil
}

// readLoop moves packets from the kernel into the netstack until the TUN
// is closed.
func (l *link) readLoop() error {
	bufs := make([][]byte, l.dev.BatchSize())
	for i := range bufs {
		bufs[i] = make([]byte, tunOffset+65535)
	}
	sizes := make([]int, len(bufs))
	for {
		n, err := l.dev.Read(bufs, sizes, tunOffset)
		if errors.Is(err, tun.ErrTooManySegments) {
			continue // the batch overflowed; the dropped segments are retransmitted
		}
		if err != nil {
			return err
		}
		for i := range n {
			l.inbound(bufs[i][tunOffset : tunOffset+sizes[i]])
		}
	}
}

func (l *link) inbound(pkt []byte) {
	ip := header.IPv4(pkt)
	// Drops IPv6 and anything not addressed to the device.
	if len(pkt) < header.IPv4MinimumSize || !ip.IsValid(len(pkt)) || ip.DestinationAddress() != l.addr {
		return
	}
	if ip.TransportProtocol() == header.ICMPv4ProtocolNumber {
		l.echo(ip)
		return
	}
	pkb := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(pkt)})
	l.ep.InjectInbound(ipv4.ProtocolNumber, pkb)
	pkb.DecRef()
}

// writeLoop moves packets from the netstack to the kernel.
func (l *link) writeLoop(ctx context.Context) {
	for {
		pkb := l.ep.ReadContext(ctx)
		if pkb == nil {
			return
		}
		view := pkb.ToView()
		pkb.DecRef()
		buf := make([]byte, tunOffset+view.Size())
		view.Read(buf[tunOffset:])
		view.Release()
		l.write(buf)
	}
}

// write sends one packet, which starts tunOffset bytes into buf.
func (l *link) write(buf []byte) {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	l.dev.Write([][]byte{buf}, tunOffset)
}

// echo answers an ICMP echo request once the device's agent has answered a
// ping through the proxy. No reply means the device is unreachable.
func (l *link) echo(ip header.IPv4) {
	icmp := header.ICMPv4(ip.Payload())
	if len(icmp) < header.ICMPv4MinimumSize || icmp.Type() != header.ICMPv4Echo || ip.More() || ip.FragmentOffset() != 0 {
		return
	}
	select {
	case l.pings <- struct{}{}:
	default:
		return // too many in flight (ping -f): drop, like a congested link
	}
	buf := make([]byte, tunOffset+len(ip))
	copy(buf[tunOffset:], ip)
	go func() {
		defer func() { <-l.pings }()
		ctx, cancel := context.WithTimeout(context.Background(), pingDeadline)
		defer cancel()
		if pingDevice(ctx, l.hc, l.cfg, l.device) != nil {
			return
		}
		echoReply(header.IPv4(buf[tunOffset:]))
		l.write(buf)
	}()
}

// echoReply turns an ICMP echo request into its reply in place.
func echoReply(ip header.IPv4) {
	src, dst := ip.SourceAddress(), ip.DestinationAddress()
	ip.SetSourceAddress(dst)
	ip.SetDestinationAddress(src)
	ip.SetTTL(64)
	ip.SetChecksum(0)
	ip.SetChecksum(^ip.CalculateChecksum())
	icmp := header.ICMPv4(ip.Payload())
	icmp.SetType(header.ICMPv4EchoReply)
	icmp.SetChecksum(0)
	icmp.SetChecksum(^checksum.Checksum(icmp, 0))
}

// handleTCP runs for each new TCP connection to the device. SSH connections
// are completed only once the tunnel is up, so an unreachable device shows
// up as "connection refused" rather than an immediate hang-up.
func (l *link) handleTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	if id.LocalPort != sshPort {
		r.Complete(true)
		return
	}
	from := fmt.Sprintf("%s:%d", id.RemoteAddress, id.RemotePort)
	remote, err := dialTunnel(context.Background(), l.cfg, l.device)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssh from %s: %v\n", from, err)
		r.Complete(true)
		return
	}
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	if terr != nil {
		remote.Close()
		r.Complete(true)
		return
	}
	r.Complete(false)
	fmt.Fprintf(os.Stderr, "ssh from %s: connected\n", from)
	pipe(gonet.NewTCPConn(&wq, ep), remote)
	fmt.Fprintf(os.Stderr, "ssh from %s: closed\n", from)
}

// pipe copies in both directions until either side finishes, then closes both.
func pipe(a, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }()
	go func() { io.Copy(b, a); done <- struct{}{} }()
	<-done
	a.Close()
	b.Close()
	<-done
}

func parseIPv4(s string) (netip.Addr, error) {
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is4() {
		return netip.Addr{}, fmt.Errorf("%q is not an IPv4 address", s)
	}
	return a, nil
}

func runCommands(cmds ...[]string) error {
	for _, c := range cmds {
		if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %v: %s", strings.Join(c, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}
