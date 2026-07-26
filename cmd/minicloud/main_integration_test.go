//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/problem"
)

func TestProcessServesAndStopsOnInterrupt(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repository root: %v", err)
	}
	binDirectory := t.TempDir()
	serverPath := filepath.Join(binDirectory, "minicloud")
	validatorPath := filepath.Join(binDirectory, "minicloud-validator")
	buildBinary(t, root, serverPath, "./cmd/minicloud")
	buildBinary(t, root, validatorPath, "./cmd/minicloud-validator")

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving process address: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("closing reserved address: %v", err)
	}
	command := exec.Command(
		serverPath,
		"-data-dir", filepath.Join(t.TempDir(), "data"),
		"-listen", address,
		"-validator", validatorPath,
		"-sync-interval", "100ms",
	)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatalf("starting minicloud process: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			<-exited
		}
	})

	client := &http.Client{Timeout: time.Second}
	response := processGetEventually(t, client, "http://"+address+"/invoke/missing/")
	var envelope problem.Envelope
	decodeErr := json.NewDecoder(response.Body).Decode(&envelope)
	closeErr := response.Body.Close()
	if decodeErr != nil || closeErr != nil {
		t.Fatalf("reading process response: decode=%v close=%v", decodeErr, closeErr)
	}
	if response.StatusCode != http.StatusNotFound || envelope.Error.Code != problem.CodeNotFound {
		t.Fatalf("response status = %d, envelope = %+v", response.StatusCode, envelope)
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("interrupting minicloud process: %v", err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("minicloud process exit = %v, stderr = %q", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("minicloud process did not stop, stderr = %q", stderr.String())
	}
}

func buildBinary(t *testing.T, root, output, packagePath string) {
	t.Helper()
	command := exec.Command("go", "build", "-trimpath", "-o", output, packagePath)
	command.Dir = root
	if data, err := command.CombinedOutput(); err != nil {
		t.Fatalf("building %s: %v\n%s", packagePath, err, data)
	}
}

func processGetEventually(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := client.Get(url)
		if err == nil {
			return response
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s did not become ready: %v", url, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
