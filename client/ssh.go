package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/term"
)

// connect establishes an authenticated SSH client to device through the
// proxy. Future commands (scp/sftp, port forwarding) build on the returned
// client, which multiplexes any number of SSH channels over one tunnel.
func connect(ctx context.Context, cfg *config, device string) (*ssh.Client, error) {
	auth, err := authMethods(cfg, device)
	if err != nil {
		return nil, err
	}
	hostKeys, err := knownhosts.New(cfg.knownHosts)
	if err != nil {
		return nil, fmt.Errorf("known_hosts: %w", err)
	}

	conn, err := dialTunnel(ctx, cfg, device, 0)
	if err != nil {
		return nil, err
	}

	// Host keys are pinned by device ID. The tunnel has no meaningful TCP
	// peer, so hand knownhosts a placeholder; it prefers the hostname anyway.
	placeholder := &net.TCPAddr{IP: net.IPv4zero, Port: 22}
	sshCfg := &ssh.ClientConfig{
		User: cfg.user,
		Auth: auth,
		HostKeyCallback: func(hostname string, _ net.Addr, key ssh.PublicKey) error {
			if err := hostKeys(hostname, placeholder, key); err != nil {
				var keyErr *knownhosts.KeyError
				if errors.As(err, &keyErr) && len(keyErr.Want) == 0 {
					return fmt.Errorf("device %s is not in %s (host key %s)", device, cfg.knownHosts, ssh.FingerprintSHA256(key))
				}
				return err
			}
			return nil
		},
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, net.JoinHostPort(device, "22"), sshCfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return ssh.NewClient(c, chans, reqs), nil
}

// authMethods mirrors OpenSSH: try the identity key when there is one, then
// fall back to a password if the server allows it. SSHTUN_PASSWORD supplies
// the password non-interactively (for scripts and tests); otherwise the user
// is prompted, with up to three attempts.
func authMethods(cfg *config, device string) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if cfg.identity != "" {
		signer, err := loadSigner(cfg.identity)
		switch {
		case err == nil:
			methods = append(methods, ssh.PublicKeys(signer))
		case !errors.Is(err, fs.ErrNotExist):
			return nil, err
		}
	}
	if pw, ok := os.LookupEnv("SSHTUN_PASSWORD"); ok {
		return append(methods, ssh.Password(pw)), nil
	}
	prompt := func() (string, error) {
		pw, err := readSecret(fmt.Sprintf("%s@%s's password: ", cfg.user, device))
		return string(pw), err
	}
	return append(methods, ssh.RetryableAuthMethod(ssh.PasswordCallback(prompt), 3)), nil
}

// readSecret prompts on the controlling terminal without echo. It uses
// /dev/tty rather than stdin so prompts work while stdin carries data.
func readSecret(prompt string) ([]byte, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		fd := int(os.Stdin.Fd())
		if !term.IsTerminal(fd) {
			return nil, errors.New("no terminal to prompt on (set SSHTUN_PASSWORD for non-interactive use)")
		}
		fmt.Fprint(os.Stderr, prompt)
		defer fmt.Fprintln(os.Stderr)
		return term.ReadPassword(fd)
	}
	defer tty.Close()
	fmt.Fprint(tty, prompt)
	defer fmt.Fprintln(tty)
	return term.ReadPassword(int(tty.Fd()))
}

func loadSigner(path string) (ssh.Signer, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(pem)
	var missing *ssh.PassphraseMissingError
	if !errors.As(err, &missing) {
		return signer, err
	}
	pass, err := readSecret(fmt.Sprintf("Passphrase for %s: ", path))
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKeyWithPassphrase(pem, pass)
}
