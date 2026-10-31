package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const dialTimeout = 15 * time.Second

func httpClient(cfg *config) (*http.Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.caFile != "" {
		pem, err := os.ReadFile(cfg.caFile)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates found in %s", cfg.caFile)
		}
		tlsCfg.RootCAs = pool
	}
	return &http.Client{
		Timeout:   dialTimeout,
		Transport: &http.Transport{TLSClientConfig: tlsCfg, Proxy: http.ProxyFromEnvironment},
	}, nil
}

// endpoint resolves path against the proxy URL, using scheme for the result
// (ws/wss for WebSockets, http/https for plain requests).
func endpoint(cfg *config, path string, query url.Values, websocketScheme bool) (string, error) {
	u, err := url.Parse(cfg.proxyURL)
	if err != nil {
		return "", fmt.Errorf("invalid proxy URL: %w", err)
	}
	secure := u.Scheme == "wss" || u.Scheme == "https"
	switch {
	case websocketScheme && secure:
		u.Scheme = "wss"
	case websocketScheme:
		u.Scheme = "ws"
	case secure:
		u.Scheme = "https"
	default:
		u.Scheme = "http"
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func authHeader(cfg *config) http.Header {
	return http.Header{"Authorization": {"Bearer " + cfg.token}}
}

// dialTunnel returns a byte stream to a TCP port on the device; port 0 means
// the device's SSH server. The device's agent decides which ports it allows.
func dialTunnel(ctx context.Context, cfg *config, device string, port uint16) (net.Conn, error) {
	hc, err := httpClient(cfg)
	if err != nil {
		return nil, err
	}
	query := url.Values{"device": {device}}
	if port != 0 {
		query.Set("port", strconv.Itoa(int(port)))
	}
	u, err := endpoint(cfg, "/connect", query, true)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: hc, HTTPHeader: authHeader(cfg)})
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			return nil, fmt.Errorf("proxy refused connection: %s: %s", resp.Status, strings.TrimSpace(string(body)))
		}
		return nil, fmt.Errorf("dial proxy: %w", err)
	}
	c.SetReadLimit(1 << 20)
	return websocket.NetConn(context.Background(), c, websocket.MessageBinary), nil
}

func listDevices(ctx context.Context, cfg *config) error {
	hc, err := httpClient(cfg)
	if err != nil {
		return err
	}
	u, err := endpoint(cfg, "/devices", nil, false)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header = authHeader(cfg)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("proxy: " + resp.Status)
	}
	var devices []string
	if err := json.NewDecoder(resp.Body).Decode(&devices); err != nil {
		return err
	}
	for _, d := range devices {
		fmt.Println(d)
	}
	return nil
}

// pingDevice asks the proxy to round-trip a ping to the device's agent.
func pingDevice(ctx context.Context, hc *http.Client, cfg *config, device string) error {
	u, err := endpoint(cfg, "/ping", url.Values{"device": {device}}, false)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header = authHeader(cfg)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	// Drain so the connection is reused and later pings measure one round trip.
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return errors.New("proxy: " + resp.Status)
	}
	return nil
}
