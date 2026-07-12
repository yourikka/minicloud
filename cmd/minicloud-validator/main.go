package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/yourikka/minicloud/internal/validator/engine"
	"github.com/yourikka/minicloud/internal/validator/oslimit"
	"github.com/yourikka/minicloud/internal/validator/protocol"
)

const (
	defaultDeadline     = 30 * time.Second
	defaultMaxFileBytes = uint64(512 << 20)
	defaultMaxOpenFiles = uint64(64)
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("minicloud-validator", flag.ContinueOnError)
	flags.SetOutput(stderr)
	maxArtifactBytes := flags.Int64(
		"max-artifact-bytes",
		protocol.DefaultArtifactBytes,
		"maximum artifact bytes accepted from the parent",
	)
	deadline := flags.Duration("deadline", defaultDeadline, "validator process deadline")
	cpuSeconds := flags.Uint64("cpu-seconds", uint64(defaultDeadline/time.Second), "linux cpu hard limit")
	maxFileBytes := flags.Uint64("max-file-bytes", defaultMaxFileBytes, "linux per-file hard limit")
	maxOpenFiles := flags.Uint64("max-open-files", defaultMaxOpenFiles, "linux open file hard limit")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *deadline <= 0 || *deadline > defaultDeadline {
		writeInternalError(stderr)
		return 2
	}
	invalidCPU := *cpuSeconds == 0 || *cpuSeconds > uint64(defaultDeadline/time.Second)
	invalidFileLimit := *maxFileBytes == 0 || *maxFileBytes > defaultMaxFileBytes
	invalidOpenFiles := *maxOpenFiles < 16 || *maxOpenFiles > defaultMaxOpenFiles
	if invalidCPU || invalidFileLimit || invalidOpenFiles {
		writeInternalError(stderr)
		return 2
	}

	limitResult, err := oslimit.Apply(oslimit.Config{
		CPUSeconds:   *cpuSeconds,
		MaxFileBytes: *maxFileBytes,
		MaxOpenFiles: *maxOpenFiles,
	})
	if err != nil {
		writeInternalError(stderr)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), *deadline)
	defer cancel()
	request, wasm, err := protocol.ReadRequest(stdin, *maxArtifactBytes)
	if err != nil {
		writeInternalError(stderr)
		return 1
	}
	report, err := engine.Validate(ctx, request, wasm)
	if err != nil {
		writeInternalError(stderr)
		return 1
	}
	report.Isolation = protocol.Isolation{
		ProcessBoundary: true,
		Deadline:        true,
		CPULimit:        limitResult.CPULimit,
		FileSizeLimit:   limitResult.FileSizeLimit,
		MemoryLimit:     false,
		TempDiskLimit:   false,
	}
	encoded, err := protocol.EncodeReport(report)
	if err != nil {
		writeInternalError(stderr)
		return 1
	}
	written, err := stdout.Write(encoded)
	if err != nil || written != len(encoded) {
		writeInternalError(stderr)
		return 1
	}
	return 0
}

func writeInternalError(stderr io.Writer) {
	if _, err := fmt.Fprintln(stderr, "validator: internal failure"); err != nil {
		return
	}
}
