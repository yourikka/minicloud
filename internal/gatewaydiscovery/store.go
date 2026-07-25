package gatewaydiscovery

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/discovery"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/servingauth"
)

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// Store is safe for concurrent Watch, health, and invocation lookups.
type Store struct {
	clock        servingauth.MonotonicClock
	maxStale     time.Duration
	maxFunctions int

	mu             sync.Mutex
	functions      map[string]functionRecord
	position       eventPosition
	fullSynced     bool
	needsFullSync  bool
	watchConnected bool
	lastElapsed    time.Duration
	clockHealthy   bool
}

// New validates memory and time bounds and creates an unsynchronized Store.
func New(config Config) (*Store, error) {
	if config.Clock == nil {
		started := time.Now()
		config.Clock = func() time.Duration { return time.Since(started) }
	}
	maxStale := DefaultMaxStale
	if config.MaxStale != nil {
		maxStale = *config.MaxStale
	}
	if config.MaxFunctions == 0 {
		config.MaxFunctions = DefaultMaxFunctions
	}
	if maxStale < 0 || maxStale > HardMaxStale {
		return nil, errors.New("gateway serving max stale is outside v1 bounds")
	}
	if config.MaxFunctions < 1 || config.MaxFunctions > HardMaxFunctions {
		return nil, errors.New("gateway function limit is outside v1 bounds")
	}
	elapsed := config.Clock()
	if elapsed < 0 {
		return nil, errors.New("gateway monotonic clock returned negative elapsed time")
	}
	return &Store{
		clock:         config.Clock,
		maxStale:      maxStale,
		maxFunctions:  config.MaxFunctions,
		functions:     make(map[string]functionRecord),
		lastElapsed:   elapsed,
		clockHealthy:  true,
		needsFullSync: true,
	}, nil
}

// Apply validates and atomically installs one complete Serving Watch event.
func (s *Store) Apply(event Event) error {
	if s == nil {
		return errors.New("gateway discovery store is nil")
	}
	normalized, fingerprint, err := normalizeEvent(event, s.maxFunctions)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.clockHealthy {
		return classified(problem.CodeControlPlaneStale, "gateway monotonic clock is unhealthy")
	}
	if normalized.DiscoveryEpoch < s.position.epoch {
		return classified(problem.CodeStaleGeneration, "serving discovery epoch regressed")
	}
	if samePosition(normalized, s.position) {
		if fingerprint == s.position.fingerprint {
			return nil
		}
		return classified(problem.CodeStaleGeneration, "serving watch position was reused with different content")
	}
	if normalized.Full {
		return s.applyFullLocked(normalized, fingerprint)
	}
	return s.applyIncrementalLocked(normalized, fingerprint)
}

func (s *Store) applyFullLocked(event Event, fingerprint digest.SHA256) error {
	if event.DiscoveryEpoch == s.position.epoch && event.ServingSequence < s.position.sequence {
		return classified(problem.CodeStaleGeneration, "full sync serving sequence regressed")
	}
	for _, snapshot := range event.Snapshots {
		if current, exists := s.functions[snapshot.FunctionID]; exists {
			if err := revisionsDoNotRegress(current.snapshot, snapshot); err != nil {
				return err
			}
		}
	}
	now, err := s.elapsedLocked()
	if err != nil {
		return err
	}
	next := make(map[string]functionRecord, len(event.Snapshots))
	for _, snapshot := range event.Snapshots {
		current := s.functions[snapshot.FunctionID]
		next[snapshot.FunctionID] = functionRecord{
			snapshot:   snapshot.Clone(),
			receivedAt: now,
			suppressed: retainedSuppressions(current.suppressed, snapshot.Endpoints),
		}
	}
	s.functions = next
	s.position = eventPosition{epoch: event.DiscoveryEpoch, sequence: event.ServingSequence, fingerprint: fingerprint}
	s.fullSynced = true
	s.needsFullSync = false
	s.watchConnected = true
	return nil
}

func (s *Store) applyIncrementalLocked(event Event, fingerprint digest.SHA256) error {
	if !s.fullSynced || s.needsFullSync {
		return classified(problem.CodeControlPlaneStale, "gateway requires a complete serving Full Sync")
	}
	if event.DiscoveryEpoch > s.position.epoch {
		s.needsFullSync = true
		return classified(problem.CodeControlPlaneStale, "a higher discovery epoch requires Full Sync")
	}
	if event.ServingSequence <= s.position.sequence {
		return classified(problem.CodeStaleGeneration, "serving sequence regressed")
	}
	if s.position.sequence == math.MaxUint64 || event.ServingSequence != s.position.sequence+1 {
		s.needsFullSync = true
		return classified(problem.CodeControlPlaneStale, "serving sequence gap requires Full Sync")
	}
	snapshot := event.Snapshots[0]
	if current, exists := s.functions[snapshot.FunctionID]; exists {
		if err := revisionsDoNotRegress(current.snapshot, snapshot); err != nil {
			return err
		}
	} else if len(s.functions) >= s.maxFunctions {
		return classified(problem.CodeOverloaded, "gateway function serving cache is full")
	}
	now, err := s.elapsedLocked()
	if err != nil {
		return err
	}
	current := s.functions[snapshot.FunctionID]
	s.functions[snapshot.FunctionID] = functionRecord{
		snapshot:   snapshot.Clone(),
		receivedAt: now,
		suppressed: retainedSuppressions(current.suppressed, snapshot.Endpoints),
	}
	s.position = eventPosition{epoch: event.DiscoveryEpoch, sequence: event.ServingSequence, fingerprint: fingerprint}
	s.watchConnected = true
	return nil
}

// SetWatchConnected records transport liveness without refreshing any LKG
// receive point. A zero MaxStale Store serves only while connected.
func (s *Store) SetWatchConnected(connected bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.watchConnected = connected
	s.mu.Unlock()
}

// Lookup returns one complete, defensively copied Function snapshot after LKG
// and local endpoint suppression checks.
func (s *Store) Lookup(functionID string) (View, error) {
	if s == nil {
		return View{}, errors.New("gateway discovery store is nil")
	}
	if !identifierPattern.MatchString(functionID) {
		return View{}, problem.Invalid("function_id", "must be a valid identifier")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now, err := s.elapsedLocked()
	if err != nil {
		return View{}, err
	}
	if !s.fullSynced || s.needsFullSync {
		return View{}, classified(problem.CodeControlPlaneStale, "gateway serving view requires Full Sync")
	}
	record, exists := s.functions[functionID]
	if !exists {
		return View{}, classified(problem.CodeNotFound, "function serving snapshot was not found")
	}
	if !s.usableLocked(record, now) {
		return View{}, classified(problem.CodeControlPlaneStale, "function serving snapshot is stale")
	}
	return visibleSnapshot(record), nil
}

// LookupAll returns one atomic, sorted view of every Function in the current
// Full Sync generation. Any stale member fails the whole read.
func (s *Store) LookupAll() ([]View, error) {
	if s == nil {
		return nil, errors.New("gateway discovery store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now, err := s.elapsedLocked()
	if err != nil {
		return nil, err
	}
	if !s.fullSynced || s.needsFullSync {
		return nil, classified(problem.CodeControlPlaneStale, "gateway serving view requires Full Sync")
	}
	result := make([]View, 0, len(s.functions))
	for _, record := range s.functions {
		if !s.usableLocked(record, now) {
			return nil, classified(problem.CodeControlPlaneStale, "gateway serving view contains a stale function")
		}
		result = append(result, visibleSnapshot(record))
	}
	slices.SortFunc(result, func(left, right View) int {
		return compareString(left.Snapshot.FunctionID, right.Snapshot.FunctionID)
	})
	return result, nil
}

// SuppressEndpoint applies a local health failure only when the complete
// authoritative Assignment fence still exists. It never refreshes LKG time.
func (s *Store) SuppressEndpoint(functionID string, assignment servingauth.AssignmentIdentity) error {
	if s == nil {
		return errors.New("gateway discovery store is nil")
	}
	if !identifierPattern.MatchString(functionID) {
		return problem.Invalid("function_id", "must be a valid identifier")
	}
	if err := assignment.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.functions[functionID]
	if !exists {
		return classified(problem.CodeNotFound, "function serving snapshot was not found")
	}
	for _, endpoint := range record.snapshot.Endpoints {
		if endpoint.Assignment.AssignmentID != assignment.AssignmentID {
			continue
		}
		if endpoint.Assignment != assignment {
			return classified(problem.CodeStaleAssignment, "endpoint failure fence is stale")
		}
		if record.suppressed == nil {
			record.suppressed = make(map[string]servingauth.AssignmentIdentity)
		}
		record.suppressed[assignment.AssignmentID] = assignment
		s.functions[functionID] = record
		return nil
	}
	return classified(problem.CodeStaleAssignment, "endpoint failure does not match an authoritative endpoint")
}

// Status returns bounded state without exposing Function or credential data.
func (s *Store) Status() Status {
	if s == nil {
		return Status{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{
		DiscoveryEpoch:  s.position.epoch,
		ServingSequence: s.position.sequence,
		Functions:       len(s.functions),
		FullSynced:      s.fullSynced,
		NeedsFullSync:   s.needsFullSync,
		WatchConnected:  s.watchConnected,
		ClockHealthy:    s.clockHealthy,
	}
}

func normalizeEvent(event Event, maxFunctions int) (Event, digest.SHA256, error) {
	if event.DiscoveryEpoch == 0 || event.ServingSequence == 0 {
		return Event{}, "", problem.Invalid("serving_event", "epoch and sequence must be greater than zero")
	}
	if len(event.Snapshots) > maxFunctions {
		return Event{}, "", problem.Invalid("serving_event", "contains too many Function snapshots")
	}
	if !event.Full && len(event.Snapshots) != 1 {
		return Event{}, "", problem.Invalid("serving_event", "an incremental event must contain exactly one Function snapshot")
	}
	normalized := Event{
		Full:            event.Full,
		DiscoveryEpoch:  event.DiscoveryEpoch,
		ServingSequence: event.ServingSequence,
		Snapshots:       make([]discovery.Snapshot, len(event.Snapshots)),
	}
	seen := make(map[string]struct{}, len(event.Snapshots))
	for index, snapshot := range event.Snapshots {
		cloned := snapshot.Clone()
		if cloned.DiscoveryEpoch != event.DiscoveryEpoch || cloned.ServingSequence != event.ServingSequence {
			return Event{}, "", problem.Invalid("serving_event", "snapshot position does not match the event")
		}
		if err := cloned.Validate(); err != nil {
			return Event{}, "", err
		}
		if _, exists := seen[cloned.FunctionID]; exists {
			return Event{}, "", problem.Invalid("serving_event", "contains a duplicate Function snapshot")
		}
		seen[cloned.FunctionID] = struct{}{}
		normalized.Snapshots[index] = cloned
	}
	slices.SortFunc(normalized.Snapshots, func(left, right discovery.Snapshot) int {
		return compareString(left.FunctionID, right.FunctionID)
	})
	fingerprint, err := eventFingerprint(normalized)
	if err != nil {
		return Event{}, "", err
	}
	return normalized, fingerprint, nil
}

func eventFingerprint(event Event) (digest.SHA256, error) {
	type item struct {
		FunctionID string        `json:"function_id"`
		Checksum   digest.SHA256 `json:"checksum"`
	}
	type payload struct {
		Full            bool   `json:"full"`
		DiscoveryEpoch  uint64 `json:"discovery_epoch"`
		ServingSequence uint64 `json:"serving_sequence"`
		Snapshots       []item `json:"snapshots"`
	}
	value := payload{Full: event.Full, DiscoveryEpoch: event.DiscoveryEpoch, ServingSequence: event.ServingSequence}
	value.Snapshots = make([]item, 0, len(event.Snapshots))
	for _, snapshot := range event.Snapshots {
		value.Snapshots = append(value.Snapshots, item{FunctionID: snapshot.FunctionID, Checksum: snapshot.Checksum})
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("marshaling serving event fingerprint: %w", err)
	}
	return digest.CanonicalJSON("serving-watch-event", "v1", encoded)
}

func revisionsDoNotRegress(current, next discovery.Snapshot) error {
	if next.Function.ResourceRevision < current.Function.ResourceRevision ||
		next.Trigger.ResourceRevision < current.Trigger.ResourceRevision {
		return classified(problem.CodeStaleGeneration, "function or trigger resource revision regressed")
	}
	if next.Function.ResourceRevision == current.Function.ResourceRevision &&
		!sameFunction(current.Function, next.Function) {
		return classified(problem.CodeStaleGeneration, "function content changed without a resource revision")
	}
	if next.Trigger.ResourceRevision == current.Trigger.ResourceRevision &&
		!sameHTTPTrigger(current.Trigger, next.Trigger) {
		return classified(problem.CodeStaleGeneration, "http trigger content changed without a resource revision")
	}
	if current.Route.Present && !next.Route.Present {
		return classified(problem.CodeStaleGeneration, "an existing route cannot disappear from an incremental snapshot")
	}
	if current.Route.Present && next.Route.Present &&
		(next.Route.ResourceRevision < current.Route.ResourceRevision ||
			next.Route.RouteRevision < current.Route.RouteRevision) {
		return classified(problem.CodeStaleGeneration, "route revision regressed")
	}
	if current.Route.Present && next.Route.Present &&
		next.Route.ResourceRevision == current.Route.ResourceRevision &&
		next.Route.RouteRevision == current.Route.RouteRevision &&
		!sameRoute(current.Route, next.Route) {
		return classified(problem.CodeStaleGeneration, "route content changed without a resource or route revision")
	}
	return nil
}

func sameFunction(left, right discovery.Function) bool {
	return left == right
}

func sameHTTPTrigger(left, right discovery.HTTPTrigger) bool {
	if left.ID != right.ID ||
		left.FunctionID != right.FunctionID ||
		left.ResourceRevision != right.ResourceRevision ||
		left.Enabled != right.Enabled ||
		left.AuthPolicy != right.AuthPolicy {
		return false
	}
	if left.TokenVerifierDigest == nil || right.TokenVerifierDigest == nil {
		return left.TokenVerifierDigest == nil && right.TokenVerifierDigest == nil
	}
	return *left.TokenVerifierDigest == *right.TokenVerifierDigest
}

func sameRoute(left, right discovery.Route) bool {
	return left.Present == right.Present &&
		left.FunctionID == right.FunctionID &&
		left.ResourceRevision == right.ResourceRevision &&
		left.RouteRevision == right.RouteRevision &&
		left.Enabled == right.Enabled &&
		slices.Equal(left.Targets, right.Targets) &&
		left.Affinity == right.Affinity &&
		left.AffinityHeader == right.AffinityHeader &&
		left.HashVersion == right.HashVersion &&
		left.SaltID == right.SaltID &&
		bytes.Equal(left.Salt, right.Salt)
}

func retainedSuppressions(
	previous map[string]servingauth.AssignmentIdentity,
	endpoints []discovery.Endpoint,
) map[string]servingauth.AssignmentIdentity {
	if len(previous) == 0 {
		return make(map[string]servingauth.AssignmentIdentity)
	}
	retained := make(map[string]servingauth.AssignmentIdentity)
	for _, endpoint := range endpoints {
		if identity, exists := previous[endpoint.Assignment.AssignmentID]; exists && identity == endpoint.Assignment {
			retained[identity.AssignmentID] = identity
		}
	}
	return retained
}

func visibleSnapshot(record functionRecord) View {
	snapshot := record.snapshot.Clone()
	view := View{Snapshot: snapshot, Endpoints: slices.Clone(snapshot.Endpoints)}
	if len(record.suppressed) == 0 {
		return view
	}
	visible := view.Endpoints[:0]
	for _, endpoint := range view.Endpoints {
		if identity, suppressed := record.suppressed[endpoint.Assignment.AssignmentID]; suppressed && identity == endpoint.Assignment {
			continue
		}
		visible = append(visible, endpoint)
	}
	view.Endpoints = visible
	return view
}

func (s *Store) usableLocked(record functionRecord, now time.Duration) bool {
	if s.maxStale == 0 {
		return s.watchConnected
	}
	return now >= record.receivedAt && now-record.receivedAt < s.maxStale
}

func (s *Store) elapsedLocked() (time.Duration, error) {
	now := s.clock()
	if !s.clockHealthy || now < s.lastElapsed {
		s.clockHealthy = false
		s.needsFullSync = true
		return 0, classified(problem.CodeControlPlaneStale, "gateway monotonic clock is unhealthy")
	}
	s.lastElapsed = now
	return now, nil
}

func samePosition(event Event, position eventPosition) bool {
	return event.DiscoveryEpoch == position.epoch && event.ServingSequence == position.sequence
}

func compareString(left, right string) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func classified(code problem.Code, message string) error {
	return &problem.Error{Code: code, Message: message}
}
