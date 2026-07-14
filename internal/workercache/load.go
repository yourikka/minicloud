package workercache

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/yourikka/minicloud/internal/artifact"
	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/wasmexec"
	"github.com/yourikka/minicloud/internal/wasmprofile"
	abi "github.com/yourikka/minicloud/sdk/go/minicloudabi"
)

// ArtifactSource returns bytes that were verified and rewound by the CAS.
type ArtifactSource interface {
	OpenVerified(context.Context, digest.SHA256) (*os.File, artifact.Info, error)
}

// Compiler is implemented by wasmexec.Engine.
type Compiler interface {
	Compile(context.Context, []byte) (*wasmexec.Program, wasmexec.Metrics, error)
	Profile() wasmprofile.Profile
}

// InvocationRequest groups the typed ABI input with its host timeout.
type InvocationRequest struct {
	Request abi.Request
	Timeout time.Duration
}

func (c *Cache) loadAndCompile(
	ctx context.Context,
	key Key,
	spec ModuleSpec,
) (*wasmexec.Program, Result, error) {
	result := Result{Key: key}
	loadStarted := time.Now()
	var wasm []byte
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		wasm, err = c.readVerifiedArtifact(ctx, spec)
		if err == nil || !errors.Is(err, artifact.ErrCorrupt) {
			break
		}
	}
	result.ArtifactLoad = time.Since(loadStarted)
	if err != nil {
		return nil, result, err
	}
	program, metrics, err := c.compiler.Compile(ctx, wasm)
	result.Compile = metrics.Compile
	if err != nil {
		if program != nil {
			return nil, result, errors.Join(err, program.Close(context.Background()))
		}
		return nil, result, err
	}
	if program == nil {
		return nil, result, errors.New("worker cache compiler returned no program")
	}
	return program, result, nil
}

func (c *Cache) readVerifiedArtifact(
	ctx context.Context,
	spec ModuleSpec,
) ([]byte, error) {
	file, info, err := c.artifacts.OpenVerified(ctx, spec.ArtifactDigest)
	if err != nil {
		if file != nil {
			return nil, errors.Join(err, file.Close())
		}
		return nil, err
	}
	if file == nil {
		return nil, errors.New("worker cache artifact source returned no file")
	}
	wasm, readErr := io.ReadAll(io.LimitReader(
		&contextReader{ctx: ctx, source: file},
		spec.ArtifactSize+1,
	))
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, err
	}
	metadataMatches := info.Digest == spec.ArtifactDigest && info.Size == spec.ArtifactSize
	bytesMatch := int64(len(wasm)) == spec.ArtifactSize && digest.Sum(wasm) == spec.ArtifactDigest
	if !metadataMatches || !bytesMatch {
		return nil, artifact.ErrCorrupt
	}
	return wasm, nil
}

type contextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(buffer)
}
