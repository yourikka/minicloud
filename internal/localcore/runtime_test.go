package localcore

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/gatewayhttp"
	"github.com/yourikka/minicloud/internal/problem"
)

func TestRuntimeServesEmptySynchronizedViewAndCloses(t *testing.T) {
	t.Parallel()
	command, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	runtime, err := New(t.Context(), Config{
		DataRoot:         t.TempDir(),
		ValidatorCommand: command,
		SyncInterval:     10 * time.Millisecond,
		HTTP:             serverConfig("127.0.0.1:0"),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if runtime.Controller() == nil || runtime.Handler() == nil {
		t.Fatal("New() did not expose composed controller and invocation handler")
	}
	serveContext, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() {
		cancel()
		closeContext, closeCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer closeCancel()
		_ = runtime.Close(closeContext)
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- runtime.Serve(serveContext, listener) }()

	client := &http.Client{Timeout: time.Second}
	response := getEventually(t, client, "http://"+listener.Addr().String()+"/invoke/missing/")
	defer response.Body.Close()
	var envelope problem.Envelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if response.StatusCode != http.StatusNotFound || envelope.Error.Code != problem.CodeNotFound {
		t.Fatalf("response status = %d, envelope = %+v", response.StatusCode, envelope)
	}
	if err := runtime.Close(t.Context()); err == nil {
		t.Fatal("Close() accepted a runtime that is still serving")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve() did not stop after cancellation")
	}
	for range 2 {
		closeContext, closeCancel := context.WithTimeout(t.Context(), 3*time.Second)
		if err := runtime.Close(closeContext); err != nil {
			closeCancel()
			t.Fatalf("Close() error = %v", err)
		}
		closeCancel()
	}
	if err := runtime.Converge(t.Context()); err == nil {
		t.Fatal("Converge() accepted a closed runtime")
	}
}

func TestNewRejectsMissingRequiredProcessConfig(t *testing.T) {
	t.Parallel()
	if _, err := New(t.Context(), Config{}); err == nil {
		t.Fatal("New() accepted missing data root and validator command")
	}
}

func TestRuntimeServesAuthenticatedManagementBoundary(t *testing.T) {
	t.Parallel()
	command, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	token := "0123456789abcdef0123456789abcdef"
	runtime, err := New(t.Context(), Config{
		DataRoot:         t.TempDir(),
		ValidatorCommand: command,
		SyncInterval:     10 * time.Millisecond,
		HTTP:             serverConfig("127.0.0.1:0"),
		Management: ManagementConfig{
			HTTP:  serverConfig("127.0.0.1:0"),
			Token: token,
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	serveContext, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() {
		cancel()
		closeContext, closeCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer closeCancel()
		_ = runtime.Close(closeContext)
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- runtime.Serve(serveContext, listener) }()

	address := managementAddressEventually(t, runtime)
	client := &http.Client{Timeout: time.Second}
	profileURL := "http://" + address + "/v1/profile"

	unauthenticated, err := client.Get(profileURL)
	if err != nil {
		t.Fatalf("GET %s error = %v", profileURL, err)
	}
	defer unauthenticated.Body.Close()
	if unauthenticated.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated profile status = %d, want 401", unauthenticated.StatusCode)
	}

	request, err := http.NewRequest(http.MethodGet, profileURL, nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	authenticated, err := client.Do(request)
	if err != nil {
		t.Fatalf("GET %s error = %v", profileURL, err)
	}
	defer authenticated.Body.Close()
	var profile struct {
		Profile    string `json:"profile"`
		Replicated bool   `json:"replicated"`
	}
	if err := json.NewDecoder(authenticated.Body).Decode(&profile); err != nil {
		t.Fatalf("decoding profile: %v", err)
	}
	if authenticated.StatusCode != http.StatusOK || profile.Profile != "local-core" || profile.Replicated {
		t.Fatalf("profile = (%d, %+v), want the explicit local-core profile", authenticated.StatusCode, profile)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve() did not stop after cancellation")
	}
	if runtime.ManagementAddress() != "" {
		t.Fatal("ManagementAddress() should be empty after serving stops")
	}
}

func TestNewRejectsManagementConfigWithoutToken(t *testing.T) {
	t.Parallel()
	command, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	_, err = New(t.Context(), Config{
		DataRoot:         t.TempDir(),
		ValidatorCommand: command,
		Management:       ManagementConfig{HTTP: serverConfig("127.0.0.1:0")},
	})
	if err == nil {
		t.Fatal("New() accepted a management listener without a token")
	}
}

func managementAddressEventually(t *testing.T, runtime *Runtime) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if address := runtime.ManagementAddress(); address != "" {
			return address
		}
		if time.Now().After(deadline) {
			t.Fatal("management listener did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func serverConfig(address string) gatewayhttp.ServerConfig {
	return gatewayhttp.ServerConfig{Address: address, ShutdownTimeout: time.Second}
}

func getEventually(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		response, err := client.Get(url)
		if err == nil {
			return response
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s did not become ready: %v", url, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
