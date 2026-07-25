package discovery

import (
	"errors"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/scheduler"
	"github.com/yourikka/minicloud/internal/servingauth"
	"github.com/yourikka/minicloud/internal/wasmprofile"
)

func TestBuildProducesCanonicalDefensiveSnapshot(t *testing.T) {
	t.Parallel()
	builder := newTestBuilder(t, Config{})
	input := validInput()
	second := candidateFor("assignment-b", "worker-b", "boot-b", "version-b", 2, digest.Sum([]byte("policy-b")))
	input.Route.Targets = append(input.Route.Targets, model.RouteTarget{
		VersionID:             "version-b",
		AdmissionEpoch:        1,
		DeploymentGeneration:  2,
		EffectivePolicyDigest: digest.Sum([]byte("policy-b")),
		WeightBasisPoints:     4_000,
	})
	input.Route.Targets[0].WeightBasisPoints = 6_000
	input.Candidates = append([]EndpointCandidate{second}, input.Candidates...)

	first, err := builder.Build(input)
	if err != nil {
		t.Fatalf("Build(first) error = %v", err)
	}
	if err := first.Snapshot.Validate(); err != nil {
		t.Fatalf("Snapshot.Validate() error = %v", err)
	}
	if got := endpointIDs(first.Snapshot.Endpoints); !slices.Equal(got, []string{"assignment-a", "assignment-b"}) {
		t.Fatalf("endpoint order = %v", got)
	}
	if got := decisionIDs(first.Candidates); !slices.Equal(got, []string{"assignment-a", "assignment-b"}) {
		t.Fatalf("decision order = %v", got)
	}

	shuffled := validInput()
	shuffled.Route.Targets = slices.Clone(input.Route.Targets)
	slices.Reverse(shuffled.Route.Targets)
	shuffled.Candidates = slices.Clone(input.Candidates)
	slices.Reverse(shuffled.Candidates)
	secondResult, err := builder.Build(shuffled)
	if err != nil {
		t.Fatalf("Build(shuffled) error = %v", err)
	}
	if first.Snapshot.Checksum != secondResult.Snapshot.Checksum {
		t.Fatalf("checksum changed with input order: %q != %q", first.Snapshot.Checksum, secondResult.Snapshot.Checksum)
	}
	if !reflect.DeepEqual(first.Candidates, secondResult.Candidates) {
		t.Fatalf("candidate decisions changed with input order: %+v != %+v", first.Candidates, secondResult.Candidates)
	}
	const checksumVector = "sha256:8a44f7f1efedabbcdc59ef7a4406827a8971eb308467b9b6fefffb6cd2d43452"
	if first.Snapshot.Checksum.String() != checksumVector {
		t.Fatalf("checksum = %q, want %q", first.Snapshot.Checksum, checksumVector)
	}

	originalChecksum := first.Snapshot.Checksum
	*input.Trigger.TokenVerifierDigest = digest.Sum([]byte("mutated-verifier"))
	input.Route.Salt[0] ^= 0xff
	input.Route.Targets[0].EffectivePolicyDigest = digest.Sum([]byte("mutated-policy"))
	input.Candidates[0].Worker.Labels["zone"] = "mutated"
	if first.Snapshot.Checksum != originalChecksum || first.Snapshot.Route.Salt[0] == input.Route.Salt[0] {
		t.Fatal("Build retained caller-owned memory")
	}

	clone := first.Snapshot.Clone()
	*clone.Trigger.TokenVerifierDigest = digest.Sum([]byte("clone-verifier"))
	clone.Route.Salt[0] ^= 0xff
	clone.Route.Targets[0].WeightBasisPoints++
	clone.Endpoints[0].Address = "changed.example:9999"
	if err := first.Snapshot.Validate(); err != nil {
		t.Fatalf("mutating Clone affected original: %v", err)
	}
	if first.Snapshot.Checksum != originalChecksum {
		t.Fatal("mutating Clone changed original checksum")
	}
}

func TestSnapshotValidateRejectsChecksumAndFenceTampering(t *testing.T) {
	t.Parallel()
	builder := newTestBuilder(t, Config{})
	result, err := builder.Build(validInput())
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*Snapshot)
	}{
		{name: "checksum", mutate: func(s *Snapshot) { s.Checksum = digest.Sum([]byte("wrong")) }},
		{name: "epoch", mutate: func(s *Snapshot) { s.DiscoveryEpoch++ }},
		{name: "sequence", mutate: func(s *Snapshot) { s.ServingSequence++ }},
		{name: "generated at", mutate: func(s *Snapshot) { s.GeneratedAt = s.GeneratedAt.Add(time.Second) }},
		{name: "function revision", mutate: func(s *Snapshot) { s.Function.ResourceRevision++ }},
		{name: "trigger revision", mutate: func(s *Snapshot) { s.Trigger.ResourceRevision++ }},
		{name: "verifier", mutate: func(s *Snapshot) { *s.Trigger.TokenVerifierDigest = digest.Sum([]byte("other-verifier")) }},
		{name: "route revision", mutate: func(s *Snapshot) { s.Route.RouteRevision++ }},
		{name: "salt", mutate: func(s *Snapshot) { s.Route.Salt[0] ^= 0xff }},
		{name: "target policy", mutate: func(s *Snapshot) { s.Route.Targets[0].EffectivePolicyDigest = digest.Sum([]byte("other-target")) }},
		{name: "assignment id", mutate: func(s *Snapshot) { s.Endpoints[0].Assignment.AssignmentID = "assignment-z" }},
		{name: "version id", mutate: func(s *Snapshot) { s.Endpoints[0].Assignment.VersionID = "version-z" }},
		{name: "admission epoch", mutate: func(s *Snapshot) { s.Endpoints[0].Assignment.AdmissionEpoch++ }},
		{name: "generation", mutate: func(s *Snapshot) { s.Endpoints[0].Assignment.DeploymentGeneration++ }},
		{name: "endpoint policy", mutate: func(s *Snapshot) { s.Endpoints[0].Assignment.PolicyDigest = digest.Sum([]byte("other-policy")) }},
		{name: "mode", mutate: func(s *Snapshot) { s.Endpoints[0].Assignment.Mode = servingauth.ModeDrainOnly }},
		{name: "worker id", mutate: func(s *Snapshot) { s.Endpoints[0].Assignment.Worker.WorkerID = "worker-z" }},
		{name: "boot id", mutate: func(s *Snapshot) { s.Endpoints[0].Assignment.Worker.BootID = "boot-z" }},
		{name: "session epoch", mutate: func(s *Snapshot) { s.Endpoints[0].Assignment.Worker.SessionEpoch++ }},
		{name: "address", mutate: func(s *Snapshot) { s.Endpoints[0].Address = "worker-z.internal:7443" }},
		{name: "state", mutate: func(s *Snapshot) { s.Endpoints[0].State = "Lost" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			tampered := result.Snapshot.Clone()
			test.mutate(&tampered)
			if err := tampered.Validate(); err == nil {
				t.Fatal("Validate() accepted a tampered snapshot")
			}
		})
	}
}

func TestBuildFiltersEveryServingBoundary(t *testing.T) {
	t.Parallel()
	builder := newTestBuilder(t, Config{})
	input := validInput()
	input.Candidates = nil

	type testCase struct {
		id     string
		reason ExclusionReason
		mutate func(*EndpointCandidate)
	}
	tests := []testCase{
		{id: "assignment-cancelled", reason: ReasonAssignmentNotAssigned, mutate: func(c *EndpointCandidate) { c.DesiredState = AssignmentCancelled }},
		{id: "authorization-missing", reason: ReasonAuthorizationNotInstalled, mutate: func(c *EndpointCandidate) { c.Authorization = nil }},
		{id: "authorization-epoch", reason: ReasonAuthorizationMismatch, mutate: func(c *EndpointCandidate) { c.Authorization.Fence.DiscoveryEpoch++ }},
		{id: "authorization-fence", reason: ReasonAuthorizationMismatch, mutate: func(c *EndpointCandidate) { c.Authorization.Fence.Assignment.Worker.BootID = "other-boot" }},
		{id: "mode-drain", reason: ReasonAssignmentModeNotNormal, mutate: func(c *EndpointCandidate) { c.Assignment.Mode = servingauth.ModeDrainOnly }},
		{id: "worker-id", reason: ReasonWorkerSessionMismatch, mutate: func(c *EndpointCandidate) { c.Assignment.Worker.WorkerID = "worker-other" }},
		{id: "boot-id", reason: ReasonWorkerSessionMismatch, mutate: func(c *EndpointCandidate) { c.Assignment.Worker.BootID = "boot-other" }},
		{id: "session-epoch", reason: ReasonWorkerSessionMismatch, mutate: func(c *EndpointCandidate) { c.Assignment.Worker.SessionEpoch++ }},
		{id: "worker-intent", reason: ReasonWorkerNotSchedulable, mutate: func(c *EndpointCandidate) { c.Worker.Intent = scheduler.IntentDraining }},
		{id: "worker-state", reason: ReasonWorkerNotReady, mutate: func(c *EndpointCandidate) { c.Worker.State = scheduler.SessionSuspect }},
		{id: "worker-drain", reason: ReasonWorkerDraining, mutate: func(c *EndpointCandidate) { c.Worker.Drain = scheduler.DrainDraining }},
		{id: "replica-state", reason: ReasonReplicaNotReady, mutate: func(c *EndpointCandidate) { c.ReplicaReady = false }},
		{id: "version", reason: ReasonRouteTargetMissing, mutate: func(c *EndpointCandidate) { c.Assignment.VersionID = "version-other" }},
		{id: "admission", reason: ReasonAdmissionEpochMismatch, mutate: func(c *EndpointCandidate) { c.Assignment.AdmissionEpoch++ }},
		{id: "generation", reason: ReasonDeploymentGenerationMismatch, mutate: func(c *EndpointCandidate) { c.Assignment.DeploymentGeneration++ }},
		{id: "policy", reason: ReasonPolicyDigestMismatch, mutate: func(c *EndpointCandidate) { c.Assignment.PolicyDigest = digest.Sum([]byte("other-policy")) }},
		{id: "address", reason: ReasonInvalidAddress, mutate: func(c *EndpointCandidate) { c.Address = "https://worker.invalid" }},
		{id: "worker-observation", reason: ReasonInvalidWorkerObservation, mutate: func(c *EndpointCandidate) { c.Worker.Runtime.Name = "other-runtime" }},
		{id: "assignment", reason: ReasonInvalidAssignment, mutate: func(c *EndpointCandidate) { c.Assignment.Worker.BootID = "" }},
	}
	valid := candidateFor("assignment-valid", "worker-valid", "boot-valid", "version-a", 1, input.Route.Targets[0].EffectivePolicyDigest)
	input.Candidates = append(input.Candidates, valid)
	for _, test := range tests {
		candidate := candidateFor(test.id, "worker-"+test.id, "boot-"+test.id, "version-a", 1, input.Route.Targets[0].EffectivePolicyDigest)
		test.mutate(&candidate)
		input.Candidates = append(input.Candidates, candidate)
	}

	result, err := builder.Build(input)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if got := endpointIDs(result.Snapshot.Endpoints); !slices.Equal(got, []string{"assignment-valid"}) {
		t.Fatalf("included endpoints = %v", got)
	}
	decisions := make(map[string]CandidateDecision, len(result.Candidates))
	for _, decision := range result.Candidates {
		decisions[decision.AssignmentID] = decision
	}
	for _, test := range tests {
		decision := decisions[test.id]
		if decision.Included || !slices.Contains(decision.Reasons, test.reason) {
			t.Errorf("decision %q = %+v, want exclusion %q", test.id, decision, test.reason)
		}
	}
}

func TestBuildDisablesEndpointsWithCompleteView(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		reason ExclusionReason
		mutate func(*Input)
	}{
		{name: "function", reason: ReasonFunctionNotActive, mutate: func(input *Input) { input.Function.Lifecycle = model.FunctionDisabled }},
		{name: "trigger", reason: ReasonTriggerDisabled, mutate: func(input *Input) { input.Trigger.Enabled = false }},
		{name: "route", reason: ReasonRouteDisabled, mutate: func(input *Input) { input.Route.Enabled = false; input.Route.Targets = nil }},
		{name: "no route", reason: ReasonRouteDisabled, mutate: func(input *Input) { input.Route = Route{FunctionID: input.Function.ID} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			input := validInput()
			test.mutate(&input)
			builder := newTestBuilder(t, Config{})
			result, err := builder.Build(input)
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			if len(result.Snapshot.Endpoints) != 0 || result.Candidates[0].Included ||
				!slices.Contains(result.Candidates[0].Reasons, test.reason) {
				t.Fatalf("result = %+v, want exclusion %q", result, test.reason)
			}
			if err := result.Snapshot.Validate(); err != nil {
				t.Fatalf("disabled Snapshot.Validate() error = %v", err)
			}
		})
	}
}

func TestBuilderBoundsAndInputValidation(t *testing.T) {
	t.Parallel()
	for _, maxEndpoints := range []int{-1, HardMaxEndpoints + 1} {
		if _, err := New(Config{MaxEndpoints: maxEndpoints}); err == nil {
			t.Fatalf("New(MaxEndpoints=%d) succeeded", maxEndpoints)
		}
	}
	builder := newTestBuilder(t, Config{MaxEndpoints: 1})
	input := validInput()
	input.Candidates = append(input.Candidates,
		candidateFor("assignment-b", "worker-b", "boot-b", "version-a", 1, input.Route.Targets[0].EffectivePolicyDigest))
	_, err := builder.Build(input)
	var classified *problem.Error
	if !errors.As(err, &classified) || classified.Code != problem.CodeOverloaded {
		t.Fatalf("Build() error = %v, want overloaded", err)
	}

	input = validInput()
	input.Trigger.TokenVerifierDigest = nil
	if _, err := newTestBuilder(t, Config{}).Build(input); err == nil {
		t.Fatal("Build() accepted token auth without a verifier")
	}
}

func TestPublisherOwnsOneGlobalSequence(t *testing.T) {
	t.Parallel()
	builder := newTestBuilder(t, Config{})
	publisher, err := NewPublisher(900, builder)
	if err != nil {
		t.Fatalf("NewPublisher() error = %v", err)
	}
	const calls = 100
	sequences := make(chan uint64, calls)
	var wait sync.WaitGroup
	for range calls {
		wait.Go(func() {
			input := validInput()
			setAuthorizationEpoch(&input, 900)
			result, err := publisher.Build(input)
			if err != nil {
				t.Errorf("Build() error = %v", err)
				return
			}
			if result.Snapshot.DiscoveryEpoch != 900 {
				t.Errorf("epoch = %d, want 900", result.Snapshot.DiscoveryEpoch)
			}
			if len(result.Snapshot.Endpoints) != 1 {
				t.Errorf("published endpoint count = %d, want one", len(result.Snapshot.Endpoints))
			}
			sequences <- result.Snapshot.ServingSequence
		})
	}
	wait.Wait()
	close(sequences)
	seen := make(map[uint64]struct{}, calls)
	for sequence := range sequences {
		seen[sequence] = struct{}{}
	}
	for sequence := uint64(1); sequence <= calls; sequence++ {
		if _, exists := seen[sequence]; !exists {
			t.Fatalf("global sequence %d was not published", sequence)
		}
	}
	epoch, sequence := publisher.Position()
	if epoch != 900 || sequence != calls {
		t.Fatalf("Position() = (%d,%d), want (900,%d)", epoch, sequence, calls)
	}
}

func TestPublisherBuildFullUsesOneGlobalSequence(t *testing.T) {
	t.Parallel()
	publisher, err := NewPublisher(901, newTestBuilder(t, Config{}))
	if err != nil {
		t.Fatalf("NewPublisher() error = %v", err)
	}
	first := validInput()
	second := validInput()
	setAuthorizationEpoch(&first, 901)
	setAuthorizationEpoch(&second, 901)
	second.Function.ID = "function-b"
	second.Function.Name = "function-b"
	second.Trigger.ID = "trigger-b"
	second.Trigger.FunctionID = "function-b"
	second.Route.FunctionID = "function-b"
	results, sequence, err := publisher.BuildFull([]Input{first, second})
	if err != nil {
		t.Fatalf("BuildFull() error = %v", err)
	}
	if sequence != 1 || len(results) != 2 {
		t.Fatalf("BuildFull() = (%d results, sequence %d)", len(results), sequence)
	}
	for _, result := range results {
		if result.Snapshot.DiscoveryEpoch != 901 || result.Snapshot.ServingSequence != 1 {
			t.Fatalf("Full Sync snapshot position = (%d,%d)", result.Snapshot.DiscoveryEpoch, result.Snapshot.ServingSequence)
		}
	}
	empty, emptySequence, err := publisher.BuildFull(nil)
	if err != nil {
		t.Fatalf("empty BuildFull() error = %v", err)
	}
	if empty == nil || len(empty) != 0 || emptySequence != 2 {
		t.Fatalf("empty BuildFull() = (%v,%d), want initialized empty slice and sequence 2", empty, emptySequence)
	}
}

func TestPublisherBuildFullRejectsDuplicateFunction(t *testing.T) {
	t.Parallel()
	publisher, err := NewPublisher(901, newTestBuilder(t, Config{}))
	if err != nil {
		t.Fatalf("NewPublisher() error = %v", err)
	}
	input := validInput()
	setAuthorizationEpoch(&input, 901)
	if _, _, err := publisher.BuildFull([]Input{input, input}); err == nil {
		t.Fatal("BuildFull() accepted duplicate Function input")
	}
	if _, sequence := publisher.Position(); sequence != 0 {
		t.Fatalf("failed BuildFull() advanced sequence to %d", sequence)
	}
}

func setAuthorizationEpoch(input *Input, epoch uint64) {
	for index := range input.Candidates {
		authorization := *input.Candidates[index].Authorization
		authorization.Fence.DiscoveryEpoch = epoch
		input.Candidates[index].Authorization = &authorization
	}
}

func validInput() Input {
	policy := digest.Sum([]byte("policy-a"))
	verifier := digest.Sum([]byte("verifier-a"))
	return Input{
		DiscoveryEpoch:  101,
		ServingSequence: 7,
		GeneratedAt:     time.Date(2026, time.July, 25, 12, 34, 56, 123456789, time.UTC),
		Function: Function{
			ID: "function-a", Name: "function-a", ResourceRevision: 3, Lifecycle: model.FunctionActive,
		},
		Trigger: HTTPTrigger{
			ID: "trigger-a", FunctionID: "function-a", ResourceRevision: 4,
			Enabled: true, AuthPolicy: AuthToken, TokenVerifierDigest: &verifier,
		},
		Route: Route{
			Present: true, FunctionID: "function-a", ResourceRevision: 5, RouteRevision: 6, Enabled: true,
			Targets: []model.RouteTarget{{
				VersionID: "version-a", AdmissionEpoch: 1, DeploymentGeneration: 1,
				EffectivePolicyDigest: policy, WeightBasisPoints: model.TotalRouteWeightBasisPoints,
			}},
			Affinity: model.AffinityRequestID, HashVersion: model.HashVersionSHA256BPSV1,
			SaltID: "salt-a", Salt: []byte("0123456789abcdef"),
		},
		Candidates: []EndpointCandidate{
			candidateFor("assignment-a", "worker-a", "boot-a", "version-a", 1, policy),
		},
	}
}

func candidateFor(
	assignmentID, workerID, bootID, versionID string,
	generation uint64,
	policy digest.SHA256,
) EndpointCandidate {
	session := servingauth.WorkerSession{WorkerID: workerID, BootID: bootID, SessionEpoch: 1}
	return EndpointCandidate{
		Assignment: servingauth.AssignmentIdentity{
			Worker: session, AssignmentID: assignmentID, VersionID: versionID,
			AdmissionEpoch: 1, DeploymentGeneration: generation, PolicyDigest: policy,
			Mode: servingauth.ModeNormal,
		},
		DesiredState: AssignmentAssigned,
		Worker: scheduler.WorkerSnapshot{
			Session: session,
			Runtime: scheduler.RuntimeProfile{
				Name: wasmprofile.RuntimeName, Version: wasmprofile.RuntimeVersion,
				Engine: wasmprofile.EngineCompiler, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
				ABI: model.ABIWASICommandV1, HostAPI: model.HostAPIProfileNone,
				FeatureProfile: wasmprofile.FeatureProfile, MemoryMiB: 128,
			},
			Intent: scheduler.IntentSchedulable, State: scheduler.SessionReady,
			Drain:    scheduler.DrainNotDraining,
			Capacity: scheduler.Capacity{MemoryMiB: 512, Slots: 8},
			Labels:   map[string]string{"zone": "test"},
		},
		ReplicaReady: true,
		Authorization: &servingauth.Authorization{
			Fence: servingauth.InvocationFence{
				Assignment: servingauth.AssignmentIdentity{
					Worker: session, AssignmentID: assignmentID, VersionID: versionID,
					AdmissionEpoch: 1, DeploymentGeneration: generation, PolicyDigest: policy,
					Mode: servingauth.ModeNormal,
				},
				DiscoveryEpoch: 101,
			},
			Lifetime: servingauth.LifetimeTTL,
			TTL:      time.Minute,
		},
		Address: workerID + ".internal:7443",
	}
}

func newTestBuilder(t *testing.T, config Config) *Builder {
	t.Helper()
	builder, err := New(config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return builder
}

func endpointIDs(endpoints []Endpoint) []string {
	ids := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		ids = append(ids, endpoint.Assignment.AssignmentID)
	}
	return ids
}

func decisionIDs(decisions []CandidateDecision) []string {
	ids := make([]string, 0, len(decisions))
	for _, decision := range decisions {
		ids = append(ids, decision.AssignmentID)
	}
	return ids
}
