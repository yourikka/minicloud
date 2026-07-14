// Package workercache loads verified artifacts and owns a bounded compiled
// module cache for one Worker.
package workercache

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/yourikka/minicloud/internal/wasmexec"
)

const (
	DefaultMaxWeightBytes     = int64(10 << 30)
	DefaultMaxEntries         = 4096
	DefaultMaxConcurrentLoads = 2
	DefaultMaxQueuedLoads     = 64
	hardMaxConcurrentLoads    = 64
	hardMaxQueuedLoads        = 4096
)

var (
	ErrClosed        = errors.New("worker compiled cache is closed")
	ErrFull          = errors.New("worker compiled cache has no evictable capacity")
	ErrEntryTooLarge = errors.New("compiled cache entry exceeds configured capacity")
	ErrLoadQueueFull = errors.New("worker compiled cache load queue is full")
)

// EvictionReason is a stable metrics label for capacity-driven eviction.
type EvictionReason string

const (
	EvictionCapacityBytes   EvictionReason = "capacity_bytes"
	EvictionCapacityEntries EvictionReason = "capacity_entries"
)

// Config defines independent hard bounds for cached entries and cold loads.
type Config struct {
	Artifacts          ArtifactSource
	Compiler           Compiler
	MaxWeightBytes     int64
	MaxEntries         int
	MaxConcurrentLoads int
	MaxQueuedLoads     int
}

// Result describes how an Acquire was satisfied and its cold-path timing.
type Result struct {
	Key          Key
	Hit          bool
	Coalesced    bool
	ArtifactLoad time.Duration
	Compile      time.Duration
	Evicted      int
}

// Stats is a consistent cache metrics snapshot. CurrentWeight is the sum of
// verified artifact bytes charged to cached compilations, not native RSS.
type Stats struct {
	Hits                uint64
	Misses              uint64
	Coalesced           uint64
	EvictionsCapacity   uint64
	EvictionsBytes      uint64
	EvictionsEntries    uint64
	RejectionsFull      uint64
	RejectionsLoadQueue uint64
	CloseErrors         uint64
	Entries             int
	CurrentWeight       int64
	Inflight            int
}

// Cache is safe for concurrent use.
type Cache struct {
	artifacts      ArtifactSource
	compiler       Compiler
	profile        Profile
	maxWeightBytes int64
	maxEntries     int
	loadGate       *loadGate

	mu            sync.Mutex
	entries       map[Key]*entry
	lru           list.List
	flights       map[Key]*flight
	currentWeight int64
	active        int
	drained       chan struct{}
	closing       bool
	releasing     bool
	released      bool
	releaseDone   chan struct{}
	closePending  []*wasmexec.Program
	stats         Stats
}

type entry struct {
	key     Key
	program *wasmexec.Program
	weight  int64
	refs    int
	element *list.Element
}

type eviction struct {
	entry  *entry
	reason EvictionReason
}

type flight struct {
	key      Key
	done     chan struct{}
	cancel   context.CancelFunc
	waiters  int
	finished bool
	entry    *entry
	result   Result
	err      error
}

// Lease pins one Program against eviction. Release must be called after the
// invocation using this lease has completed.
type Lease struct {
	cache    *Cache
	entry    *entry
	mu       sync.RWMutex
	released bool
	release  sync.Once
}

// New validates all cache bounds and locks the compiler profile into keys.
func New(config Config) (*Cache, error) {
	if config.Artifacts == nil {
		return nil, errors.New("worker cache artifact source is required")
	}
	if config.Compiler == nil {
		return nil, errors.New("worker cache compiler is required")
	}
	if config.MaxWeightBytes == 0 {
		config.MaxWeightBytes = DefaultMaxWeightBytes
	}
	if config.MaxEntries == 0 {
		config.MaxEntries = DefaultMaxEntries
	}
	if config.MaxConcurrentLoads == 0 {
		config.MaxConcurrentLoads = DefaultMaxConcurrentLoads
	}
	if config.MaxQueuedLoads == 0 {
		config.MaxQueuedLoads = DefaultMaxQueuedLoads
	}
	if config.MaxWeightBytes < 1 || config.MaxWeightBytes > DefaultMaxWeightBytes {
		return nil, errors.New("worker cache weight limit is outside v1 bounds")
	}
	if config.MaxEntries < 1 || config.MaxEntries > DefaultMaxEntries {
		return nil, errors.New("worker cache entry limit is outside v1 bounds")
	}
	if config.MaxConcurrentLoads < 1 || config.MaxConcurrentLoads > hardMaxConcurrentLoads {
		return nil, errors.New("worker cache concurrent load limit is outside v1 bounds")
	}
	if config.MaxQueuedLoads < 1 || config.MaxQueuedLoads > hardMaxQueuedLoads {
		return nil, errors.New("worker cache queued load limit is outside v1 bounds")
	}
	profile, err := newProfile(config.Compiler.Profile())
	if err != nil {
		return nil, err
	}
	drained := make(chan struct{})
	close(drained)
	return &Cache{
		artifacts:      config.Artifacts,
		compiler:       config.Compiler,
		profile:        profile,
		maxWeightBytes: config.MaxWeightBytes,
		maxEntries:     config.MaxEntries,
		loadGate:       newLoadGate(config.MaxConcurrentLoads, config.MaxQueuedLoads),
		entries:        make(map[Key]*entry),
		flights:        make(map[Key]*flight),
		drained:        drained,
	}, nil
}

// Acquire verifies and compiles a miss, or pins an existing compiled Program.
func (c *Cache) Acquire(
	ctx context.Context,
	spec ModuleSpec,
) (_ *Lease, result Result, err error) {
	if ctx == nil {
		return nil, Result{}, errors.New("worker cache context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, Result{}, err
	}
	key, err := c.profile.key(spec)
	if err != nil {
		return nil, Result{}, err
	}
	if spec.ArtifactSize > c.maxWeightBytes {
		return nil, Result{Key: key}, ErrEntryTooLarge
	}
	if !c.begin() {
		return nil, Result{Key: key}, ErrClosed
	}
	leaseReturned := false
	defer func() {
		if !leaseReturned {
			c.end()
		}
	}()

	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return nil, Result{Key: key}, ErrClosed
	}
	if cached, exists := c.entries[key]; exists {
		cached.refs++
		c.lru.MoveToFront(cached.element)
		c.stats.Hits++
		c.mu.Unlock()
		leaseReturned = true
		return &Lease{cache: c, entry: cached}, Result{Key: key, Hit: true}, nil
	}
	if running, exists := c.flights[key]; exists {
		running.waiters++
		c.stats.Coalesced++
		c.mu.Unlock()
		lease, result, err := c.awaitFlight(ctx, running, true)
		if err != nil {
			return nil, result, err
		}
		leaseReturned = true
		return lease, result, nil
	}

	reservation, reserveErr := c.loadGate.reserve()
	c.stats.Misses++
	if reserveErr != nil {
		c.stats.RejectionsLoadQueue++
		c.mu.Unlock()
		return nil, Result{Key: key}, reserveErr
	}
	workContext, cancel := context.WithCancel(context.WithoutCancel(ctx))
	running := &flight{
		key:     key,
		done:    make(chan struct{}),
		cancel:  cancel,
		waiters: 1,
		result:  Result{Key: key},
	}
	c.flights[key] = running
	c.startLocked()
	c.mu.Unlock()

	go c.runFlight(workContext, key, spec, running, reservation)
	lease, result, err := c.awaitFlight(ctx, running, false)
	if err != nil {
		return nil, result, err
	}
	leaseReturned = true
	return lease, result, nil
}

// Invoke delegates to the pinned Program.
func (l *Lease) Invoke(
	ctx context.Context,
	request InvocationRequest,
) (wasmexec.Result, error) {
	if l == nil || l.entry == nil {
		return wasmexec.Result{}, errors.New("worker cache lease is invalid")
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.released {
		return wasmexec.Result{}, errors.New("worker cache lease is released")
	}
	return l.entry.program.Invoke(ctx, request.Request, request.Timeout)
}

// Release unpins the cache entry. It is idempotent.
func (l *Lease) Release() {
	if l == nil || l.cache == nil || l.entry == nil {
		return
	}
	l.release.Do(func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.released = true
		l.cache.mu.Lock()
		l.entry.refs--
		l.cache.endLocked()
		l.cache.mu.Unlock()
	})
}

// Snapshot returns metrics and current bounded occupancy.
func (c *Cache) Snapshot() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	stats := c.stats
	stats.Entries = len(c.entries)
	stats.CurrentWeight = c.currentWeight
	stats.Inflight = len(c.flights)
	return stats
}

// Close rejects new loads, cancels cold work, waits for leases, and releases
// all compiled Programs. A failed release remains retryable.
func (c *Cache) Close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("worker cache close context is required")
	}
	for {
		c.mu.Lock()
		if c.released {
			c.mu.Unlock()
			return nil
		}
		if !c.closing {
			c.closing = true
			c.cancelFlightsLocked()
		}
		if c.releasing {
			done := c.releaseDone
			c.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		drained := c.drained
		c.mu.Unlock()
		select {
		case <-drained:
		case <-ctx.Done():
			return ctx.Err()
		}

		c.mu.Lock()
		if c.releasing {
			c.mu.Unlock()
			continue
		}
		programs := c.detachProgramsLocked()
		programs = append(programs, c.closePending...)
		c.closePending = nil
		c.releasing = true
		c.releaseDone = make(chan struct{})
		done := c.releaseDone
		c.mu.Unlock()

		failed, closeErr := closePrograms(ctx, programs)
		c.mu.Lock()
		c.closePending = failed
		c.releasing = false
		if closeErr == nil {
			c.released = true
		} else {
			c.stats.CloseErrors += uint64(len(failed))
		}
		close(done)
		c.mu.Unlock()
		return closeErr
	}
}

func (c *Cache) awaitFlight(
	ctx context.Context,
	running *flight,
	coalesced bool,
) (*Lease, Result, error) {
	select {
	case <-running.done:
		if err := ctx.Err(); err != nil {
			c.abandonFlight(running)
			return nil, Result{Key: running.key, Coalesced: coalesced}, err
		}
		c.mu.Lock()
		cached := running.entry
		result := running.result
		result.Coalesced = coalesced
		err := running.err
		c.mu.Unlock()
		if err != nil {
			return nil, result, err
		}
		return &Lease{cache: c, entry: cached}, result, nil
	case <-ctx.Done():
		c.abandonFlight(running)
		return nil, Result{Key: running.key, Coalesced: coalesced}, ctx.Err()
	}
}

func (c *Cache) abandonFlight(running *flight) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if running.waiters == 0 {
		return
	}
	running.waiters--
	if running.finished && running.entry != nil {
		running.entry.refs--
	}
	if !running.finished && running.waiters == 0 {
		running.cancel()
	}
}

func (c *Cache) runFlight(
	ctx context.Context,
	key Key,
	spec ModuleSpec,
	running *flight,
	reservation *loadReservation,
) {
	defer c.end()
	if err := reservation.wait(ctx); err != nil {
		c.completeFlight(key, spec.ArtifactSize, nil, Result{Key: key}, running, err)
		return
	}
	defer reservation.release()
	program, result, loadErr := c.loadAndCompile(ctx, key, spec)
	c.completeFlight(key, spec.ArtifactSize, program, result, running, loadErr)
}

func (c *Cache) completeFlight(
	key Key,
	weight int64,
	program *wasmexec.Program,
	result Result,
	running *flight,
	loadErr error,
) {
	var programsToClose []*wasmexec.Program
	c.mu.Lock()
	delete(c.flights, key)
	running.result = result

	switch {
	case c.closing:
		running.err = ErrClosed
	case loadErr != nil:
		running.err = loadErr
	case running.waiters == 0:
		running.err = context.Canceled
	default:
		victims, capacityErr := c.selectVictimsLocked(weight)
		if capacityErr != nil {
			c.stats.RejectionsFull++
			running.err = capacityErr
		} else {
			for _, victim := range victims {
				delete(c.entries, victim.entry.key)
				c.lru.Remove(victim.entry.element)
				c.currentWeight -= victim.entry.weight
				programsToClose = append(programsToClose, victim.entry.program)
				switch victim.reason {
				case EvictionCapacityBytes:
					c.stats.EvictionsBytes++
				case EvictionCapacityEntries:
					c.stats.EvictionsEntries++
				}
			}
			c.stats.EvictionsCapacity += uint64(len(victims))
			running.result.Evicted = len(victims)
			cached := &entry{
				key:     key,
				program: program,
				weight:  weight,
				refs:    running.waiters,
			}
			cached.element = c.lru.PushFront(cached)
			c.entries[key] = cached
			c.currentWeight += weight
			running.entry = cached
		}
	}
	if running.entry == nil && program != nil {
		programsToClose = append(programsToClose, program)
	}
	running.finished = true
	running.cancel()
	close(running.done)
	c.mu.Unlock()

	c.closeEvicted(programsToClose)
}

func (c *Cache) selectVictimsLocked(weight int64) ([]eviction, error) {
	remainingWeight := c.currentWeight
	remainingEntries := len(c.entries)
	victims := make([]eviction, 0)
	for element := c.lru.Back(); element != nil; element = element.Prev() {
		overBytes := remainingWeight+weight > c.maxWeightBytes
		overEntries := remainingEntries+1 > c.maxEntries
		if !overBytes && !overEntries {
			break
		}
		candidate := element.Value.(*entry)
		if candidate.refs != 0 {
			continue
		}
		reason := EvictionCapacityEntries
		if overBytes {
			reason = EvictionCapacityBytes
		}
		victims = append(victims, eviction{entry: candidate, reason: reason})
		remainingWeight -= candidate.weight
		remainingEntries--
	}
	if remainingWeight+weight > c.maxWeightBytes || remainingEntries+1 > c.maxEntries {
		return nil, ErrFull
	}
	return victims, nil
}

func (c *Cache) closeEvicted(programs []*wasmexec.Program) {
	failed, closeErr := closePrograms(context.Background(), programs)
	if closeErr == nil {
		return
	}
	c.mu.Lock()
	c.stats.CloseErrors += uint64(len(failed))
	c.closePending = append(c.closePending, failed...)
	c.closing = true
	c.cancelFlightsLocked()
	c.mu.Unlock()
}

func (c *Cache) cancelFlightsLocked() {
	for _, running := range c.flights {
		running.cancel()
	}
}

func (c *Cache) detachProgramsLocked() []*wasmexec.Program {
	programs := make([]*wasmexec.Program, 0, len(c.entries))
	for _, cached := range c.entries {
		programs = append(programs, cached.program)
	}
	clear(c.entries)
	c.lru.Init()
	c.currentWeight = 0
	return programs
}

func (c *Cache) begin() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		return false
	}
	c.startLocked()
	return true
}

func (c *Cache) startLocked() {
	if c.active == 0 {
		c.drained = make(chan struct{})
	}
	c.active++
}

func (c *Cache) end() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.endLocked()
}

func (c *Cache) endLocked() {
	c.active--
	if c.active == 0 {
		close(c.drained)
	}
}

func closePrograms(ctx context.Context, programs []*wasmexec.Program) ([]*wasmexec.Program, error) {
	failed := make([]*wasmexec.Program, 0)
	var closeErr error
	for _, program := range programs {
		if err := program.Close(ctx); err != nil {
			failed = append(failed, program)
			closeErr = errors.Join(closeErr, fmt.Errorf("closing cached program: %w", err))
		}
	}
	return failed, closeErr
}
