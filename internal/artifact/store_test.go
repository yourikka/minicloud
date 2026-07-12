package artifact

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/yourikka/minicloud/internal/digest"
)

func TestStorePutAndOpenVerified(t *testing.T) {
	t.Parallel()

	store, root := openTestStore(t, 1<<20)
	payload := []byte("a valid wasm artifact")
	wantDigest := digest.Sum(payload)

	putInfo, err := store.Put(context.Background(), wantDigest, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if putInfo.Digest != wantDigest {
		t.Errorf("Put() digest = %q, want %q", putInfo.Digest, wantDigest)
	}
	if putInfo.Size != int64(len(payload)) {
		t.Errorf("Put() size = %d, want %d", putInfo.Size, len(payload))
	}
	if !putInfo.Created {
		t.Error("Put() created = false, want true for a new blob")
	}

	file, openInfo, err := store.OpenVerified(context.Background(), wantDigest)
	if err != nil {
		t.Fatalf("OpenVerified() error = %v", err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Errorf("closing verified artifact: %v", err)
		}
	})

	got, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("reading verified artifact: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("OpenVerified() content = %q, want %q", got, payload)
	}
	if openInfo.Digest != wantDigest {
		t.Errorf("OpenVerified() digest = %q, want %q", openInfo.Digest, wantDigest)
	}
	if openInfo.Size != int64(len(payload)) {
		t.Errorf("OpenVerified() size = %d, want %d", openInfo.Size, len(payload))
	}
	if got := regularFileCount(t, root); got != 1 {
		t.Errorf("regular file count = %d, want 1 committed blob", got)
	}
}

func TestStorePutRejectsInvalidInputWithoutResidue(t *testing.T) {
	t.Parallel()

	sourceErr := errors.New("source interrupted")
	tests := []struct {
		name       string
		maxBytes   int64
		expected   digest.SHA256
		newSource  func() io.Reader
		wantErr    error
		wantRead   int64
		checkReads bool
	}{
		{
			name:      "digest mismatch",
			maxBytes:  1024,
			expected:  digest.Sum([]byte("different artifact")),
			newSource: func() io.Reader { return bytes.NewReader([]byte("actual artifact")) },
			wantErr:   ErrDigestMismatch,
		},
		{
			name:     "source fails after complete payload",
			maxBytes: 1024,
			expected: digest.Sum([]byte("partial artifact")),
			newSource: func() io.Reader {
				return io.MultiReader(bytes.NewReader([]byte("partial artifact")), iotest.ErrReader(sourceErr))
			},
			wantErr: sourceErr,
		},
		{
			name:       "size limit stops at max plus one",
			maxBytes:   8,
			expected:   digest.Sum(bytes.Repeat([]byte("x"), 64)),
			newSource:  func() io.Reader { return &countingReader{source: bytes.NewReader(bytes.Repeat([]byte("x"), 64))} },
			wantErr:    ErrTooLarge,
			wantRead:   9,
			checkReads: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store, root := openTestStore(t, tt.maxBytes)
			source := tt.newSource()
			_, err := store.Put(context.Background(), tt.expected, source)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Put() error = %v, want errors.Is(%v)", err, tt.wantErr)
			}
			if tt.checkReads {
				reader, ok := source.(*countingReader)
				if !ok {
					t.Fatal("test source is not a countingReader")
				}
				if got := reader.bytesRead(); got != tt.wantRead {
					t.Errorf("Put() read %d bytes, want %d", got, tt.wantRead)
				}
			}
			assertNoRegularFiles(t, root)
		})
	}
}

func TestStoreConcurrentPutSameDigestCreatesOnce(t *testing.T) {
	t.Parallel()

	store, root := openTestStore(t, 1<<20)
	payload := bytes.Repeat([]byte("concurrent artifact"), 128)
	wantDigest := digest.Sum(payload)

	const goroutines = 32
	start := make(chan struct{})
	results := make(chan putResult, goroutines)
	var ready sync.WaitGroup
	ready.Add(goroutines)

	for range goroutines {
		go func() {
			ready.Done()
			<-start
			info, err := store.Put(context.Background(), wantDigest, bytes.NewReader(payload))
			results <- putResult{info: info, err: err}
		}()
	}

	ready.Wait()
	close(start)

	created := 0
	for range goroutines {
		result := <-results
		if result.err != nil {
			t.Errorf("concurrent Put() error = %v", result.err)
			continue
		}
		if result.info.Digest != wantDigest || result.info.Size != int64(len(payload)) {
			t.Errorf("concurrent Put() info = %+v, want digest %q and size %d", result.info, wantDigest, len(payload))
		}
		if result.info.Created {
			created++
		}
	}
	if created != 1 {
		t.Errorf("concurrent Put() created count = %d, want 1", created)
	}
	if got := regularFileCount(t, root); got != 1 {
		t.Errorf("regular file count = %d, want 1 committed blob and no temp files", got)
	}
}

func TestStoreOpenVerifiedDetectsCorruption(t *testing.T) {
	t.Parallel()

	store, _ := openTestStore(t, 1<<20)
	payload := []byte("artifact before tampering")
	wantDigest := digest.Sum(payload)
	if _, err := store.Put(context.Background(), wantDigest, bytes.NewReader(payload)); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	file, _, err := store.OpenVerified(context.Background(), wantDigest)
	if err != nil {
		t.Fatalf("OpenVerified() before tampering error = %v", err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		t.Fatalf("closing artifact before tampering: %v", err)
	}
	if err := os.WriteFile(path, []byte("tampered artifact"), 0o600); err != nil {
		t.Fatalf("tampering with artifact: %v", err)
	}

	corrupt, _, err := store.OpenVerified(context.Background(), wantDigest)
	if corrupt != nil {
		if closeErr := corrupt.Close(); closeErr != nil {
			t.Errorf("closing unexpectedly returned corrupt file: %v", closeErr)
		}
		t.Error("OpenVerified() returned a file for a corrupt blob")
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("OpenVerified() error = %v, want errors.Is(ErrCorrupt)", err)
	}
}

func TestOpenRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config func(t *testing.T) Config
	}{
		{
			name: "empty root",
			config: func(t *testing.T) Config {
				t.Helper()
				return Config{MaxArtifactBytes: 1, Random: cryptorand.Reader}
			},
		},
		{
			name: "negative size limit",
			config: func(t *testing.T) Config {
				t.Helper()
				return Config{Root: t.TempDir(), MaxArtifactBytes: -1, Random: cryptorand.Reader}
			},
		},
		{
			name: "size limit above platform maximum",
			config: func(t *testing.T) Config {
				t.Helper()
				return Config{Root: t.TempDir(), MaxArtifactBytes: 256<<20 + 1, Random: cryptorand.Reader}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store, err := Open(tt.config(t))
			if store != nil {
				if closeErr := store.Close(); closeErr != nil {
					t.Errorf("closing unexpectedly returned store: %v", closeErr)
				}
				t.Error("Open() returned a store for invalid config")
			}
			if err == nil {
				t.Fatal("Open() error = nil, want invalid config error")
			}
		})
	}
}

func TestOpenUsesSafeDefaults(t *testing.T) {
	t.Parallel()

	store, err := Open(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("Open() with defaults error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	if store.maxBytes != DefaultMaxArtifactBytes {
		t.Fatalf("default max artifact bytes = %d, want %d", store.maxBytes, DefaultMaxArtifactBytes)
	}

	payload := []byte("artifact written with default limit and random source")
	info, err := store.Put(context.Background(), digest.Sum(payload), bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Put() with defaults error = %v", err)
	}
	if !info.Created {
		t.Error("Put() created = false, want true")
	}
}

func TestStoreRejectsMalformedDigestBeforeCreatingFiles(t *testing.T) {
	t.Parallel()

	store, root := openTestStore(t, 1<<20)
	malformed := digest.SHA256("sha256:../../outside")
	if _, err := store.Put(context.Background(), malformed, bytes.NewReader([]byte("payload"))); err == nil {
		t.Fatal("Put() accepted a malformed digest")
	}
	if file, _, err := store.OpenVerified(context.Background(), malformed); err == nil || file != nil {
		t.Fatalf("OpenVerified() = (%v, %v), want nil file and validation error", file, err)
	}
	assertNoRegularFiles(t, root)
}

func TestStoreCloseIsIdempotentAndStopsNewOperations(t *testing.T) {
	t.Parallel()

	store, err := Open(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}

	payload := []byte("closed store")
	_, err = store.Put(context.Background(), digest.Sum(payload), bytes.NewReader(payload))
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Put() after Close() error = %v, want errors.Is(ErrClosed)", err)
	}
}

func TestStoreHonorsContextCancellation(t *testing.T) {
	t.Parallel()

	t.Run("put during copy", func(t *testing.T) {
		t.Parallel()

		store, root := openTestStore(t, 1<<20)
		ctx, cancel := context.WithCancel(context.Background())
		source := &cancelingReader{
			cancel: cancel,
			data:   bytes.Repeat([]byte("x"), 4096),
		}

		_, err := store.Put(ctx, digest.Sum(source.data), source)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Put() error = %v, want errors.Is(context.Canceled)", err)
		}
		assertNoRegularFiles(t, root)
	})

	t.Run("open verified", func(t *testing.T) {
		t.Parallel()

		store, _ := openTestStore(t, 1<<20)
		payload := []byte("committed artifact")
		wantDigest := digest.Sum(payload)
		if _, err := store.Put(context.Background(), wantDigest, bytes.NewReader(payload)); err != nil {
			t.Fatalf("Put() error = %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		file, _, err := store.OpenVerified(ctx, wantDigest)
		if file != nil {
			if closeErr := file.Close(); closeErr != nil {
				t.Errorf("closing unexpectedly returned file: %v", closeErr)
			}
			t.Error("OpenVerified() returned a file for a canceled context")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("OpenVerified() error = %v, want errors.Is(context.Canceled)", err)
		}
	})
}

type putResult struct {
	info Info
	err  error
}

type countingReader struct {
	mu     sync.Mutex
	source io.Reader
	read   int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.source.Read(p)
	r.mu.Lock()
	r.read += int64(n)
	r.mu.Unlock()
	return n, err
}

func (r *countingReader) bytesRead() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.read
}

type cancelingReader struct {
	cancel context.CancelFunc
	data   []byte
	offset int
}

func (r *cancelingReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}

	if r.offset == 0 {
		p[0] = r.data[0]
		r.offset = 1
		r.cancel()
		return 1, nil
	}

	n := copy(p, r.data[r.offset:])
	r.offset += n
	return n, nil
}

func openTestStore(t *testing.T, maxBytes int64) (*Store, string) {
	t.Helper()

	root := t.TempDir()
	store, err := Open(Config{
		Root:             root,
		MaxArtifactBytes: maxBytes,
		Random:           cryptorand.Reader,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return store, root
}

func assertNoRegularFiles(t *testing.T, root string) {
	t.Helper()
	if got := regularFileCount(t, root); got != 0 {
		t.Errorf("regular file count = %d, want no committed blob or temp file", got)
	}
}

func regularFileCount(t *testing.T, root string) int {
	t.Helper()

	count := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking artifact root: %v", err)
	}
	return count
}
