package routing_test

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/routing"
)

func TestSelectPublishesStableBPSVectors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		functionID string
		revision   uint64
		salt       []byte
		affinity   []byte
		targets    []model.RouteTarget
		digest     digest.SHA256
		bucket     uint32
		selected   string
	}{
		{
			name:       "single target",
			functionID: "fn_01",
			revision:   1,
			salt:       []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f},
			affinity:   []byte("request-123"),
			targets: []model.RouteTarget{{
				VersionID:             "ver_01",
				AdmissionEpoch:        1,
				DeploymentGeneration:  1,
				EffectivePolicyDigest: digest.Sum([]byte("policy-1")),
				WeightBasisPoints:     model.TotalRouteWeightBasisPoints,
			}},
			digest:   "sha256:4cfbeb9d996250b898acc908d8d2a2ae155072414244771547c803d78a2586b6",
			bucket:   3007,
			selected: "ver_01/1",
		},
		{
			name:       "sorted targets and raw affinity",
			functionID: "fn_payments",
			revision:   42,
			salt:       []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff},
			affinity:   []byte{0x63, 0x75, 0x73, 0x74, 0x6f, 0x6d, 0x65, 0x72, 0x3a, 0x00, 0xff},
			targets: []model.RouteTarget{
				{VersionID: "ver_b", AdmissionEpoch: 1, DeploymentGeneration: 2, EffectivePolicyDigest: digest.Sum([]byte("policy-b")), WeightBasisPoints: 2500},
				{VersionID: "ver_a", AdmissionEpoch: 1, DeploymentGeneration: 3, EffectivePolicyDigest: digest.Sum([]byte("policy-a3")), WeightBasisPoints: 3000},
				{VersionID: "ver_a", AdmissionEpoch: 1, DeploymentGeneration: 1, EffectivePolicyDigest: digest.Sum([]byte("policy-a1")), WeightBasisPoints: 4500},
			},
			digest:   "sha256:c8f2c66d28ad9fd37d92fe0af3317b023852c580c3780d1b8eba7b86e4f2b942",
			bucket:   7849,
			selected: "ver_b/2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			route := testRoute(tt.functionID, tt.revision, tt.salt, tt.targets)
			before := append([]model.RouteTarget(nil), route.Targets...)
			for range 100 {
				decision, err := routing.Select(route, tt.affinity)
				if err != nil {
					t.Fatalf("Select() error = %v", err)
				}
				if decision.Digest != tt.digest {
					t.Fatalf("Digest = %q, want %q", decision.Digest, tt.digest)
				}
				if decision.Bucket != tt.bucket {
					t.Fatalf("Bucket = %d, want %d", decision.Bucket, tt.bucket)
				}
				selected := decision.Target.VersionID + "/" + strconv.FormatUint(decision.Target.DeploymentGeneration, 10)
				if selected != tt.selected {
					t.Fatalf("Target = %q, want %q", selected, tt.selected)
				}
			}
			if !slices.Equal(route.Targets, before) {
				t.Fatal("Select() mutated the caller's target order")
			}
		})
	}
}

func TestSelectTracksTenPercentWeightAcrossAffinityKeys(t *testing.T) {
	t.Parallel()
	route := testRoute("fn_distribution", 9, []byte("0123456789abcdef"), []model.RouteTarget{
		{VersionID: "ver_stable", AdmissionEpoch: 1, DeploymentGeneration: 1, EffectivePolicyDigest: digest.Sum([]byte("stable")), WeightBasisPoints: 9000},
		{VersionID: "ver_canary", AdmissionEpoch: 1, DeploymentGeneration: 2, EffectivePolicyDigest: digest.Sum([]byte("canary")), WeightBasisPoints: 1000},
	})
	canary := 0
	for index := range 10_000 {
		decision, err := routing.Select(route, []byte(fmt.Sprintf("affinity-%05d", index)))
		if err != nil {
			t.Fatalf("Select(%d) error = %v", index, err)
		}
		if decision.Target.VersionID == "ver_canary" {
			canary++
		}
	}
	if canary < 850 || canary > 1150 {
		t.Fatalf("canary selections = %d/10000, want 10%% +/- 1.5 percentage points", canary)
	}
}

func TestSelectUsesLengthPrefixesAndRouteRevision(t *testing.T) {
	t.Parallel()
	baseTargets := []model.RouteTarget{{
		VersionID:             "ver_01",
		AdmissionEpoch:        1,
		DeploymentGeneration:  1,
		EffectivePolicyDigest: digest.Sum([]byte("policy")),
		WeightBasisPoints:     model.TotalRouteWeightBasisPoints,
	}}
	left := testRoute("a", 7, make([]byte, 16), baseTargets)
	right := testRoute("ab", 7, make([]byte, 16), baseTargets)
	leftDecision, err := routing.Select(left, []byte("bc"))
	if err != nil {
		t.Fatalf("Select(left) error = %v", err)
	}
	rightDecision, err := routing.Select(right, []byte("c"))
	if err != nil {
		t.Fatalf("Select(right) error = %v", err)
	}
	if leftDecision.Digest == rightDecision.Digest {
		t.Fatal("length-prefixed fields produced a collision for concatenation variants")
	}

	revision := right
	revision.RouteRevision++
	revisionDecision, err := routing.Select(revision, []byte("c"))
	if err != nil {
		t.Fatalf("Select(revision) error = %v", err)
	}
	if revisionDecision.Digest == rightDecision.Digest {
		t.Fatal("RouteRevision did not affect the routing digest")
	}
}

func TestSelectRejectsDisabledRoute(t *testing.T) {
	t.Parallel()
	route := testRoute("fn_disabled", 1, make([]byte, 16), []model.RouteTarget{{
		VersionID:             "ver_01",
		AdmissionEpoch:        1,
		DeploymentGeneration:  1,
		EffectivePolicyDigest: digest.Sum([]byte("policy")),
		WeightBasisPoints:     model.TotalRouteWeightBasisPoints,
	}})
	route.Enabled = false
	route.Targets = nil
	_, err := routing.Select(route, []byte("request"))
	var classified *problem.Error
	if !errors.As(err, &classified) || classified.Code != problem.CodeFunctionDisabled {
		t.Fatalf("Select() error = %v, want function_disabled", err)
	}
}

func testRoute(functionID string, revision uint64, salt []byte, targets []model.RouteTarget) model.Route {
	return model.Route{
		Metadata: model.Metadata{
			ID:               "route_01",
			Namespace:        model.DefaultNamespace,
			CreatedAt:        time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
			UpdatedAt:        time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
			CreatedRaftIndex: 1,
			ResourceRevision: 1,
		},
		FunctionID:    functionID,
		RouteRevision: revision,
		Targets:       targets,
		Affinity:      model.AffinityRequestID,
		HashVersion:   model.HashVersionSHA256BPSV1,
		SaltID:        "salt_01",
		Salt:          salt,
		Enabled:       true,
	}
}
