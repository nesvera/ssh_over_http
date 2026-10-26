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

// dialTunnel returns a byte stream to the device's SSH server.
func dialTunnel(ctx context.Context, cfg *config, device string) (net.Conn, error) {
	hc, err := httpClient(cfg)
	if err != nil {
		return nil, err
	}
	u, err := endpoint(cfg, "/connect", url.Values{"device": {device}}, true)
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
