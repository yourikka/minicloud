package scheduler

import (
	"encoding/json"
	"errors"
	"math/bits"
	"slices"
	"sync"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/problem"
)

// Config bounds retained idempotency records. Records are removed only by an
// explicit ACK from the control plane; the Planner never silently evicts them.
type Config struct {
	MaxDecisions int
}

// Planner performs deterministic placement and is safe for concurrent callers.
// It is intentionally not a Raft FSM: callers must install a current Leader
// Barrier before asking it to create an external Assignment side effect.
type Planner struct {
	mu           sync.Mutex
	maxDecisions int
	barrier      LeaderBarrier
	decisions    map[string]decisionRecord
	assignments  map[string]string
}

type decisionRecord struct {
	fingerprint digest.SHA256
	decision    Decision
}

// New validates the bounded decision-retention configuration.
func New(config Config) (*Planner, error) {
	if config.MaxDecisions == 0 {
		config.MaxDecisions = DefaultMaxDecisions
	}
	if config.MaxDecisions < 1 || config.MaxDecisions > HardMaxDecisions {
		return nil, errors.New("scheduler decision limit is outside v1 bounds")
	}
	return &Planner{
		maxDecisions: config.MaxDecisions,
		decisions:    make(map[string]decisionRecord),
		assignments:  make(map[string]string),
	}, nil
}

// InstallBarrier records the current Leader's committed term barrier. A
// barrier that goes backwards is rejected, and a non-ready barrier cannot
// authorize scheduling.
func (p *Planner) InstallBarrier(barrier LeaderBarrier) error {
	if p == nil {
		return errors.New("scheduler planner is nil")
	}
	if barrier.Term == 0 || barrier.AppliedIndex == 0 {
		return problem.Invalid("leader_barrier", "term and applied index must be greater than zero")
	}
	if !barrier.Ready {
		return classified(problem.CodeNoQuorum, "leader barrier is not committed")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.barrier.Term != 0 && (barrier.Term < p.barrier.Term ||
		barrier.AppliedIndex < p.barrier.AppliedIndex) {
		return classified(problem.CodeStaleGeneration, "leader barrier moved backwards")
	}
	p.barrier = barrier
	return nil
}

// Barrier returns the currently installed barrier.
func (p *Planner) Barrier() LeaderBarrier {
	if p == nil {
		return LeaderBarrier{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.barrier
}

// InvalidateBarrier withdraws scheduling authority after leadership or quorum
// is lost. Retained idempotency records remain readable for exact retries, but
// no new Placement decision can be created until a fresh barrier is installed.
func (p *Planner) InvalidateBarrier() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.barrier.Ready = false
	p.mu.Unlock()
}

// Plan chooses one eligible Worker. The same Command ID is idempotent, while
// a new command for an already planned Assignment returns the original result.
func (p *Planner) Plan(request PlacementRequest, workers []WorkerSnapshot) (Decision, error) {
	if p == nil {
		return Decision{}, errors.New("scheduler planner is nil")
	}
	if err := request.Validate(); err != nil {
		return Decision{}, err
	}
	fingerprint, err := requestFingerprint(request)
	if err != nil {
		return Decision{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if existing, exists := p.decisions[request.CommandID]; exists {
		if existing.fingerprint != fingerprint {
			return Decision{}, classified(problem.CodeConflict, "command id was reused with different placement input")
		}
		decision := cloneDecision(existing.decision)
		decision.Status = StatusDuplicate
		return decision, nil
	}
	if commandID, exists := p.assignments[request.AssignmentID]; exists {
		existing := p.decisions[commandID]
		if existing.fingerprint != fingerprint {
			return Decision{}, classified(problem.CodeConflict, "assignment id was reused with different placement input")
		}
		decision := cloneDecision(existing.decision)
		decision.Status = StatusAlreadySatisfied
		return decision, nil
	}
	if !p.barrier.Ready {
		return Decision{}, classified(problem.CodeControlPlaneStale, "current Leader barrier is not installed")
	}
	if len(p.decisions) >= p.maxDecisions {
		return Decision{}, classified(problem.CodeOverloaded, "scheduler decision capacity is full")
	}
	if err := validateWorkerSet(workers); err != nil {
		return Decision{}, err
	}
	candidates, eligible := evaluateCandidates(request, workers)
	if len(eligible) == 0 {
		return Decision{Barrier: p.barrier, Candidates: candidates},
			classified(problem.CodeNoReadyReplica, "no Worker satisfies the placement constraints")
	}
	slices.SortFunc(eligible, compareEligible)
	chosen := eligible[0]
	assignment := Assignment{
		CommandID:            request.CommandID,
		AssignmentID:         request.AssignmentID,
		Worker:               chosen.worker.Session,
		VersionID:            request.VersionID,
		ArtifactDigest:       request.ArtifactDigest,
		ArtifactSize:         request.ArtifactSize,
		ABI:                  request.ABI,
		HostAPI:              request.HostAPI,
		FeatureProfile:       request.FeatureProfile,
		MemoryMiB:            request.MemoryMiB,
		RequiredSlots:        request.RequiredSlots,
		AdmissionEpoch:       request.AdmissionEpoch,
		DeploymentGeneration: request.DeploymentGeneration,
		PolicyDigest:         request.PolicyDigest,
	}
	decision := Decision{
		Status:     StatusApplied,
		Assignment: assignment,
		Barrier:    p.barrier,
		Candidates: candidates,
	}
	p.decisions[request.CommandID] = decisionRecord{fingerprint: fingerprint, decision: cloneDecision(decision)}
	p.assignments[request.AssignmentID] = request.CommandID
	return decision, nil
}

// Acknowledge removes a persisted/idempotent result after the control plane
// has incorporated it. It is explicit so the Planner cannot forget a command
// while a response may still be retried.
func (p *Planner) Acknowledge(commandID string) error {
	if p == nil {
		return errors.New("scheduler planner is nil")
	}
	if !identifierPattern.MatchString(commandID) {
		return problem.Invalid("command_id", "must be a valid identifier")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	record, exists := p.decisions[commandID]
	if !exists {
		return nil
	}
	delete(p.decisions, commandID)
	if p.assignments[record.decision.Assignment.AssignmentID] == commandID {
		delete(p.assignments, record.decision.Assignment.AssignmentID)
	}
	return nil
}

// DecisionCount reports retained idempotency records.
func (p *Planner) DecisionCount() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.decisions)
}

type eligibleCandidate struct {
	worker            WorkerSnapshot
	cacheScore        uint8
	spreadScore       uint8
	loadNumerator     uint64
	loadDenominator   uint64
	memoryNumerator   uint64
	memoryDenominator uint64
}

func evaluateCandidates(request PlacementRequest, workers []WorkerSnapshot) ([]Candidate, []eligibleCandidate) {
	candidates := make([]Candidate, 0, len(workers))
	eligible := make([]eligibleCandidate, 0, len(workers))
	seenIDs := make(map[string]struct{}, len(workers))
	for _, source := range workers {
		worker := cloneWorkerSnapshot(source)
		candidate := Candidate{WorkerID: worker.Session.WorkerID, Session: worker.Session}
		if _, exists := seenIDs[worker.Session.WorkerID]; exists {
			candidate.Reasons = append(candidate.Reasons, ReasonInvalidObservation)
			candidates = append(candidates, candidate)
			continue
		}
		seenIDs[worker.Session.WorkerID] = struct{}{}
		if err := worker.validate(); err != nil {
			candidate.Reasons = append(candidate.Reasons, ReasonInvalidObservation)
			candidates = append(candidates, candidate)
			continue
		}
		if worker.Intent != IntentSchedulable {
			candidate.Reasons = append(candidate.Reasons, ReasonIntent)
		}
		if worker.State != SessionReady {
			candidate.Reasons = append(candidate.Reasons, ReasonSession)
		}
		if !worker.Runtime.compatible(request) {
			candidate.Reasons = append(candidate.Reasons, ReasonRuntime)
		}
		if !labelsMatch(worker.Labels, request.RequiredLabels) {
			candidate.Reasons = append(candidate.Reasons, ReasonLabels)
		}
		memoryOK := worker.Capacity.MemoryMiB >= worker.Capacity.AllocatedMemoryMiB &&
			worker.Capacity.MemoryMiB-worker.Capacity.AllocatedMemoryMiB >= uint64(request.MemoryMiB)
		slotsOK := worker.Capacity.Slots >= worker.Capacity.AllocatedSlots &&
			worker.Capacity.Slots-worker.Capacity.AllocatedSlots >= request.RequiredSlots
		if !memoryOK {
			candidate.Reasons = append(candidate.Reasons, ReasonMemory)
		}
		if !slotsOK {
			candidate.Reasons = append(candidate.Reasons, ReasonSlots)
		}
		if len(candidate.Reasons) != 0 {
			candidates = append(candidates, candidate)
			continue
		}
		cacheScore := uint8(0)
		if _, exists := worker.Cache.Artifacts[request.ArtifactDigest]; exists {
			cacheScore = 1
		}
		if request.CompiledCacheKey.ArtifactDigest != "" {
			if _, exists := worker.Cache.Compiled[request.CompiledCacheKey]; exists {
				cacheScore = 2
			}
		}
		spreadScore := uint8(1)
		if _, exists := request.ExistingWorkerIDs[worker.Session.WorkerID]; exists {
			spreadScore = 0
		}
		candidate.Eligible = true
		candidate.CacheScore = cacheScore
		candidate.LoadNumerator = worker.Capacity.AllocatedSlots
		candidate.LoadDenominator = worker.Capacity.Slots
		eligible = append(eligible, eligibleCandidate{
			worker: worker, cacheScore: cacheScore, spreadScore: spreadScore,
			loadNumerator:     worker.Capacity.AllocatedSlots,
			loadDenominator:   worker.Capacity.Slots,
			memoryNumerator:   worker.Capacity.AllocatedMemoryMiB,
			memoryDenominator: worker.Capacity.MemoryMiB,
		})
		candidates = append(candidates, candidate)
	}
	slices.SortFunc(candidates, compareCandidates)
	return candidates, eligible
}

func labelsMatch(labels, required map[string]string) bool {
	for key, expected := range required {
		if labels[key] != expected {
			return false
		}
	}
	return true
}

func compareCandidates(left, right Candidate) int {
	if left.WorkerID < right.WorkerID {
		return -1
	}
	if left.WorkerID > right.WorkerID {
		return 1
	}
	if left.Session.BootID < right.Session.BootID {
		return -1
	}
	if left.Session.BootID > right.Session.BootID {
		return 1
	}
	if left.Session.SessionEpoch < right.Session.SessionEpoch {
		return -1
	}
	if left.Session.SessionEpoch > right.Session.SessionEpoch {
		return 1
	}
	return 0
}

func compareEligible(left, right eligibleCandidate) int {
	if left.cacheScore != right.cacheScore {
		if left.cacheScore > right.cacheScore {
			return -1
		}
		return 1
	}
	if left.spreadScore != right.spreadScore {
		if left.spreadScore > right.spreadScore {
			return -1
		}
		return 1
	}
	if compareRatio(left.loadNumerator, left.loadDenominator, right.loadNumerator, right.loadDenominator) != 0 {
		return compareRatio(left.loadNumerator, left.loadDenominator, right.loadNumerator, right.loadDenominator)
	}
	if compareRatio(left.memoryNumerator, left.memoryDenominator, right.memoryNumerator, right.memoryDenominator) != 0 {
		return compareRatio(left.memoryNumerator, left.memoryDenominator, right.memoryNumerator, right.memoryDenominator)
	}
	return compareCandidates(
		Candidate{WorkerID: left.worker.Session.WorkerID, Session: left.worker.Session},
		Candidate{WorkerID: right.worker.Session.WorkerID, Session: right.worker.Session},
	)
}

func compareRatio(leftNumerator, leftDenominator, rightNumerator, rightDenominator uint64) int {
	leftHigh, leftLow := bits.Mul64(leftNumerator, rightDenominator)
	rightHigh, rightLow := bits.Mul64(rightNumerator, leftDenominator)
	if leftHigh < rightHigh || (leftHigh == rightHigh && leftLow < rightLow) {
		return -1
	}
	if leftHigh > rightHigh || (leftHigh == rightHigh && leftLow > rightLow) {
		return 1
	}
	return 0
}

func validateWorkerSet(workers []WorkerSnapshot) error {
	if len(workers) > MaxWorkers {
		return problem.Invalid("workers", "contains more than the v1 worker observation limit")
	}
	seen := make(map[string]struct{}, len(workers))
	for _, worker := range workers {
		if _, exists := seen[worker.Session.WorkerID]; exists {
			return problem.Invalid("workers", "contains a duplicate worker_id")
		}
		seen[worker.Session.WorkerID] = struct{}{}
	}
	return nil
}

func cloneWorkerSnapshot(worker WorkerSnapshot) WorkerSnapshot {
	worker.Labels = mapsClone(worker.Labels)
	worker.Cache = cloneHints(worker.Cache)
	return worker
}

func requestFingerprint(request PlacementRequest) (digest.SHA256, error) {
	// CommandID identifies the idempotency record; it is not part of the
	// placement payload. This lets a retry with a new control command recognize
	// an already planned Assignment without allowing a changed target through.
	request.CommandID = ""
	source, err := json.Marshal(request)
	if err != nil {
		return "", errors.Join(errors.New("marshaling scheduler request"), err)
	}
	return digest.CanonicalJSON("scheduler-decision", "v1", source)
}

func cloneDecision(decision Decision) Decision {
	decision.Candidates = slices.Clone(decision.Candidates)
	for index := range decision.Candidates {
		decision.Candidates[index] = cloneCandidate(decision.Candidates[index])
	}
	return decision
}

func classified(code problem.Code, message string) error {
	return &problem.Error{Code: code, Message: message}
}
