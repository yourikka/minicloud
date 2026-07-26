package controlplane

import (
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/scheduler"
	"github.com/yourikka/minicloud/internal/servingauth"
)

func TestAssignmentStoreInstallsExactCommittedIntent(t *testing.T) {
	t.Parallel()
	store, command := readyAssignmentStore(t)
	record, err := store.Install(command)
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	identity := record.Identity()
	if record.FunctionID != command.FunctionID || record.DesiredState != AssignmentAssigned ||
		record.ResourceRevision != 1 || record.CreatedRaftIndex != command.AppliedIndex ||
		record.LastAppliedIndex != command.AppliedIndex || identity.AssignmentID != command.Placement.AssignmentID ||
		identity.Worker != command.Placement.Worker || identity.Mode != servingauth.ModeNormal {
		t.Fatalf("Install() = %+v, identity = %+v", record, identity)
	}
	replayed, err := store.Install(command)
	if err == nil || replayed != (AssignmentRecord{}) {
		t.Fatalf("exact ID replay = %+v, %v", replayed, err)
	}

	record.DesiredState = AssignmentCancelled
	snapshot := store.Snapshot()
	snapshot[0].Placement.CommandID = "mutated"
	again, err := store.Get(command.Placement.AssignmentID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if again.DesiredState != AssignmentAssigned || again.Placement.CommandID != command.Placement.CommandID {
		t.Fatalf("Store retained caller-owned record: %+v", again)
	}
}

func TestAssignmentStoreRejectsStaleOrConflictingPlacementWithoutMutation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*InstallAssignmentCommand)
		code   problem.Code
	}{
		{
			name: "stale scaling revision",
			mutate: func(command *InstallAssignmentCommand) {
				command.ExpectedScalingRevision++
			},
			code: problem.CodeRevisionConflict,
		},
		{
			name: "missing function",
			mutate: func(command *InstallAssignmentCommand) {
				command.FunctionID = "function-missing"
			},
			code: problem.CodeNotFound,
		},
		{
			name: "stale policy",
			mutate: func(command *InstallAssignmentCommand) {
				command.Placement.PolicyDigest = digest.Sum([]byte("other-policy"))
			},
			code: problem.CodeStaleGeneration,
		},
		{
			name: "runtime mismatch",
			mutate: func(command *InstallAssignmentCommand) {
				command.Placement.ArtifactDigest = digest.Sum([]byte("other-artifact"))
			},
			code: problem.CodeConflict,
		},
		{
			name: "precedes route",
			mutate: func(command *InstallAssignmentCommand) {
				command.AppliedIndex = 5
			},
			code: problem.CodeInvalidArgument,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store, command := readyAssignmentStore(t)
			test.mutate(&command)
			_, err := store.Install(command)
			assertAssignmentProblemCode(t, err, test.code)
			if records := store.Snapshot(); len(records) != 0 {
				t.Fatalf("failed Install() mutated records: %+v", records)
			}
		})
	}
}

func TestAssignmentStoreCancelsWithCASAndNeverReusesIdentity(t *testing.T) {
	t.Parallel()
	store, install := readyAssignmentStore(t)
	created, err := store.Install(install)
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	cancel := CancelAssignmentCommand{
		AssignmentID: created.Placement.AssignmentID, ExpectedResourceRevision: 1,
		AppliedIndex: 7, UpdatedAt: install.UpdatedAt.Add(time.Minute),
	}
	cancelled, err := store.Cancel(cancel)
	if err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	if cancelled.DesiredState != AssignmentCancelled || cancelled.ResourceRevision != 2 ||
		cancelled.LastAppliedIndex != cancel.AppliedIndex {
		t.Fatalf("Cancel() = %+v", cancelled)
	}
	if _, err := store.Cancel(cancel); err == nil {
		t.Fatal("Cancel() accepted stale CAS replay")
	}
	install.Placement.CommandID = "command-reuse"
	install.AppliedIndex = 8
	install.UpdatedAt = cancel.UpdatedAt.Add(time.Minute)
	if _, err := store.Install(install); err == nil {
		t.Fatal("Install() reused a cancelled Assignment ID")
	}
}

func TestAssignmentStoreConcurrentInstallDoesNotExceedDesiredReplicas(t *testing.T) {
	t.Parallel()
	store, base := readyAssignmentStore(t)
	const callers = 64
	errorsByCall := make(chan error, callers)
	var wait sync.WaitGroup
	for index := range callers {
		wait.Go(func() {
			command := base
			suffix := strconv.Itoa(index + 1)
			command.Placement.AssignmentID = "assignment-concurrent-" + suffix
			command.Placement.CommandID = "command-concurrent-" + suffix
			command.AppliedIndex = uint64(index) + 7
			command.UpdatedAt = base.UpdatedAt.Add(time.Duration(index+1) * time.Second)
			_, err := store.Install(command)
			errorsByCall <- err
		})
	}
	wait.Wait()
	close(errorsByCall)
	var succeeded, conflicted int
	for err := range errorsByCall {
		if err == nil {
			succeeded++
			continue
		}
		var classified *problem.Error
		if errors.As(err, &classified) && classified.Code == problem.CodeConflict {
			conflicted++
			continue
		}
		t.Fatalf("concurrent Install() error = %v", err)
	}
	if succeeded != 1 || conflicted != callers-1 {
		t.Fatalf("concurrent Install() succeeded=%d conflicted=%d", succeeded, conflicted)
	}
	if records := store.Snapshot(); len(records) != 1 {
		t.Fatalf("Snapshot() has %d records, want 1", len(records))
	}
}

func TestAssignmentStoreEnforcesBoundedNonReusableIDs(t *testing.T) {
	t.Parallel()
	store, command := readyAssignmentStore(t)
	store.maxAssignments = 1
	created, err := store.Install(command)
	if err != nil {
		t.Fatalf("first Install() error = %v", err)
	}
	if _, err := store.Cancel(CancelAssignmentCommand{
		AssignmentID: created.Placement.AssignmentID, ExpectedResourceRevision: 1,
		AppliedIndex: 7, UpdatedAt: command.UpdatedAt.Add(time.Minute),
	}); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	command.Placement.AssignmentID = "assignment-02"
	command.Placement.CommandID = "command-02"
	command.AppliedIndex = 8
	command.UpdatedAt = command.UpdatedAt.Add(2 * time.Minute)
	_, err = store.Install(command)
	assertAssignmentProblemCode(t, err, problem.CodeOverloaded)
}

func readyAssignmentStore(t *testing.T) (*AssignmentStore, InstallAssignmentCommand) {
	t.Helper()
	_, _, routes, version, deployment := readyRouteStore(t)
	routeAt := deployment.UpdatedAt.Add(time.Minute)
	route := validRoute(version, deployment, 1, 5, routeAt)
	if _, _, err := routes.Publish(PublishRouteCommand{
		FunctionID: "function-01", ExpectedActiveRouteRevision: 0,
		Route: route, UpdatedAt: routeAt, AppliedIndex: 5,
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	store, err := NewAssignmentStore(routes)
	if err != nil {
		t.Fatalf("NewAssignmentStore() error = %v", err)
	}
	return store, InstallAssignmentCommand{
		FunctionID: "function-01",
		Placement: scheduler.Assignment{
			CommandID: "command-01", AssignmentID: "assignment-01",
			Worker:    servingauth.WorkerSession{WorkerID: "worker-01", BootID: "boot-01", SessionEpoch: 1},
			VersionID: version.VersionID, ArtifactDigest: version.ArtifactDigest,
			ArtifactSize: version.ArtifactSize, ABI: version.ABI, HostAPI: version.HostAPIProfile,
			FeatureProfile: version.RuntimeFeatureProfile, MemoryMiB: deployment.ResourceLimits.MemoryMiB,
			RequiredSlots: 1, AdmissionEpoch: version.AdmissionEpoch,
			DeploymentGeneration: deployment.Generation, PolicyDigest: deployment.EffectivePolicyDigest,
		},
		IfNoneMatch:             true,
		ExpectedScalingRevision: deployment.ScalingRevision,
		AppliedIndex:            6,
		UpdatedAt:               routeAt.Add(time.Minute),
	}
}

func assertAssignmentProblemCode(t *testing.T, err error, want problem.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want code %q", want)
	}
	var classified *problem.Error
	if !errors.As(err, &classified) {
		t.Fatalf("error type = %T, want *problem.Error: %v", err, err)
	}
	if classified.Code != want {
		t.Fatalf("error code = %q, want %q: %v", classified.Code, want, err)
	}
}
