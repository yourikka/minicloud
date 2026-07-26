package gatewayhttp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	DefaultAddress           = "127.0.0.1:8080"
	DefaultReadHeaderTimeout = 5 * time.Second
	DefaultReadTimeout       = 15 * time.Second
	DefaultWriteTimeout      = 15 * time.Second
	DefaultIdleTimeout       = 60 * time.Second
	DefaultShutdownTimeout   = 10 * time.Second
	DefaultMaxHeaderBytes    = 64 << 10
	HardMaxServerTimeout     = 2 * time.Minute
	HardMaxHeaderBytes       = 1 << 20
)

// TLSFiles contains one server certificate chain and private key pair.
type TLSFiles struct {
	Certificate string
	PrivateKey  string
}

// ServerConfig bounds a public HTTP listener around an invocation Handler.
type ServerConfig struct {
	Handler           http.Handler
	Address           string
	TLS               *TLSFiles
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	MaxHeaderBytes    int
}

// Server owns one hardened HTTP server and its graceful shutdown policy.
type Server struct {
	http            *http.Server
	tls             *TLSFiles
	shutdownTimeout time.Duration
}

// NewServer validates listener exposure and fills every HTTP resource bound.
// Plain HTTP is accepted only on an explicit loopback address.
func NewServer(config ServerConfig) (*Server, error) {
	if config.Handler == nil {
		return nil, errors.New("gateway HTTP server handler is required")
	}
	if config.Address == "" {
		config.Address = DefaultAddress
	}
	if config.ReadHeaderTimeout == 0 {
		config.ReadHeaderTimeout = DefaultReadHeaderTimeout
	}
	if config.ReadTimeout == 0 {
		config.ReadTimeout = DefaultReadTimeout
	}
	if config.WriteTimeout == 0 {
		config.WriteTimeout = DefaultWriteTimeout
	}
	if config.IdleTimeout == 0 {
		config.IdleTimeout = DefaultIdleTimeout
	}
	if config.ShutdownTimeout == 0 {
		config.ShutdownTimeout = DefaultShutdownTimeout
	}
	if config.MaxHeaderBytes == 0 {
		config.MaxHeaderBytes = DefaultMaxHeaderBytes
	}
	if err := validateServerConfig(config); err != nil {
		return nil, err
	}
	var tlsFiles *TLSFiles
	var tlsConfig *tls.Config
	if config.TLS != nil {
		files := *config.TLS
		tlsFiles = &files
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return &Server{
		http: &http.Server{
			Addr:              config.Address,
			Handler:           config.Handler,
			ReadHeaderTimeout: config.ReadHeaderTimeout,
			ReadTimeout:       config.ReadTimeout,
			WriteTimeout:      config.WriteTimeout,
			IdleTimeout:       config.IdleTimeout,
			MaxHeaderBytes:    config.MaxHeaderBytes,
			TLSConfig:         tlsConfig,
		},
		tls:             tlsFiles,
		shutdownTimeout: config.ShutdownTimeout,
	}, nil
}

// ListenAndServe owns its listener until the context is cancelled or serving
// fails. Cancellation triggers a bounded graceful shutdown.
func (s *Server) ListenAndServe(ctx context.Context) error {
	if ctx == nil {
		return errors.New("gateway HTTP server context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.http == nil {
		return errors.New("gateway HTTP server dependencies are required")
	}
	listener, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return fmt.Errorf("listening for gateway HTTP requests: %w", err)
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- s.Serve(listener)
	}()
	select {
	case err := <-serveDone:
		return err
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		defer cancel()
		shutdownErr := s.http.Shutdown(shutdownContext)
		serveErr := <-serveDone
		return errors.Join(shutdownErr, serveErr)
	}
}

// Serve runs on a caller-owned listener and selects TLS from the immutable
// constructor configuration.
func (s *Server) Serve(listener net.Listener) error {
	if s == nil || s.http == nil {
		return errors.New("gateway HTTP server dependencies are required")
	}
	if listener == nil {
		return errors.New("gateway HTTP listener is required")
	}
	if s.tls == nil && !loopbackListener(listener.Addr()) {
		return errors.New("gateway HTTP server requires a loopback listener without TLS")
	}
	var err error
	if s.tls == nil {
		err = s.http.Serve(listener)
	} else {
		err = s.http.ServeTLS(listener, s.tls.Certificate, s.tls.PrivateKey)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func loopbackListener(address net.Addr) bool {
	if address == nil || address.Network() != "tcp" {
		return false
	}
	host, _, err := net.SplitHostPort(address.String())
	return err == nil && loopbackHost(host)
}

// Shutdown gracefully stops the server within its configured bound.
func (s *Server) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("gateway HTTP shutdown context is required")
	}
	if s == nil || s.http == nil {
		return errors.New("gateway HTTP server dependencies are required")
	}
	return s.http.Shutdown(ctx)
}

// Address returns the configured listen address without opening a socket.
func (s *Server) Address() string {
	if s == nil || s.http == nil {
		return ""
	}
	return s.http.Addr
}

func validateServerConfig(config ServerConfig) error {
	host, port, err := net.SplitHostPort(config.Address)
	if err != nil || port == "" {
		return errors.New("gateway HTTP server address must contain a host and port")
	}
	if !loopbackHost(host) && config.TLS == nil {
		return errors.New("gateway HTTP server requires TLS outside loopback")
	}
	if config.TLS != nil && (config.TLS.Certificate == "" || config.TLS.PrivateKey == "") {
		return errors.New("gateway HTTP server TLS certificate and private key are required together")
	}
	timeouts := []time.Duration{
		config.ReadHeaderTimeout,
		config.ReadTimeout,
		config.WriteTimeout,
		config.IdleTimeout,
		config.ShutdownTimeout,
	}
	for _, timeout := range timeouts {
		if timeout <= 0 || timeout > HardMaxServerTimeout {
			return errors.New("gateway HTTP server timeout is outside v1 bounds")
		}
	}
	if config.MaxHeaderBytes < 1 || config.MaxHeaderBytes > HardMaxHeaderBytes {
		return errors.New("gateway HTTP server header limit is outside v1 bounds")
	}
	return nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}
