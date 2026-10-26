package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestAgentInterop runs the real Node.js agent against the proxy, with a TCP
// echo server standing in for sshd, and pushes several large transfers
// through concurrently to exercise multiplexing and flow control.
func TestAgentInterop(t *testing.T) {
	baseURL := startStack(t, startEcho(t))

	t.Run("rejects bad client token", func(t *testing.T) {
		_, resp, err := dial(baseURL, "dev1", "wrong")
		if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("want 401, got resp=%v err=%v", resp, err)
		}
	})

	t.Run("unknown device", func(t *testing.T) {
		_, resp, err := dial(baseURL, "nope", "client-token")
		if err == nil || resp == nil || resp.StatusCode != http.StatusNotFound {
			t.Fatalf("want 404, got resp=%v err=%v", resp, err)
		}
	})

	t.Run("concurrent bulk echo", func(t *testing.T) {
		const streams, size = 8, 4 << 20
		var wg sync.WaitGroup
		for i := range streams {
			wg.Add(1)
			go func() {
				defer wg.Done()
				conn, _, err := dial(baseURL, "dev1", "client-token")
				if err != nil {
					t.Errorf("stream %d: dial: %v", i, err)
					return
				}
				defer conn.Close()

				want := make([]byte, size)
				rand.Read(want)
				go conn.Write(want)
				got := make([]byte, size)
				if _, err := io.ReadFull(conn, got); err != nil {
					t.Errorf("stream %d: read: %v", i, err)
					return
				}
				if !bytes.Equal(got, want) {
					t.Errorf("stream %d: echoed data mismatch", i)
				}
			}()
		}
		wg.Wait()
	})
}

// startStack runs the proxy plus the Node.js agent, whose "sshd" is whatever
// listens on target, and returns the proxy's base URL once the agent is up.
func startStack(t *testing.T, target net.Listener) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	agentDir, _ := filepath.Abs("../../../target/agent")
	if _, err := os.Stat(filepath.Join(agentDir, "node_modules", "ws")); err != nil {
		t.Skip("agent dependencies not installed (run npm install in target/agent/)")
	}

	srv := New(Config{
		AgentTokens:  map[string]string{"dev1": "agent-token"},
		ClientTokens: []string{"client-token"},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	_, port, _ := net.SplitHostPort(target.Addr().String())
	agent := exec.Command(node, filepath.Join(agentDir, "src", "index.js"))
	agent.Env = append(os.Environ(),
		"PROXY_URL=ws"+strings.TrimPrefix(ts.URL, "http"),
		"DEVICE_ID=dev1",
		"AGENT_TOKEN=agent-token",
		"SSH_PORT="+port,
	)
	agent.Stdout, agent.Stderr = testWriter{t}, testWriter{t}
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Process.Kill(); agent.Wait() })

	deadline := time.Now().Add(10 * time.Second)
	for srv.lookup("dev1") == nil {
		if time.Now().After(deadline) {
			t.Fatal("agent never registered")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return ts.URL
}

func dial(baseURL, device, token string) (net.Conn, *http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	u := "ws" + strings.TrimPrefix(baseURL, "http") + "/connect?device=" + device
	c, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
	})
	if err != nil {
		return nil, resp, err
	}
	c.SetReadLimit(clientMaxFrame)
	return websocket.NetConn(context.Background(), c, websocket.MessageBinary), resp, nil
}

func startEcho(t *testing.T) net.Listener {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return ln
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
