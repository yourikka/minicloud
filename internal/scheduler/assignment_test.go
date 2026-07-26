package scheduler

import (
	"testing"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/servingauth"
	"github.com/yourikka/minicloud/internal/wasmprofile"
)

func TestAssignmentValidateRejectsIncompletePlacementResults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*Assignment)
	}{
		{name: "invalid command", mutate: func(a *Assignment) { a.CommandID = "" }},
		{name: "invalid identity", mutate: func(a *Assignment) { a.AssignmentID = "" }},
		{name: "invalid artifact", mutate: func(a *Assignment) { a.ArtifactDigest = "" }},
		{name: "invalid artifact size", mutate: func(a *Assignment) { a.ArtifactSize = 0 }},
		{name: "invalid abi", mutate: func(a *Assignment) { a.ABI = "component-v1" }},
		{name: "invalid host api", mutate: func(a *Assignment) { a.HostAPI = "network" }},
		{name: "invalid feature profile", mutate: func(a *Assignment) { a.FeatureProfile = "other" }},
		{name: "invalid memory tier", mutate: func(a *Assignment) { a.MemoryMiB = 65 }},
		{name: "zero slots", mutate: func(a *Assignment) { a.RequiredSlots = 0 }},
		{name: "excess slots", mutate: func(a *Assignment) { a.RequiredSlots = MaxWorkerSlots + 1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assignment := validAssignmentResult()
			test.mutate(&assignment)
			if err := assignment.Validate(); err == nil {
				t.Fatalf("Assignment.Validate() accepted %+v", assignment)
			}
		})
	}
}

func TestAssignmentValidateAcceptsPlannerResult(t *testing.T) {
	t.Parallel()
	if err := validAssignmentResult().Validate(); err != nil {
		t.Fatalf("Assignment.Validate() error = %v", err)
	}
}

func validAssignmentResult() Assignment {
	return Assignment{
		CommandID: "command-a", AssignmentID: "assignment-a",
		Worker:    servingauth.WorkerSession{WorkerID: "worker-a", BootID: "boot-a", SessionEpoch: 1},
		VersionID: "version-a", ArtifactDigest: digest.Sum([]byte("artifact-a")), ArtifactSize: 1024,
		ABI: model.ABIWASICommandV1, HostAPI: model.HostAPIProfileNone,
		FeatureProfile: wasmprofile.FeatureProfile, MemoryMiB: 64, RequiredSlots: 1,
		AdmissionEpoch: 1, DeploymentGeneration: 1, PolicyDigest: digest.Sum([]byte("policy-a")),
	}
}
