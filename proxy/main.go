// Command proxy relays SSH sessions between sshtun clients and device agents.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"strings"

	"sshoverhttp/proxy/internal/proxy"
)

func main() {
	listen := flag.String("listen", ":8443", "listen address")
	cert := flag.String("cert", "", "TLS certificate (PEM); serves plain HTTP when empty")
	key := flag.String("key", "", "TLS private key (PEM)")
	flag.Parse()

	cfg := proxy.Config{
		AgentTokens:  parseAgentTokens(os.Getenv("AGENT_TOKENS")),
		ClientTokens: splitList(os.Getenv("CLIENT_TOKENS")),
	}
	if len(cfg.AgentTokens) == 0 || len(cfg.ClientTokens) == 0 {
		log.Fatal("AGENT_TOKENS (device=token,...) and CLIENT_TOKENS (token,...) must be set")
	}

	srv := &http.Server{Addr: *listen, Handler: proxy.New(cfg).Handler()}
	log.Printf("proxy listening on %s (tls=%t, devices=%d)", *listen, *cert != "", len(cfg.AgentTokens))
	var err error
	if *cert != "" {
		err = srv.ListenAndServeTLS(*cert, *key)
	} else {
		err = srv.ListenAndServe()
	}
	log.Fatal(err)
}

// parseAgentTokens parses "dev1=tok1,dev2=tok2".
func parseAgentTokens(s string) map[string]string {
	m := make(map[string]string)
	for _, kv := range splitList(s) {
		if device, token, ok := strings.Cut(kv, "="); ok && device != "" && token != "" {
			m[device] = token
		}
	}
	return m
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
