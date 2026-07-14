package workercache

import (
	"bytes"
	"context"
	"errors"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/artifact"
	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/wasmexec"
	"github.com/yourikka/minicloud/internal/wasmprofile"
)

func TestCacheKeyIncludesExactCompilationProfile(t *testing.T) {
	harness := newCacheHarness(t)
	cache, _ := harness.newCache(t, Config{}, nil)
	spec := harness.putModule(t, commandModule('a'))

	key, err := cache.profile.key(spec)
	if err != nil {
		t.Fatalf("profile.key() error = %v", err)
	}
	wantProfile := harness.engine.Profile()
	if key.ArtifactDigest != spec.ArtifactDigest || key.ArtifactSize != spec.ArtifactSize {
		t.Fatalf("artifact key = (%q, %d), want (%q, %d)", key.ArtifactDigest, key.ArtifactSize, spec.ArtifactDigest, spec.ArtifactSize)
	}
	if key.RuntimeName != wasmprofile.RuntimeName || key.RuntimeVersion != wasmprofile.RuntimeVersion {
		t.Fatalf("runtime key = %q/%q, want %q/%q", key.RuntimeName, key.RuntimeVersion, wasmprofile.RuntimeName, wasmprofile.RuntimeVersion)
	}
	if key.ABI != model.ABIWASICommandV1 || key.HostAPIProfile != model.HostAPIProfileNone {
		t.Fatalf("ABI key = %q/%q", key.ABI, key.HostAPIProfile)
	}
	if key.RuntimeFeatureProfile != wasmprofile.FeatureProfile {
		t.Fatalf("feature key = %q, want %q", key.RuntimeFeatureProfile, wasmprofile.FeatureProfile)
	}
	if key.Engine != wantProfile.Engine || key.MemoryLimitMiB != wantProfile.MemoryLimitMiB {
		t.Fatalf("engine key = (%q, %d), want (%q, %d)", key.Engine, key.MemoryLimitMiB, wantProfile.Engine, wantProfile.MemoryLimitMiB)
	}
	if key.GOOS != runtime.GOOS || key.GOARCH != runtime.GOARCH {
		t.Fatalf("platform key = %q/%q, want %q/%q", key.GOOS, key.GOARCH, runtime.GOOS, runtime.GOARCH)
	}
}

func TestNewRejectsInvalidBounds(t *testing.T) {
	harness := newCacheHarness(t)
	compiler := &compilerProbe{base: harness.engine}
	valid := Config{Artifacts: harness.source, Compiler: compiler}
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "missing artifact source", mutate: func(config *Config) { config.Artifacts = nil }},
		{name: "missing compiler", mutate: func(config *Config) { config.Compiler = nil }},
		{name: "negative weight", mutate: func(config *Config) { config.MaxWeightBytes = -1 }},
		{name: "weight above hard maximum", mutate: func(config *Config) { config.MaxWeightBytes = DefaultMaxWeightBytes + 1 }},
		{name: "negative entries", mutate: func(config *Config) { config.MaxEntries = -1 }},
		{name: "entries above hard maximum", mutate: func(config *Config) { config.MaxEntries = DefaultMaxEntries + 1 }},
		{name: "negative concurrent loads", mutate: func(config *Config) { config.MaxConcurrentLoads = -1 }},
		{name: "concurrent loads above hard maximum", mutate: func(config *Config) { config.MaxConcurrentLoads = hardMaxConcurrentLoads + 1 }},
		{name: "negative queued loads", mutate: func(config *Config) { config.MaxQueuedLoads = -1 }},
		{name: "queued loads above hard maximum", mutate: func(config *Config) { config.MaxQueuedLoads = hardMaxQueuedLoads + 1 }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			if cache, err := New(config); err == nil || cache != nil {
				t.Fatalf("New() = (%v, %v), want nil cache and error", cache, err)
			}
		})
	}
}

func TestCacheCoalescesConcurrentAcquireAndHitsAfterward(t *testing.T) {
	harness := newCacheHarness(t)
	started := make(chan struct{})
	releaseCompile := make(chan struct{})
	var startedOnce sync.Once
	cache, compiler := harness.newCache(t, Config{}, func(
		ctx context.Context,
		wasm []byte,
		_ int,
	) (*wasmexec.Program, wasmexec.Metrics, error) {
		startedOnce.Do(func() { close(started) })
		select {
		case <-releaseCompile:
			return harness.engine.Compile(ctx, wasm)
		case <-ctx.Done():
			return nil, wasmexec.Metrics{}, ctx.Err()
		}
	})
	spec := harness.putModule(t, commandModule('a'))

	const callers = 32
	results := make(chan acquireOutcome, callers)
	go acquireInto(context.Background(), cache, spec, results)
	awaitSignal(t, started, "first compilation")
	for range callers - 1 {
		go acquireInto(context.Background(), cache, spec, results)
	}
	awaitCondition(t, func() bool { return cache.Snapshot().Coalesced == callers-1 }, "all callers to join the shared load")
	close(releaseCompile)

	leases := make([]*Lease, 0, callers)
	for range callers {
		outcome := <-results
		if outcome.err != nil {
			t.Fatalf("Acquire() error = %v", outcome.err)
		}
		leases = append(leases, outcome.lease)
	}
	if got := compiler.Calls(); got != 1 {
		t.Fatalf("Compile() calls = %d, want 1", got)
	}
	if got := harness.source.Calls(spec.ArtifactDigest); got != 1 {
		t.Fatalf("OpenVerified() calls = %d, want 1", got)
	}
	stats := cache.Snapshot()
	if stats.Misses != 1 || stats.Coalesced != callers-1 || stats.Entries != 1 || stats.CurrentWeight != spec.ArtifactSize {
		t.Fatalf("cache stats after shared load = %+v", stats)
	}
	for _, lease := range leases {
		lease.Release()
	}

	hitLease, result, err := cache.Acquire(context.Background(), spec)
	if err != nil {
		t.Fatalf("hit Acquire() error = %v", err)
	}
	if !result.Hit || result.Coalesced {
		t.Fatalf("hit result = %+v, want Hit only", result)
	}
	hitLease.Release()
	if got := cache.Snapshot().Hits; got != 1 {
		t.Fatalf("cache hits = %d, want 1", got)
	}
}

func TestCacheWaiterCancellationDoesNotCancelSharedLoad(t *testing.T) {
	harness := newCacheHarness(t)
	started := make(chan struct{})
	releaseCompile := make(chan struct{})
	cache, _ := harness.newCache(t, Config{}, func(
		ctx context.Context,
		wasm []byte,
		_ int,
	) (*wasmexec.Program, wasmexec.Metrics, error) {
		close(started)
		select {
		case <-releaseCompile:
			return harness.engine.Compile(ctx, wasm)
		case <-ctx.Done():
			return nil, wasmexec.Metrics{}, ctx.Err()
		}
	})
	spec := harness.putModule(t, commandModule('b'))

	leaderContext, cancelLeader := context.WithCancel(context.Background())
	leaderResult := make(chan acquireOutcome, 1)
	go acquireInto(leaderContext, cache, spec, leaderResult)
	awaitSignal(t, started, "shared compilation")

	followerResult := make(chan acquireOutcome, 1)
	go acquireInto(context.Background(), cache, spec, followerResult)
	awaitCondition(t, func() bool { return cache.Snapshot().Coalesced == 1 }, "follower to join the shared load")
	cancelLeader()
	leader := <-leaderResult
	if !errors.Is(leader.err, context.Canceled) || leader.lease != nil {
		t.Fatalf("leader Acquire() = (%v, %v), want context cancellation", leader.lease, leader.err)
	}
	close(releaseCompile)

	follower := <-followerResult
	if follower.err != nil || follower.lease == nil {
		t.Fatalf("follower Acquire() = (%v, %v), want success", follower.lease, follower.err)
	}
	follower.lease.Release()
}

func TestCacheCancelsSharedLoadAfterEveryWaiterLeaves(t *testing.T) {
	harness := newCacheHarness(t)
	started := make(chan struct{})
	compileCanceled := make(chan struct{})
	cache, compiler := harness.newCache(t, Config{}, func(
		ctx context.Context,
		_ []byte,
		_ int,
	) (*wasmexec.Program, wasmexec.Metrics, error) {
		close(started)
		<-ctx.Done()
		close(compileCanceled)
		return nil, wasmexec.Metrics{}, ctx.Err()
	})
	spec := harness.putModule(t, commandModule('c'))

	firstContext, cancelFirst := context.WithCancel(context.Background())
	secondContext, cancelSecond := context.WithCancel(context.Background())
	results := make(chan acquireOutcome, 2)
	go acquireInto(firstContext, cache, spec, results)
	awaitSignal(t, started, "shared compilation")
	go acquireInto(secondContext, cache, spec, results)
	awaitCondition(t, func() bool { return cache.Snapshot().Coalesced == 1 }, "second waiter to join")
	cancelFirst()
	cancelSecond()

	for range 2 {
		outcome := <-results
		if !errors.Is(outcome.err, context.Canceled) || outcome.lease != nil {
			t.Fatalf("Acquire() = (%v, %v), want context cancellation", outcome.lease, outcome.err)
		}
	}
	awaitSignal(t, compileCanceled, "shared compilation cancellation")
	awaitCondition(t, func() bool { return cache.Snapshot().Inflight == 0 }, "canceled flight cleanup")
	if got := compiler.Calls(); got != 1 {
		t.Fatalf("Compile() calls = %d, want 1", got)
	}
	if stats := cache.Snapshot(); stats.Entries != 0 || stats.CurrentWeight != 0 {
		t.Fatalf("cache retained canceled compilation: %+v", stats)
	}
}

func TestCacheBoundsDistinctColdLoads(t *testing.T) {
	harness := newCacheHarness(t)
	started := make(chan struct{})
	releaseCompile := make(chan struct{})
	var startedOnce sync.Once
	cache, compiler := harness.newCache(t, Config{
		MaxConcurrentLoads: 1,
		MaxQueuedLoads:     1,
	}, func(
		ctx context.Context,
		wasm []byte,
		_ int,
	) (*wasmexec.Program, wasmexec.Metrics, error) {
		startedOnce.Do(func() { close(started) })
		select {
		case <-releaseCompile:
			return harness.engine.Compile(ctx, wasm)
		case <-ctx.Done():
			return nil, wasmexec.Metrics{}, ctx.Err()
		}
	})
	firstSpec := harness.putModule(t, commandModule('d'))
	secondSpec := harness.putModule(t, commandModule('e'))
	thirdSpec := harness.putModule(t, commandModule('f'))

	results := make(chan acquireOutcome, 2)
	go acquireInto(context.Background(), cache, firstSpec, results)
	awaitSignal(t, started, "first cold load")
	go acquireInto(context.Background(), cache, secondSpec, results)
	awaitCondition(t, func() bool { return cache.Snapshot().Inflight == 2 }, "second cold load to queue")

	lease, _, err := cache.Acquire(context.Background(), thirdSpec)
	if lease != nil || !errors.Is(err, ErrLoadQueueFull) {
		t.Fatalf("third Acquire() = (%v, %v), want ErrLoadQueueFull", lease, err)
	}
	if got := compiler.Calls(); got != 1 {
		t.Fatalf("Compile() calls while one load queued = %d, want 1", got)
	}
	close(releaseCompile)
	for range 2 {
		outcome := <-results
		if outcome.err != nil {
			t.Fatalf("bounded Acquire() error = %v", outcome.err)
		}
		outcome.lease.Release()
	}
	if got := compiler.Calls(); got != 2 {
		t.Fatalf("Compile() calls = %d, want 2", got)
	}
	stats := cache.Snapshot()
	if stats.Misses != 3 || stats.RejectionsLoadQueue != 1 {
		t.Fatalf("bounded load stats = %+v", stats)
	}
}

func TestCacheEvictsLeastRecentlyUsedUnpinnedEntry(t *testing.T) {
	harness := newCacheHarness(t)
	cache, compiler := harness.newCache(t, Config{MaxEntries: 2}, nil)
	firstSpec := harness.putModule(t, commandModule('g'))
	secondSpec := harness.putModule(t, commandModule('h'))
	thirdSpec := harness.putModule(t, commandModule('i'))

	acquireAndRelease(t, cache, firstSpec)
	acquireAndRelease(t, cache, secondSpec)
	acquireAndRelease(t, cache, firstSpec)
	acquireAndRelease(t, cache, thirdSpec)
	if got := compiler.Calls(); got != 3 {
		t.Fatalf("Compile() calls before reacquiring LRU = %d, want 3", got)
	}
	acquireAndRelease(t, cache, secondSpec)
	if got := compiler.Calls(); got != 4 {
		t.Fatalf("Compile() calls after reacquiring LRU = %d, want 4", got)
	}
	stats := cache.Snapshot()
	if stats.Entries != 2 || stats.EvictionsCapacity != 2 || stats.EvictionsEntries != 2 {
		t.Fatalf("LRU stats = %+v, want two entries and two evictions", stats)
	}
}

func TestCacheReportsByteCapacityEvictionReason(t *testing.T) {
	harness := newCacheHarness(t)
	module := commandModule('q')
	cache, _ := harness.newCache(t, Config{MaxWeightBytes: int64(len(module) * 2)}, nil)
	firstSpec := harness.putModule(t, module)
	secondSpec := harness.putModule(t, commandModule('r'))
	thirdSpec := harness.putModule(t, commandModule('s'))

	acquireAndRelease(t, cache, firstSpec)
	acquireAndRelease(t, cache, secondSpec)
	acquireAndRelease(t, cache, thirdSpec)
	stats := cache.Snapshot()
	if stats.EvictionsCapacity != 1 || stats.EvictionsBytes != 1 || stats.EvictionsEntries != 0 {
		t.Fatalf("byte capacity eviction stats = %+v", stats)
	}
}

func TestCacheRejectsInsertionWhenEveryEntryIsPinned(t *testing.T) {
	harness := newCacheHarness(t)
	cache, compiler := harness.newCache(t, Config{MaxEntries: 1}, nil)
	firstSpec := harness.putModule(t, commandModule('j'))
	secondSpec := harness.putModule(t, commandModule('k'))

	firstLease, _, err := cache.Acquire(context.Background(), firstSpec)
	if err != nil {
		t.Fatalf("first Acquire() error = %v", err)
	}
	secondLease, _, err := cache.Acquire(context.Background(), secondSpec)
	if secondLease != nil || !errors.Is(err, ErrFull) {
		t.Fatalf("pinned Acquire() = (%v, %v), want ErrFull", secondLease, err)
	}
	stats := cache.Snapshot()
	if stats.Entries != 1 || stats.CurrentWeight != firstSpec.ArtifactSize || stats.RejectionsFull != 1 {
		t.Fatalf("pinned cache stats = %+v", stats)
	}
	firstLease.Release()
	acquireAndRelease(t, cache, secondSpec)
	if got := compiler.Calls(); got != 3 {
		t.Fatalf("Compile() calls = %d, want rejected compile to be retried", got)
	}
}

func TestCacheRejectsOversizedEntryBeforeOpeningArtifact(t *testing.T) {
	harness := newCacheHarness(t)
	wasm := commandModule('l')
	cache, compiler := harness.newCache(t, Config{MaxWeightBytes: int64(len(wasm) - 1)}, nil)
	spec := harness.putModule(t, wasm)

	lease, _, err := cache.Acquire(context.Background(), spec)
	if lease != nil || !errors.Is(err, ErrEntryTooLarge) {
		t.Fatalf("Acquire() = (%v, %v), want ErrEntryTooLarge", lease, err)
	}
	if got := harness.source.Calls(spec.ArtifactDigest); got != 0 {
		t.Fatalf("OpenVerified() calls = %d, want 0", got)
	}
	if got := compiler.Calls(); got != 0 {
		t.Fatalf("Compile() calls = %d, want 0", got)
	}
}

func TestCacheRechecksBytesReturnedByArtifactSource(t *testing.T) {
	harness := newCacheHarness(t)
	wasm := commandModule('m')
	spec := moduleSpec(wasm)
	wrongPath := t.TempDir() + "/wrong.wasm"
	if err := os.WriteFile(wrongPath, bytes.Repeat([]byte{0xff}, len(wasm)), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	source := artifactSourceFunc(func(context.Context, digest.SHA256) (*os.File, artifact.Info, error) {
		file, err := os.Open(wrongPath)
		return file, artifact.Info{Digest: spec.ArtifactDigest, Size: spec.ArtifactSize}, err
	})
	compiler := &compilerProbe{base: harness.engine}
	cache, err := New(Config{Artifacts: source, Compiler: compiler})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { closeCache(t, cache) })

	lease, _, err := cache.Acquire(context.Background(), spec)
	if lease != nil || !errors.Is(err, artifact.ErrCorrupt) {
		t.Fatalf("Acquire() = (%v, %v), want artifact.ErrCorrupt", lease, err)
	}
	if got := compiler.Calls(); got != 0 {
		t.Fatalf("Compile() calls = %d, want 0 for corrupt bytes", got)
	}
}

func TestCacheRefetchesOnceAfterCorruptArtifact(t *testing.T) {
	harness := newCacheHarness(t)
	wasm := commandModule('v')
	spec := moduleSpec(wasm)
	validPath := t.TempDir() + "/valid.wasm"
	if err := os.WriteFile(validPath, wasm, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	var mu sync.Mutex
	openCalls := 0
	source := artifactSourceFunc(func(context.Context, digest.SHA256) (*os.File, artifact.Info, error) {
		mu.Lock()
		defer mu.Unlock()
		openCalls++
		if openCalls == 1 {
			return nil, artifact.Info{}, artifact.ErrCorrupt
		}
		file, err := os.Open(validPath)
		return file, artifact.Info{Digest: spec.ArtifactDigest, Size: spec.ArtifactSize}, err
	})
	compiler := &compilerProbe{base: harness.engine}
	cache, err := New(Config{Artifacts: source, Compiler: compiler})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { closeCache(t, cache) })

	lease, _, err := cache.Acquire(context.Background(), spec)
	if err != nil {
		t.Fatalf("Acquire() after refetch error = %v", err)
	}
	lease.Release()
	mu.Lock()
	gotOpenCalls := openCalls
	mu.Unlock()
	if gotOpenCalls != 2 || compiler.Calls() != 1 {
		t.Fatalf("refetch calls = open %d, compile %d; want 2 and 1", gotOpenCalls, compiler.Calls())
	}
}

func TestCacheRetriesFailedColdLoad(t *testing.T) {
	harness := newCacheHarness(t)
	wantErr := errors.New("transient compilation failure")
	cache, compiler := harness.newCache(t, Config{}, func(
		ctx context.Context,
		wasm []byte,
		call int,
	) (*wasmexec.Program, wasmexec.Metrics, error) {
		if call == 1 {
			return nil, wasmexec.Metrics{}, wantErr
		}
		return harness.engine.Compile(ctx, wasm)
	})
	spec := harness.putModule(t, commandModule('n'))

	lease, _, err := cache.Acquire(context.Background(), spec)
	if lease != nil || !errors.Is(err, wantErr) {
		t.Fatalf("first Acquire() = (%v, %v), want transient error", lease, err)
	}
	lease, _, err = cache.Acquire(context.Background(), spec)
	if err != nil {
		t.Fatalf("second Acquire() error = %v", err)
	}
	lease.Release()
	if got := compiler.Calls(); got != 2 {
		t.Fatalf("Compile() calls = %d, want 2", got)
	}
}

func TestCacheRejectsNilProgramFromCompiler(t *testing.T) {
	harness := newCacheHarness(t)
	cache, _ := harness.newCache(t, Config{}, func(
		context.Context,
		[]byte,
		int,
	) (*wasmexec.Program, wasmexec.Metrics, error) {
		return nil, wasmexec.Metrics{}, nil
	})
	spec := harness.putModule(t, commandModule('w'))

	lease, _, err := cache.Acquire(context.Background(), spec)
	if lease != nil || err == nil {
		t.Fatalf("Acquire() = (%v, %v), want invalid compiler result error", lease, err)
	}
	if stats := cache.Snapshot(); stats.Entries != 0 || stats.CurrentWeight != 0 {
		t.Fatalf("cache retained nil compilation: %+v", stats)
	}
}

func TestCacheDoesNotCompileArtifactReturnedAfterSharedCancellation(t *testing.T) {
	harness := newCacheHarness(t)
	wasm := commandModule('x')
	spec := moduleSpec(wasm)
	validPath := t.TempDir() + "/valid.wasm"
	if err := os.WriteFile(validPath, wasm, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	sourceStarted := make(chan struct{})
	source := artifactSourceFunc(func(ctx context.Context, _ digest.SHA256) (*os.File, artifact.Info, error) {
		close(sourceStarted)
		<-ctx.Done()
		file, err := os.Open(validPath)
		return file, artifact.Info{Digest: spec.ArtifactDigest, Size: spec.ArtifactSize}, err
	})
	compiler := &compilerProbe{base: harness.engine}
	cache, err := New(Config{Artifacts: source, Compiler: compiler})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { closeCache(t, cache) })

	acquireContext, cancelAcquire := context.WithCancel(context.Background())
	result := make(chan acquireOutcome, 1)
	go acquireInto(acquireContext, cache, spec, result)
	awaitSignal(t, sourceStarted, "artifact source")
	cancelAcquire()
	if outcome := <-result; outcome.lease != nil || !errors.Is(outcome.err, context.Canceled) {
		t.Fatalf("Acquire() = (%v, %v), want context cancellation", outcome.lease, outcome.err)
	}
	awaitCondition(t, func() bool { return cache.Snapshot().Inflight == 0 }, "canceled artifact read cleanup")
	if got := compiler.Calls(); got != 0 {
		t.Fatalf("Compile() calls = %d, want 0", got)
	}
}

func TestCacheCloseWaitsForLeaseAndRejectsNewAcquire(t *testing.T) {
	harness := newCacheHarness(t)
	cache, _ := harness.newCache(t, Config{}, nil)
	spec := harness.putModule(t, commandModule('o'))
	lease, _, err := cache.Acquire(context.Background(), spec)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}

	closeContext, cancelClose := context.WithCancel(context.Background())
	cancelClose()
	if err := cache.Close(closeContext); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close() error = %v, want context.Canceled", err)
	}
	newLease, _, err := cache.Acquire(context.Background(), spec)
	if newLease != nil || !errors.Is(err, ErrClosed) {
		t.Fatalf("Acquire() after Close = (%v, %v), want ErrClosed", newLease, err)
	}
	lease.Release()
	if err := cache.Close(context.Background()); err != nil {
		t.Fatalf("retry Close() error = %v", err)
	}
}

func TestCacheCloseCancelsInflightColdLoad(t *testing.T) {
	harness := newCacheHarness(t)
	started := make(chan struct{})
	compileCanceled := make(chan struct{})
	cache, _ := harness.newCache(t, Config{}, func(
		ctx context.Context,
		_ []byte,
		_ int,
	) (*wasmexec.Program, wasmexec.Metrics, error) {
		close(started)
		<-ctx.Done()
		close(compileCanceled)
		return nil, wasmexec.Metrics{}, ctx.Err()
	})
	spec := harness.putModule(t, commandModule('t'))
	acquired := make(chan acquireOutcome, 1)
	go acquireInto(context.Background(), cache, spec, acquired)
	awaitSignal(t, started, "cold load before close")

	closed := make(chan error, 1)
	go func() { closed <- cache.Close(context.Background()) }()
	awaitSignal(t, compileCanceled, "cold load cancellation during close")
	if outcome := <-acquired; outcome.lease != nil || !errors.Is(outcome.err, ErrClosed) {
		t.Fatalf("Acquire() during Close = (%v, %v), want ErrClosed", outcome.lease, outcome.err)
	}
	if err := <-closed; err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestLeaseReleaseIsRaceSafeAndIdempotent(t *testing.T) {
	harness := newCacheHarness(t)
	cache, _ := harness.newCache(t, Config{}, nil)
	spec := harness.putModule(t, commandModule('u'))
	lease, _, err := cache.Acquire(context.Background(), spec)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}

	const invocations = 16
	start := make(chan struct{})
	errorsSeen := make(chan error, invocations)
	for range invocations {
		go func() {
			<-start
			_, invokeErr := lease.Invoke(context.Background(), InvocationRequest{})
			errorsSeen <- invokeErr
		}()
	}
	close(start)
	lease.Release()
	lease.Release()
	for range invocations {
		if invokeErr := <-errorsSeen; invokeErr == nil {
			t.Error("concurrent Invoke() unexpectedly succeeded with an empty response")
		}
	}
	if _, err := lease.Invoke(context.Background(), InvocationRequest{}); err == nil {
		t.Fatal("Invoke() after Release succeeded")
	}
}

type cacheHarness struct {
	store  *artifact.Store
	source *countingArtifactSource
	engine *wasmexec.Engine
}

func newCacheHarness(t *testing.T) *cacheHarness {
	t.Helper()
	store, err := artifact.Open(artifact.Config{Root: t.TempDir(), MaxArtifactBytes: 1 << 20})
	if err != nil {
		t.Fatalf("artifact.Open() error = %v", err)
	}
	engine, err := wasmexec.New(context.Background(), wasmexec.Config{})
	if err != nil {
		_ = store.Close()
		t.Fatalf("wasmexec.New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := engine.Close(context.Background()); err != nil {
			t.Errorf("Engine.Close() error = %v", err)
		}
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	return &cacheHarness{
		store:  store,
		source: &countingArtifactSource{source: store, calls: make(map[digest.SHA256]int)},
		engine: engine,
	}
}

func (h *cacheHarness) newCache(
	t *testing.T,
	config Config,
	hook compileHook,
) (*Cache, *compilerProbe) {
	t.Helper()
	compiler := &compilerProbe{base: h.engine, hook: hook}
	config.Artifacts = h.source
	config.Compiler = compiler
	cache, err := New(config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { closeCache(t, cache) })
	return cache, compiler
}

func (h *cacheHarness) putModule(t *testing.T, wasm []byte) ModuleSpec {
	t.Helper()
	spec := moduleSpec(wasm)
	if _, err := h.store.Put(context.Background(), spec.ArtifactDigest, bytes.NewReader(wasm)); err != nil {
		t.Fatalf("Store.Put() error = %v", err)
	}
	return spec
}

type countingArtifactSource struct {
	source ArtifactSource
	mu     sync.Mutex
	calls  map[digest.SHA256]int
}

func (s *countingArtifactSource) OpenVerified(
	ctx context.Context,
	want digest.SHA256,
) (*os.File, artifact.Info, error) {
	s.mu.Lock()
	s.calls[want]++
	s.mu.Unlock()
	return s.source.OpenVerified(ctx, want)
}

func (s *countingArtifactSource) Calls(want digest.SHA256) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[want]
}

type artifactSourceFunc func(context.Context, digest.SHA256) (*os.File, artifact.Info, error)

func (f artifactSourceFunc) OpenVerified(
	ctx context.Context,
	want digest.SHA256,
) (*os.File, artifact.Info, error) {
	return f(ctx, want)
}

type compileHook func(context.Context, []byte, int) (*wasmexec.Program, wasmexec.Metrics, error)

type compilerProbe struct {
	base  *wasmexec.Engine
	hook  compileHook
	mu    sync.Mutex
	calls int
}

func (c *compilerProbe) Compile(
	ctx context.Context,
	wasm []byte,
) (*wasmexec.Program, wasmexec.Metrics, error) {
	c.mu.Lock()
	c.calls++
	call := c.calls
	c.mu.Unlock()
	if c.hook != nil {
		return c.hook(ctx, wasm, call)
	}
	return c.base.Compile(ctx, wasm)
}

func (c *compilerProbe) Profile() wasmprofile.Profile {
	return c.base.Profile()
}

func (c *compilerProbe) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

type acquireOutcome struct {
	lease  *Lease
	result Result
	err    error
}

func acquireInto(ctx context.Context, cache *Cache, spec ModuleSpec, output chan<- acquireOutcome) {
	lease, result, err := cache.Acquire(ctx, spec)
	output <- acquireOutcome{lease: lease, result: result, err: err}
}

func acquireAndRelease(t *testing.T, cache *Cache, spec ModuleSpec) Result {
	t.Helper()
	lease, result, err := cache.Acquire(context.Background(), spec)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	lease.Release()
	return result
}

func closeCache(t *testing.T, cache *Cache) {
	t.Helper()
	if err := cache.Close(context.Background()); err != nil {
		t.Errorf("Cache.Close() error = %v", err)
	}
}

func awaitSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func awaitCondition(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(time.Millisecond)
	}
}

func moduleSpec(wasm []byte) ModuleSpec {
	return ModuleSpec{
		ArtifactDigest:        digest.Sum(wasm),
		ArtifactSize:          int64(len(wasm)),
		ABI:                   model.ABIWASICommandV1,
		HostAPIProfile:        model.HostAPIProfileNone,
		RuntimeFeatureProfile: wasmprofile.FeatureProfile,
	}
}

func commandModule(variant byte) []byte {
	module := []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
		0x03, 0x02, 0x01, 0x00,
		0x07, 0x0a, 0x01, 0x06, 0x5f, 0x73, 0x74, 0x61, 0x72, 0x74, 0x00, 0x00,
		0x0a, 0x04, 0x01, 0x02, 0x00, 0x0b,
	}
	return append(module, 0x00, 0x02, 0x01, variant)
}
