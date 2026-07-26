package gatewayhttp

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNewServerDefaultsToBoundedLoopbackHTTP(t *testing.T) {
	t.Parallel()
	server, err := NewServer(ServerConfig{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if server.Address() != DefaultAddress || server.http.ReadHeaderTimeout != DefaultReadHeaderTimeout ||
		server.http.ReadTimeout != DefaultReadTimeout || server.http.WriteTimeout != DefaultWriteTimeout ||
		server.http.IdleTimeout != DefaultIdleTimeout || server.http.MaxHeaderBytes != DefaultMaxHeaderBytes ||
		server.http.TLSConfig != nil {
		t.Fatalf("NewServer() defaults = %+v", server.http)
	}
}

func TestNewServerRejectsUnsafeExposureAndInvalidBounds(t *testing.T) {
	t.Parallel()
	valid := ServerConfig{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}
	tests := []struct {
		name   string
		mutate func(*ServerConfig)
	}{
		{name: "missing handler", mutate: func(config *ServerConfig) { config.Handler = nil }},
		{name: "wildcard without TLS", mutate: func(config *ServerConfig) { config.Address = ":8080" }},
		{name: "external without TLS", mutate: func(config *ServerConfig) { config.Address = "192.0.2.10:8080" }},
		{name: "missing TLS key", mutate: func(config *ServerConfig) {
			config.Address = "192.0.2.10:8443"
			config.TLS = &TLSFiles{Certificate: "cert.pem"}
		}},
		{name: "invalid address", mutate: func(config *ServerConfig) { config.Address = "localhost" }},
		{name: "timeout above bound", mutate: func(config *ServerConfig) {
			config.ReadHeaderTimeout = HardMaxServerTimeout + time.Nanosecond
		}},
		{name: "header limit above bound", mutate: func(config *ServerConfig) {
			config.MaxHeaderBytes = HardMaxHeaderBytes + 1
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := valid
			test.mutate(&config)
			if _, err := NewServer(config); err == nil {
				t.Fatal("NewServer() accepted invalid configuration")
			}
		})
	}
}

func TestNewServerRequiresModernTLSOutsideLoopback(t *testing.T) {
	t.Parallel()
	server, err := NewServer(ServerConfig{
		Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		Address: "192.0.2.10:8443",
		TLS:     &TLSFiles{Certificate: "cert.pem", PrivateKey: "key.pem"},
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if server.http.TLSConfig == nil || server.http.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("TLS config = %+v", server.http.TLSConfig)
	}
}

func TestServerServesAndShutsDownCallerListener(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusAccepted)
		_, _ = response.Write([]byte(request.URL.Path))
	})
	server, err := NewServer(ServerConfig{Handler: handler})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + listener.Addr().String() + "/ready")
	if err != nil {
		t.Fatalf("Client.Get() error = %v", err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		t.Fatalf("reading response: %v", err)
	}
	if response.StatusCode != http.StatusAccepted || string(body) != "/ready" {
		t.Fatalf("response status = %d, body = %q", response.StatusCode, body)
	}
	shutdownContext, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
}

func TestServerRejectsCallerOwnedExternalPlainListener(t *testing.T) {
	t.Parallel()
	server, err := NewServer(ServerConfig{
		Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	listener := &fixedAddressListener{
		address: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 8080},
	}
	if err := server.Serve(listener); err == nil || !strings.Contains(err.Error(), "loopback listener") {
		t.Fatalf("Serve() error = %v, want loopback listener error", err)
	}
	if listener.accepted {
		t.Fatal("Serve() accepted connections before validating listener exposure")
	}
}

type fixedAddressListener struct {
	address  net.Addr
	accepted bool
}

func (l *fixedAddressListener) Accept() (net.Conn, error) {
	l.accepted = true
	return nil, errors.New("unexpected accept")
}

func (*fixedAddressListener) Close() error { return nil }

func (l *fixedAddressListener) Addr() net.Addr { return l.address }
