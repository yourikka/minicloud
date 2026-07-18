// Package scheduler contains deterministic, transport-neutral placement rules.
// It does not persist Raft state or create globally unique IDs.
package scheduler

import (
	"regexp"
	"slices"
	"unicode/utf8"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/servingauth"
	"github.com/yourikka/minicloud/internal/wasmprofile"
	"github.com/yourikka/minicloud/internal/workercache"
)

const (
	DefaultMaxDecisions = 100_000
	HardMaxDecisions    = 100_000
	MaxWorkers          = 1_000
	MaxRequiredLabels   = 32
	MaxExistingWorkers  = 100
	MaxCacheHints       = 4_096
	MaxWorkerSlots      = 1_024
)

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
var labelKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,62}$`)

// SchedulingIntent is the persisted Worker control intent.
type SchedulingIntent string

const (
	IntentSchedulable SchedulingIntent = "Schedulable"
	IntentDraining    SchedulingIntent = "Draining"
	IntentRemoved     SchedulingIntent = "Removed"
)

// SessionState is the Leader-derived Worker session condition.
type SessionState string

const (
	SessionJoining     SessionState = "Joining"
	SessionReady       SessionState = "Ready"
	SessionSuspect     SessionState = "Suspect"
	SessionUnavailable SessionState = "Unavailable"
)

// RuntimeProfile describes the exact runtime compatibility advertised by a
// Worker. It is an eligibility input, not a cache preference.
type RuntimeProfile struct {
	Name           string
	Version        string
	Engine         string
	GOOS           string
	GOARCH         string
	ABI            string
	HostAPI        string
	FeatureProfile string
	MemoryMiB      uint32
}

// Capacity is the Worker resource budget and current allocation snapshot.
type Capacity struct {
	MemoryMiB          uint64
	AllocatedMemoryMiB uint64
	Slots              uint64
	AllocatedSlots     uint64
}

// CacheHints are advisory observations used only to rank already eligible
// Workers. They never make an incompatible Worker eligible.
type CacheHints struct {
	Artifacts map[digest.SHA256]struct{}
	Compiled  map[workercache.Key]struct{}
}

// WorkerSnapshot is a defensive, point-in-time Worker observation.
type WorkerSnapshot struct {
	Session  servingauth.WorkerSession
	Runtime  RuntimeProfile
	Intent   SchedulingIntent
	State    SessionState
	Capacity Capacity
	Labels   map[string]string
	Cache    CacheHints
}

// PlacementRequest contains all immutable inputs for one Assignment decision.
// AssignmentID and CommandID must come from the control plane; this package
// deliberately does not generate globally unique identifiers.
type PlacementRequest struct {
	CommandID            string
	AssignmentID         string
	VersionID            string
	ArtifactDigest       digest.SHA256
	ArtifactSize         int64
	ABI                  string
	HostAPI              string
	FeatureProfile       string
	RuntimeName          string
	RuntimeVersion       string
	RuntimeEngine        string
	GOOS                 string
	GOARCH               string
	MemoryMiB            uint32
	RequiredSlots        uint64
	AdmissionEpoch       uint64
	DeploymentGeneration uint64
	PolicyDigest         digest.SHA256
	CompiledCacheKey     workercache.Key
	RequiredLabels       map[string]string
	ExistingWorkerIDs    map[string]struct{}
}

// Assignment is the immutable placement result returned to the caller.
type Assignment struct {
	CommandID            string
	AssignmentID         string
	Worker               servingauth.WorkerSession
	VersionID            string
	ArtifactDigest       digest.SHA256
	ArtifactSize         int64
	ABI                  string
	HostAPI              string
	FeatureProfile       string
	MemoryMiB            uint32
	RequiredSlots        uint64
	AdmissionEpoch       uint64
	DeploymentGeneration uint64
	PolicyDigest         digest.SHA256
}

// DecisionStatus describes how a Plan call was satisfied.
type DecisionStatus string

const (
	StatusApplied          DecisionStatus = "applied"
	StatusDuplicate        DecisionStatus = "duplicate"
	StatusAlreadySatisfied DecisionStatus = "already_satisfied"
)

// FilterReason is a stable explanation for excluding a Worker.
type FilterReason string

const (
	ReasonInvalidObservation FilterReason = "invalid_observation"
	ReasonIntent             FilterReason = "intent_not_schedulable"
	ReasonSession            FilterReason = "session_not_ready"
	ReasonRuntime            FilterReason = "runtime_incompatible"
	ReasonLabels             FilterReason = "labels_not_satisfied"
	ReasonMemory             FilterReason = "memory_capacity"
	ReasonSlots              FilterReason = "slot_capacity"
)

// Candidate is the explainable result of applying hard filters and soft rank
// signals to one Worker. WorkerID is retained even for invalid observations.
type Candidate struct {
	WorkerID        string
	Session         servingauth.WorkerSession
	Eligible        bool
	Reasons         []FilterReason
	CacheScore      uint8
	LoadNumerator   uint64
	LoadDenominator uint64
}

// LeaderBarrier proves that the current Leader has applied its term barrier.
// The proof itself is supplied by the future Raft layer; Planner only stores
// and compares the immutable evidence.
type LeaderBarrier struct {
	Term         uint64
	AppliedIndex uint64
	Ready        bool
}

// Decision is a deterministic placement result with all filter explanations.
type Decision struct {
	Status     DecisionStatus
	Assignment Assignment
	Barrier    LeaderBarrier
	Candidates []Candidate
}

func (s SchedulingIntent) valid() bool {
	return s == IntentSchedulable || s == IntentDraining || s == IntentRemoved
}

func (s SessionState) valid() bool {
	return s == SessionJoining || s == SessionReady ||
		s == SessionSuspect || s == SessionUnavailable
}

func (r RuntimeProfile) compatible(request PlacementRequest) bool {
	return r.Name == request.RuntimeName &&
		r.Version == request.RuntimeVersion &&
		r.Engine == request.RuntimeEngine &&
		r.GOOS == request.GOOS &&
		r.GOARCH == request.GOARCH &&
		r.ABI == request.ABI &&
		r.HostAPI == request.HostAPI &&
		r.FeatureProfile == request.FeatureProfile &&
		r.MemoryMiB == request.MemoryMiB
}

func (r PlacementRequest) Validate() error {
	for field, value := range map[string]string{
		"command_id":    r.CommandID,
		"assignment_id": r.AssignmentID,
		"version_id":    r.VersionID,
	} {
		if !identifierPattern.MatchString(value) {
			return problem.Invalid(field, "must be a valid identifier")
		}
	}
	if _, err := digest.ParseSHA256(r.ArtifactDigest.String()); err != nil {
		return problem.Invalid("artifact_digest", "must be a lowercase sha-256 digest")
	}
	if r.ArtifactSize < 1 || r.ArtifactSize > model.MaxArtifactBytes {
		return problem.Invalid("artifact_size", "must be within the v1 artifact size limit")
	}
	if r.ABI != model.ABIWASICommandV1 {
		return problem.Invalid("abi", "is not supported")
	}
	if r.HostAPI != model.HostAPIProfileNone {
		return problem.Invalid("host_api", "is not supported")
	}
	if r.RuntimeName != wasmprofile.RuntimeName || r.RuntimeVersion != wasmprofile.RuntimeVersion ||
		r.RuntimeEngine == "" || r.GOOS == "" || r.GOARCH == "" || r.FeatureProfile != wasmprofile.FeatureProfile ||
		r.MemoryMiB == 0 || r.RequiredSlots == 0 {
		return problem.Invalid("runtime_profile", "runtime, platform, feature, memory, and slots are required")
	}
	if _, err := wasmprofile.New(r.RuntimeEngine, r.MemoryMiB); err != nil {
		return problem.Invalid("runtime_profile", "engine and memory tier are unsupported")
	}
	if r.RequiredSlots > MaxWorkerSlots {
		return problem.Invalid("required_slots", "exceeds the v1 worker slot limit")
	}
	if r.AdmissionEpoch == 0 || r.DeploymentGeneration == 0 {
		return problem.Invalid("generation", "admission epoch and deployment generation are required")
	}
	if _, err := digest.ParseSHA256(r.PolicyDigest.String()); err != nil {
		return problem.Invalid("policy_digest", "must be a lowercase sha-256 digest")
	}
	if key := r.CompiledCacheKey; key.ArtifactDigest != "" {
		if key.ArtifactDigest != r.ArtifactDigest || key.ArtifactSize != r.ArtifactSize ||
			key.RuntimeName != r.RuntimeName || key.RuntimeVersion != r.RuntimeVersion ||
			key.Engine != r.RuntimeEngine || key.GOOS != r.GOOS || key.GOARCH != r.GOARCH ||
			key.ABI != r.ABI || key.HostAPIProfile != r.HostAPI ||
			key.RuntimeFeatureProfile != r.FeatureProfile || key.MemoryLimitMiB != r.MemoryMiB {
			return problem.Invalid("compiled_cache_key", "does not match placement runtime inputs")
		}
	}
	if len(r.RequiredLabels) > MaxRequiredLabels {
		return problem.Invalid("required_labels", "contains too many labels")
	}
	for key, value := range r.RequiredLabels {
		if !labelKeyPattern.MatchString(key) {
			return problem.Invalid("required_labels", "contains an invalid key")
		}
		if !utf8.ValidString(value) || len(value) > 256 {
			return problem.Invalid("required_labels", "contains an invalid value")
		}
	}
	if len(r.ExistingWorkerIDs) > MaxExistingWorkers {
		return problem.Invalid("existing_worker_ids", "contains too many workers")
	}
	for id := range r.ExistingWorkerIDs {
		if !identifierPattern.MatchString(id) {
			return problem.Invalid("existing_worker_ids", "contains an invalid worker id")
		}
	}
	return nil
}

func (w WorkerSnapshot) validate() error {
	if err := w.Session.Validate(); err != nil {
		return err
	}
	if !w.Intent.valid() || !w.State.valid() {
		return problem.Invalid("worker_state", "intent or session state is unsupported")
	}
	if w.Runtime.Name != wasmprofile.RuntimeName || w.Runtime.Version != wasmprofile.RuntimeVersion ||
		w.Runtime.GOOS == "" || w.Runtime.GOARCH == "" || w.Runtime.ABI != model.ABIWASICommandV1 ||
		w.Runtime.HostAPI != model.HostAPIProfileNone || w.Runtime.FeatureProfile != wasmprofile.FeatureProfile {
		return problem.Invalid("runtime", "runtime profile is incomplete")
	}
	if _, err := wasmprofile.New(w.Runtime.Engine, w.Runtime.MemoryMiB); err != nil {
		return problem.Invalid("runtime", "runtime engine or memory tier is unsupported")
	}
	if w.Capacity.AllocatedMemoryMiB > w.Capacity.MemoryMiB ||
		w.Capacity.AllocatedSlots > w.Capacity.Slots || w.Capacity.Slots > MaxWorkerSlots {
		return problem.Invalid("capacity", "allocation exceeds capacity")
	}
	if len(w.Labels) > MaxRequiredLabels || len(w.Cache.Artifacts) > MaxCacheHints || len(w.Cache.Compiled) > MaxCacheHints {
		return problem.Invalid("worker_observation", "contains more entries than the v1 bound")
	}
	for key, value := range w.Labels {
		if !labelKeyPattern.MatchString(key) || !utf8.ValidString(value) || len(value) > 256 {
			return problem.Invalid("labels", "contains an invalid label")
		}
	}
	return nil
}

func mapsClone(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func cloneHints(hints CacheHints) CacheHints {
	clone := CacheHints{}
	if hints.Artifacts != nil {
		clone.Artifacts = make(map[digest.SHA256]struct{}, len(hints.Artifacts))
		for key := range hints.Artifacts {
			clone.Artifacts[key] = struct{}{}
		}
	}
	if hints.Compiled != nil {
		clone.Compiled = make(map[workercache.Key]struct{}, len(hints.Compiled))
		for key := range hints.Compiled {
			clone.Compiled[key] = struct{}{}
		}
	}
	return clone
}

func cloneCandidate(candidate Candidate) Candidate {
	candidate.Reasons = slices.Clone(candidate.Reasons)
	return candidate
}
