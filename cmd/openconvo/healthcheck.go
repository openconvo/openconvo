package main

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/openconvo/openconvo/internal/config"
)

// runHealthcheck probes the local server's /health endpoint. It exists
// so the Docker HEALTHCHECK needs no curl/wget in the runtime image.
// The address comes from the same configuration the server binds to, so
// the probe cannot drift from the running process.
func runHealthcheck() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	addr := net.JoinHostPort(dialHost(cfg.Host), strconv.Itoa(cfg.Port))
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s/health", addr))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health endpoint returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// dialHost returns the address to probe for a server bound to host. A
// server on every interface (the default) is reachable over loopback; one
// bound to a single address is reachable only there.
func dialHost(host string) string {
	switch host {
	case "", "0.0.0.0", "::":
		return "127.0.0.1"
	}
	return host
}
