// Package validator supervises disposable validator subprocesses.
package validator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/yourikka/minicloud/internal/validator/protocol"
)

const (
	DefaultDeadline       = 30 * time.Second
	DefaultMaxConcurrent  = 2
	DefaultMaxStderrBytes = 16 << 10
	defaultCPUSeconds     = uint64(30)
	defaultMaxFileBytes   = uint64(512 << 20)
	defaultMaxOpenFiles   = uint64(64)
)

var (
	ErrOverloaded    = errors.New("validator is at concurrency limit")
	ErrTimedOut      = errors.New("validator process timed out")
	ErrProcessFailed = errors.New("validator process failed")
	ErrInvalidReport = errors.New("validator returned an invalid report")
	ErrOutputLimit   = errors.New("validator output exceeds limit")
)

// Config controls the external watchdog. Memory and aggregate temp-disk hard
// limits remain the responsibility of the Linux cgroup/tmpfs supervisor.
type Config struct {
	Command          string
	TempRoot         string
	Deadline         time.Duration
	MaxConcurrent    int
	MaxArtifactBytes int64
	MaxStderrBytes   int
}

// Client is safe for concurrent use.
type Client struct {
	command          string
	tempRoot         string
	deadline         time.Duration
	maxArtifactBytes int64
	maxStderrBytes   int
	slots            chan struct{}
}

// New resolves the child executable once and validates all hard bounds.
func New(config Config) (*Client, error) {
	if config.Command == "" {
		return nil, errors.New("validator command is required")
	}
	command, err := exec.LookPath(config.Command)
	if err != nil {
		return nil, fmt.Errorf("resolving validator command: %w", err)
	}
	command, err = filepath.Abs(command)
	if err != nil {
		return nil, fmt.Errorf("resolving absolute validator command: %w", err)
	}
	if config.Deadline == 0 {
		config.Deadline = DefaultDeadline
	}
	if config.MaxConcurrent == 0 {
		config.MaxConcurrent = DefaultMaxConcurrent
	}
	if config.MaxArtifactBytes == 0 {
		config.MaxArtifactBytes = protocol.DefaultArtifactBytes
	}
	if config.MaxStderrBytes == 0 {
		config.MaxStderrBytes = DefaultMaxStderrBytes
	}
	if config.Deadline < time.Second || config.Deadline > DefaultDeadline {
		return nil, errors.New("validator deadline must be between 1s and 30s")
	}
	if config.MaxConcurrent < 1 || config.MaxConcurrent > DefaultMaxConcurrent {
		return nil, errors.New("validator concurrency must be between 1 and 2")
	}
	if config.MaxArtifactBytes < 1 || config.MaxArtifactBytes > protocol.HardArtifactBytes {
		return nil, errors.New("validator artifact limit is outside v1 bounds")
	}
	if config.MaxStderrBytes < 1 || config.MaxStderrBytes > DefaultMaxStderrBytes {
		return nil, errors.New("validator stderr limit is outside v1 bounds")
	}
	if config.TempRoot != "" {
		if err := os.MkdirAll(config.TempRoot, 0o700); err != nil {
			return nil, fmt.Errorf("creating validator temp root: %w", err)
		}
	}

	return &Client{
		command:          command,
		tempRoot:         config.TempRoot,
		deadline:         config.Deadline,
		maxArtifactBytes: config.MaxArtifactBytes,
		maxStderrBytes:   config.MaxStderrBytes,
		slots:            make(chan struct{}, config.MaxConcurrent),
	}, nil
}

// Validate runs one actual compilation in a disposable child process. It takes
// ownership of artifact and closes it on every return path so cancellation can
// interrupt the parent-side stdin pump.
func (c *Client) Validate(
	ctx context.Context,
	request protocol.Request,
	artifact io.ReadCloser,
) (report protocol.Report, err error) {
	if ctx == nil {
		return protocol.Report{}, errors.New("validator context is required")
	}
	if artifact == nil {
		return protocol.Report{}, errors.New("validator artifact is required")
	}
	defer func() {
		err = errors.Join(err, artifact.Close())
	}()
	if err := request.Validate(); err != nil {
		return protocol.Report{}, err
	}
	if request.ArtifactSize > c.maxArtifactBytes {
		return protocol.Report{}, errors.New("validator artifact exceeds client limit")
	}
	select {
	case <-ctx.Done():
		return protocol.Report{}, fmt.Errorf("validator canceled before start: %w", ctx.Err())
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	default:
		return protocol.Report{}, ErrOverloaded
	}

	stdin, err := protocol.NewRequestReader(request, artifact)
	if err != nil {
		return protocol.Report{}, err
	}
	tempDirectory, err := os.MkdirTemp(c.tempRoot, "minicloud-validator-")
	if err != nil {
		return protocol.Report{}, fmt.Errorf("creating validator temp directory: %w", err)
	}
	defer func() {
		err = errors.Join(err, removeTempDirectory(tempDirectory))
	}()

	childContext, cancel := context.WithTimeout(ctx, c.deadline)
	defer cancel()
	command := exec.CommandContext(
		childContext,
		c.command,
		"-max-artifact-bytes", fmt.Sprint(c.maxArtifactBytes),
		"-deadline", c.deadline.String(),
		"-cpu-seconds", fmt.Sprint(defaultCPUSeconds),
		"-max-file-bytes", fmt.Sprint(defaultMaxFileBytes),
		"-max-open-files", fmt.Sprint(defaultMaxOpenFiles),
	)
	configureProcess(command)
	command.Cancel = func() error { return killProcessGroup(command) }
	command.WaitDelay = time.Second
	command.Env = childEnvironment(tempDirectory)
	stdout := newLimitedBuffer(protocol.MaxReportBytes)
	stderr := newLimitedBuffer(c.maxStderrBytes)
	command.Stdout = stdout
	command.Stderr = stderr
	childStdin, err := command.StdinPipe()
	if err != nil {
		return protocol.Report{}, fmt.Errorf("creating validator stdin: %w", err)
	}

	if err := command.Start(); err != nil {
		return protocol.Report{}, fmt.Errorf("starting validator process: %w", err)
	}
	copyDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(childStdin, stdin)
		copyDone <- errors.Join(copyErr, childStdin.Close())
	}()
	runErr := command.Wait()
	if childContext.Err() != nil {
		if ctx.Err() != nil {
			return protocol.Report{}, fmt.Errorf("validator canceled by caller: %w", ctx.Err())
		}
		return protocol.Report{}, errors.Join(ErrTimedOut, childContext.Err())
	}
	if runErr != nil {
		if errors.Is(stdout.err, ErrOutputLimit) || errors.Is(stderr.err, ErrOutputLimit) {
			return protocol.Report{}, errors.Join(ErrProcessFailed, ErrOutputLimit)
		}
		return protocol.Report{}, ErrProcessFailed
	}
	if copyErr := <-copyDone; copyErr != nil {
		return protocol.Report{}, fmt.Errorf("sending artifact to validator: %w", copyErr)
	}

	report, err = protocol.DecodeReport(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		return protocol.Report{}, errors.Join(ErrInvalidReport, err)
	}
	identityMatches := report.ValidationID == request.ValidationID
	artifactMatches := report.ArtifactDigest == request.ArtifactDigest && report.ArtifactSize == request.ArtifactSize
	if !identityMatches || !artifactMatches {
		return protocol.Report{}, ErrInvalidReport
	}
	return report, nil
}

func removeTempDirectory(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("removing validator temp directory: %w", err)
	}
	return nil
}

func childEnvironment(tempDirectory string) []string {
	environment := []string{
		"HOME=" + tempDirectory,
		"LANG=C",
		"LC_ALL=C",
		"TMPDIR=" + tempDirectory,
		"TEMP=" + tempDirectory,
		"TMP=" + tempDirectory,
		"GOMEMLIMIT=384MiB",
	}
	if runtime.GOOS != "windows" {
		return environment
	}
	for _, entry := range os.Environ() {
		name, _, found := strings.Cut(entry, "=")
		if found && strings.EqualFold(name, "SYSTEMROOT") {
			environment = append(environment, entry)
		}
	}
	return environment
}

type limitedBuffer struct {
	buffer bytes.Buffer
	limit  int
	err    error
}

func newLimitedBuffer(limit int) *limitedBuffer {
	return &limitedBuffer{limit: limit}
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		b.err = ErrOutputLimit
		return 0, b.err
	}
	if len(data) > remaining {
		written, writeErr := b.buffer.Write(data[:remaining])
		b.err = errors.Join(ErrOutputLimit, writeErr)
		return written, b.err
	}
	return b.buffer.Write(data)
}

func (b *limitedBuffer) Bytes() []byte {
	return bytes.Clone(b.buffer.Bytes())
}
