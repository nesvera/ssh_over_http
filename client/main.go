// Command sshtun opens SSH sessions to devices through the WebSocket proxy.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

type config struct {
	proxyURL   string
	token      string
	caFile     string
	user       string
	identity   string
	knownHosts string
}

const usage = `Usage: sshtun [flags] <command> [args]

Commands:
  devices                    list devices connected to the proxy
  shell <device>             open an interactive shell
  exec <device> <command...> run a command and exit with its status
  cp <src> <dst>             copy a file, scp style; one side is <device>:<path>
                             e.g. sshtun cp app.tar dev1:/tmp/
                                  sshtun cp dev1:/tmp/app.tar .
  tun [-ip addr] <device>    give the device an IP address on this machine
                             (default 10.77.0.2) until Ctrl-C, so ssh, scp
                             and ping work against it directly; needs root

Authentication tries the identity key first, then prompts for a password if
the server allows it. SSHTUN_PASSWORD supplies the password non-interactively.

Flags (each also settable through the environment variable shown):
`

func main() {
	home, _ := os.UserHomeDir()
	defaultUser := ""
	if u, err := user.Current(); err == nil {
		defaultUser = u.Username
	}

	var cfg config
	flag.StringVar(&cfg.proxyURL, "proxy", env("SSHTUN_PROXY", "wss://localhost:8443"), "proxy URL [SSHTUN_PROXY]")
	flag.StringVar(&cfg.token, "token", env("SSHTUN_TOKEN", ""), "proxy bearer token [SSHTUN_TOKEN]")
	flag.StringVar(&cfg.caFile, "ca", env("SSHTUN_CA", ""), "extra CA certificate to trust for the proxy [SSHTUN_CA]")
	flag.StringVar(&cfg.user, "user", env("SSHTUN_USER", defaultUser), "remote SSH user [SSHTUN_USER]")
	flag.StringVar(&cfg.identity, "i", env("SSHTUN_IDENTITY", filepath.Join(home, ".ssh", "id_ed25519")), "SSH private key; skipped if missing, empty disables key auth [SSHTUN_IDENTITY]")
	flag.StringVar(&cfg.knownHosts, "known-hosts", env("SSHTUN_KNOWN_HOSTS", filepath.Join(home, ".ssh", "known_hosts")), "known_hosts file, keyed by device ID [SSHTUN_KNOWN_HOSTS]")
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
		flag.PrintDefaults()
	}
	flag.Parse()

	if err := run(context.Background(), &cfg, flag.Args()); err != nil {
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitStatus())
		}
		fmt.Fprintln(os.Stderr, "sshtun:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg *config, args []string) error {
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	switch cmd, args := args[0], args[1:]; cmd {
	case "devices":
		return listDevices(ctx, cfg)
	case "shell":
		if len(args) != 1 {
			return errors.New("usage: sshtun shell <device>")
		}
		client, err := connect(ctx, cfg, args[0])
		if err != nil {
			return err
		}
		defer client.Close()
		return runShell(client)
	case "exec":
		if len(args) < 2 {
			return errors.New("usage: sshtun exec <device> <command...>")
		}
		client, err := connect(ctx, cfg, args[0])
		if err != nil {
			return err
		}
		defer client.Close()
		return runExec(client, strings.Join(args[1:], " "))
	case "cp":
		return runCopy(ctx, cfg, args)
	case "tun":
		return runTun(ctx, cfg, args)
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
