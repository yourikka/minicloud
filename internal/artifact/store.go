// Package artifact implements the local content-addressed artifact store.
package artifact

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"runtime"
	"strings"
	"sync"

	"github.com/yourikka/minicloud/internal/digest"
)

const (
	DefaultMaxArtifactBytes = int64(32 << 20)
	HardMaxArtifactBytes    = int64(256 << 20)
	copyBufferBytes         = 64 << 10
	randomNameBytes         = 16
)

var (
	ErrTooLarge       = errors.New("artifact exceeds size limit")
	ErrDigestMismatch = errors.New("artifact digest mismatch")
	ErrCorrupt        = errors.New("artifact content is corrupt")
	ErrClosed         = errors.New("artifact store is closed")
)

// Config controls the local store's trust root and upload bound.
type Config struct {
	Root             string
	MaxArtifactBytes int64
	Random           io.Reader
}

// Info describes one immutable content-addressed blob.
type Info struct {
	Digest  digest.SHA256
	Size    int64
	Created bool
}

// Store persists immutable blobs below one filesystem root.
type Store struct {
	root        *os.Root
	maxBytes    int64
	random      io.Reader
	randomMu    sync.Mutex
	digestLocks [256]sync.Mutex
	mu          sync.RWMutex
	closed      bool
}

// Open creates or opens a local artifact store.
func Open(config Config) (*Store, error) {
	if config.Root == "" {
		return nil, errors.New("artifact root is required")
	}
	maxBytes := config.MaxArtifactBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxArtifactBytes
	}
	if maxBytes < 1 || maxBytes > HardMaxArtifactBytes {
		return nil, errors.New("artifact size limit must be between 1 byte and 256 MiB")
	}
	randomSource := config.Random
	if randomSource == nil {
		randomSource = rand.Reader
	}

	if err := os.MkdirAll(config.Root, 0o700); err != nil {
		return nil, fmt.Errorf("creating artifact root: %w", err)
	}
	root, err := os.OpenRoot(config.Root)
	if err != nil {
		return nil, fmt.Errorf("opening artifact root: %w", err)
	}
	if err := prepareDirectories(root); err != nil {
		return nil, errors.Join(err, root.Close())
	}

	return &Store{
		root:     root,
		maxBytes: maxBytes,
		random:   randomSource,
	}, nil
}

// Close releases the filesystem root. It does not remove persisted blobs.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}
	s.closed = true
	if err := s.root.Close(); err != nil {
		return fmt.Errorf("closing artifact root: %w", err)
	}
	return nil
}

// Put streams a blob into a temporary file, verifies its digest, and atomically
// publishes it. Concurrent identical writes are idempotent.
func (s *Store) Put(
	ctx context.Context,
	expected digest.SHA256,
	source io.Reader,
) (info Info, err error) {
	if source == nil {
		return Info{}, errors.New("artifact source is required")
	}
	if err := validateDigest(expected); err != nil {
		return Info{}, err
	}
	if err := s.begin(); err != nil {
		return Info{}, err
	}
	defer s.end()

	temporary, temporaryName, err := s.createTemporary()
	if err != nil {
		return Info{}, err
	}
	temporaryOpen := true
	temporaryExists := true
	defer func() {
		if temporaryOpen {
			err = errors.Join(err, temporary.Close())
		}
		if temporaryExists {
			removeErr := s.root.Remove(temporaryName)
			if removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("removing temporary artifact: %w", removeErr))
			}
		}
	}()

	hasher := sha256.New()
	limited := io.LimitReader(&contextReader{ctx: ctx, source: source}, s.maxBytes+1)
	written, err := io.CopyBuffer(
		io.MultiWriter(temporary, hasher),
		limited,
		make([]byte, copyBufferBytes),
	)
	if err != nil {
		return Info{}, fmt.Errorf("writing temporary artifact: %w", err)
	}
	if written > s.maxBytes {
		return Info{}, ErrTooLarge
	}
	actual := digest.SHA256("sha256:" + hex.EncodeToString(hasher.Sum(nil)))
	if actual != expected {
		return Info{}, ErrDigestMismatch
	}
	if err := temporary.Sync(); err != nil {
		return Info{}, fmt.Errorf("syncing temporary artifact: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return Info{}, fmt.Errorf("closing temporary artifact: %w", err)
	}
	temporaryOpen = false

	digestLock := s.digestLock(expected)
	digestLock.Lock()
	defer digestLock.Unlock()

	blobName := blobPath(expected)
	if err := s.root.MkdirAll(path.Dir(blobName), 0o700); err != nil {
		return Info{}, fmt.Errorf("creating artifact shard: %w", err)
	}
	var recoveryErr error
	publishErr := s.root.Link(temporaryName, blobName)
	if errors.Is(publishErr, fs.ErrExist) {
		existingInfo, verifyErr := s.verifyLocked(ctx, expected)
		if verifyErr == nil {
			return Info{
				Digest:  existingInfo.Digest,
				Size:    existingInfo.Size,
				Created: false,
			}, nil
		}
		if !errors.Is(verifyErr, ErrCorrupt) {
			return Info{}, verifyErr
		}
		recoveryErr = verifyErr
		publishErr = s.root.Link(temporaryName, blobName)
	}
	if publishErr != nil {
		return Info{}, errors.Join(recoveryErr, fmt.Errorf("publishing artifact: %w", publishErr))
	}

	if err := syncDirectory(s.root, path.Dir(blobName)); err != nil {
		return Info{}, fmt.Errorf("syncing artifact directory: %w", err)
	}
	if err := s.root.Remove(temporaryName); err != nil {
		return Info{}, fmt.Errorf("removing published temporary artifact: %w", err)
	}
	temporaryExists = false

	return Info{Digest: expected, Size: written, Created: true}, nil
}

// OpenVerified opens a blob only after rechecking its size and SHA-256 bytes.
// The returned file is positioned at byte zero and must be closed by the caller.
func (s *Store) OpenVerified(
	ctx context.Context,
	expected digest.SHA256,
) (*os.File, Info, error) {
	if err := validateDigest(expected); err != nil {
		return nil, Info{}, err
	}
	if err := s.begin(); err != nil {
		return nil, Info{}, err
	}
	defer s.end()
	digestLock := s.digestLock(expected)
	digestLock.Lock()
	defer digestLock.Unlock()

	file, err := s.root.Open(blobPath(expected))
	if err != nil {
		return nil, Info{}, fmt.Errorf("opening artifact: %w", err)
	}
	info, err := verifyFile(ctx, file, expected, s.maxBytes)
	if err != nil {
		closeErr := file.Close()
		if errors.Is(err, ErrCorrupt) {
			return nil, Info{}, errors.Join(err, closeErr, s.removeCorrupt(expected))
		}
		return nil, Info{}, errors.Join(err, closeErr)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, Info{}, errors.Join(fmt.Errorf("rewinding artifact: %w", err), file.Close())
	}
	return file, info, nil
}

// verifyLocked verifies an existing path while the digest lock is held.
func (s *Store) verifyLocked(ctx context.Context, expected digest.SHA256) (Info, error) {
	file, err := s.root.Open(blobPath(expected))
	if err != nil {
		return Info{}, fmt.Errorf("opening existing artifact: %w", err)
	}
	info, verifyErr := verifyFile(ctx, file, expected, s.maxBytes)
	closeErr := file.Close()
	if verifyErr != nil {
		if errors.Is(verifyErr, ErrCorrupt) {
			return Info{}, errors.Join(verifyErr, closeErr, s.removeCorrupt(expected))
		}
		return Info{}, errors.Join(verifyErr, closeErr)
	}
	if closeErr != nil {
		return Info{}, fmt.Errorf("closing existing artifact: %w", closeErr)
	}
	return info, nil
}

// removeCorrupt removes the current blob while the digest lock is held.
func (s *Store) removeCorrupt(expected digest.SHA256) error {
	name := blobPath(expected)
	if err := s.root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing corrupt artifact: %w", err)
	}
	if err := syncDirectory(s.root, path.Dir(name)); err != nil {
		return fmt.Errorf("syncing artifact removal: %w", err)
	}
	return nil
}

func (s *Store) digestLock(value digest.SHA256) *sync.Mutex {
	hexDigest := strings.TrimPrefix(value.String(), "sha256:")
	index := hexNibble(hexDigest[0])<<4 | hexNibble(hexDigest[1])
	return &s.digestLocks[index]
}

func hexNibble(value byte) int {
	if value <= '9' {
		return int(value - '0')
	}
	return int(value-'a') + 10
}

func verifyFile(
	ctx context.Context,
	file *os.File,
	expected digest.SHA256,
	maxBytes int64,
) (Info, error) {
	hasher := sha256.New()
	read, err := io.CopyBuffer(
		hasher,
		io.LimitReader(&contextReader{ctx: ctx, source: file}, maxBytes+1),
		make([]byte, copyBufferBytes),
	)
	if err != nil {
		return Info{}, fmt.Errorf("verifying artifact: %w", err)
	}
	if read > maxBytes {
		return Info{}, ErrCorrupt
	}
	actual := digest.SHA256("sha256:" + hex.EncodeToString(hasher.Sum(nil)))
	if actual != expected {
		return Info{}, ErrCorrupt
	}
	return Info{Digest: expected, Size: read}, nil
}

func (s *Store) begin() error {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return ErrClosed
	}
	return nil
}

func (s *Store) end() {
	s.mu.RUnlock()
}

func (s *Store) createTemporary() (*os.File, string, error) {
	for range 10 {
		nameBytes := make([]byte, randomNameBytes)
		if err := s.readRandom(nameBytes); err != nil {
			return nil, "", fmt.Errorf("generating temporary artifact name: %w", err)
		}
		name := path.Join("tmp", hex.EncodeToString(nameBytes)+".upload")
		file, err := s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			return file, name, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", fmt.Errorf("creating temporary artifact: %w", err)
		}
	}
	return nil, "", errors.New("temporary artifact name collisions exhausted")
}

func (s *Store) readRandom(buffer []byte) error {
	s.randomMu.Lock()
	defer s.randomMu.Unlock()

	_, err := io.ReadFull(s.random, buffer)
	return err
}

func prepareDirectories(root *os.Root) error {
	for _, directory := range []string{"tmp", "blobs/sha256"} {
		if err := root.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("creating artifact directory: %w", err)
		}
	}
	return nil
}

func validateDigest(value digest.SHA256) error {
	if _, err := digest.ParseSHA256(value.String()); err != nil {
		return fmt.Errorf("validating artifact digest: %w", err)
	}
	return nil
}

func blobPath(value digest.SHA256) string {
	hexDigest := strings.TrimPrefix(value.String(), "sha256:")
	return path.Join("blobs/sha256", hexDigest[:2], hexDigest[2:])
}

func syncDirectory(root *os.Root, name string) error {
	directory, err := root.Open(name)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if runtime.GOOS == "windows" {
		syncErr = nil
	}
	return errors.Join(syncErr, closeErr)
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
