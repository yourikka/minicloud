package controlplane

import (
	"errors"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
)

func TestRouteStorePublishesReadyTargetAndAdvancesFunctionAtomically(t *testing.T) {
	t.Parallel()
	catalog, _, store, version, deployment := readyRouteStore(t)
	publishedAt := version.UpdatedAt.Add(time.Minute)
	route := validRoute(version, deployment, 1, 5, publishedAt)
	published, function, err := store.Publish(PublishRouteCommand{
		FunctionID: "function-01", ExpectedActiveRouteRevision: 0, Route: route, UpdatedAt: publishedAt, AppliedIndex: 5,
	})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if published.RouteRevision != 1 || function.ActiveRouteRevision != 1 || function.ResourceRevision != 2 {
		t.Fatalf("Publish() = %+v %+v", published, function)
	}
	fromCatalog, _, err := catalog.GetFunction("function-01")
	if err != nil || fromCatalog.ActiveRouteRevision != 1 || fromCatalog.ResourceRevision != 2 {
		t.Fatalf("Catalog GetFunction() = %+v, %v", fromCatalog, err)
	}
	published.Targets[0].WeightBasisPoints = 1
	stored, err := store.Get("function-01")
	if err != nil || stored.Targets[0].WeightBasisPoints != model.TotalRouteWeightBasisPoints {
		t.Fatalf("Get() exposed route storage: %+v, %v", stored, err)
	}
	if routes := store.Snapshot(); len(routes) != 1 || routes[0].FunctionID != "function-01" {
		t.Fatalf("Snapshot() = %+v", routes)
	}
}

func TestRouteStoreRejectsStaleCASAndPolicyMismatchWithoutMutation(t *testing.T) {
	t.Parallel()
	_, _, store, version, deployment := readyRouteStore(t)
	publishedAt := version.UpdatedAt.Add(time.Minute)
	first := validRoute(version, deployment, 1, 5, publishedAt)
	if _, _, err := store.Publish(PublishRouteCommand{FunctionID: "function-01", Route: first, UpdatedAt: publishedAt, AppliedIndex: 5}); err != nil {
		t.Fatalf("Publish(first) error = %v", err)
	}
	stale := validRoute(version, deployment, 1, 6, publishedAt.Add(time.Minute))
	stale.ID = "route-stale"
	_, _, err := store.Publish(PublishRouteCommand{FunctionID: "function-01", Route: stale, UpdatedAt: stale.UpdatedAt, AppliedIndex: 6})
	assertRouteProblemCode(t, err, problem.CodeRevisionConflict, "")
	var conflict *RevisionConflict
	if !errors.As(err, &conflict) || conflict.RevisionKind != "active_route_revision" || conflict.Expected != 0 || conflict.Actual != 1 {
		t.Fatalf("route conflict = %#v", conflict)
	}
	broken := validRoute(version, deployment, 2, 6, publishedAt.Add(time.Minute))
	broken.Targets[0].EffectivePolicyDigest = digest.Sum([]byte("wrong-policy"))
	_, _, err = store.Publish(PublishRouteCommand{FunctionID: "function-01", ExpectedActiveRouteRevision: 1, Route: broken, UpdatedAt: broken.UpdatedAt, AppliedIndex: 6})
	assertRouteProblemCode(t, err, problem.CodeConflict, "")
	current, err := store.Get("function-01")
	if err != nil || current.RouteRevision != 1 {
		t.Fatalf("failed publish changed route: %+v, %v", current, err)
	}
}

func TestRouteStoreAllowsDisabledEmptyRouteForDisabledFunction(t *testing.T) {
	t.Parallel()
	catalog, _, store, version, deployment := readyRouteStore(t)
	publishedAt := version.UpdatedAt.Add(time.Minute)
	first := validRoute(version, deployment, 1, 5, publishedAt)
	if _, _, err := store.Publish(PublishRouteCommand{FunctionID: "function-01", Route: first, UpdatedAt: publishedAt, AppliedIndex: 5}); err != nil {
		t.Fatalf("Publish(first) error = %v", err)
	}
	if _, err := catalog.SetFunctionLifecycle(SetFunctionLifecycleCommand{
		FunctionID: "function-01", ExpectedResourceRevision: 2, Lifecycle: model.FunctionDisabled,
		UpdatedAt: publishedAt.Add(time.Minute), AppliedIndex: 6,
	}); err != nil {
		t.Fatalf("SetFunctionLifecycle() error = %v", err)
	}
	disabledAt := publishedAt.Add(2 * time.Minute)
	disabled := validRoute(version, deployment, 2, 7, disabledAt)
	disabled.ID = "route-disabled"
	disabled.Enabled = false
	disabled.Targets = []model.RouteTarget{}
	published, function, err := store.Publish(PublishRouteCommand{
		FunctionID: "function-01", ExpectedActiveRouteRevision: 1, Route: disabled, UpdatedAt: disabledAt, AppliedIndex: 7,
	})
	if err != nil || published.Enabled || len(published.Targets) != 0 || function.Lifecycle != model.FunctionDisabled || function.ActiveRouteRevision != 2 {
		t.Fatalf("Publish(disabled) = %+v %+v, %v", published, function, err)
	}
}

func TestRouteStoreServingStatesAreConsistentAndDefensive(t *testing.T) {
	t.Parallel()
	catalog, _, store, version, deployment := readyRouteStore(t)

	states, err := store.ServingStates()
	if err != nil {
		t.Fatalf("ServingStates() before route error = %v", err)
	}
	if len(states) != 1 || states[0].Function.ID != "function-01" || states[0].Trigger.FunctionID != "function-01" ||
		states[0].Route != nil || states[0].Version != nil || states[0].Deployment != nil {
		t.Fatalf("ServingStates() before route = %+v", states)
	}

	publishedAt := version.UpdatedAt.Add(time.Minute)
	route := validRoute(version, deployment, 1, 5, publishedAt)
	if _, _, err := store.Publish(PublishRouteCommand{
		FunctionID: "function-01", Route: route, UpdatedAt: publishedAt, AppliedIndex: 5,
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	states, err = store.ServingStates()
	if err != nil {
		t.Fatalf("ServingStates() after route error = %v", err)
	}
	state := states[0]
	if state.Route == nil || state.Version == nil || state.Deployment == nil ||
		state.Function.ActiveRouteRevision != state.Route.RouteRevision ||
		state.Route.Targets[0].VersionID != state.Version.VersionID ||
		state.Route.Targets[0].AdmissionEpoch != state.Version.AdmissionEpoch ||
		state.Route.Targets[0].DeploymentGeneration != state.Deployment.Generation ||
		state.Route.Targets[0].EffectivePolicyDigest != state.Deployment.EffectivePolicyDigest {
		t.Fatalf("ServingStates() = %+v, want one matching serving fence", state)
	}

	state.Function.Labels["mutated"] = "true"
	state.Route.Salt[0] ^= 0xff
	state.Route.Targets[0].WeightBasisPoints = 1
	state.Version.RequestedCapabilities = append(state.Version.RequestedCapabilities, model.CapabilityRequest{})
	reloaded, err := store.ServingStates()
	if err != nil {
		t.Fatalf("ServingStates() reload error = %v", err)
	}
	if reloaded[0].Function.Labels["mutated"] != "" || reloaded[0].Route.Salt[0] == state.Route.Salt[0] ||
		reloaded[0].Route.Targets[0].WeightBasisPoints != model.TotalRouteWeightBasisPoints ||
		len(reloaded[0].Version.RequestedCapabilities) != len(version.RequestedCapabilities) {
		t.Fatalf("ServingStates() exposed store memory: %+v", reloaded[0])
	}

	if _, err := catalog.SetFunctionLifecycle(SetFunctionLifecycleCommand{
		FunctionID: "function-01", ExpectedResourceRevision: 2, Lifecycle: model.FunctionDisabled,
		UpdatedAt: publishedAt.Add(time.Minute), AppliedIndex: 6,
	}); err != nil {
		t.Fatalf("SetFunctionLifecycle() error = %v", err)
	}
	disabledAt := publishedAt.Add(2 * time.Minute)
	disabled := validRoute(version, deployment, 2, 7, disabledAt)
	disabled.ID = "route-disabled"
	disabled.Enabled = false
	disabled.Targets = []model.RouteTarget{}
	if _, _, err := store.Publish(PublishRouteCommand{
		FunctionID: "function-01", ExpectedActiveRouteRevision: 1, Route: disabled, UpdatedAt: disabledAt, AppliedIndex: 7,
	}); err != nil {
		t.Fatalf("Publish(disabled) error = %v", err)
	}
	disabledStates, err := store.ServingStates()
	if err != nil {
		t.Fatalf("ServingStates() disabled route error = %v", err)
	}
	if disabledStates[0].Route == nil || disabledStates[0].Route.Enabled || disabledStates[0].Version != nil || disabledStates[0].Deployment != nil {
		t.Fatalf("ServingStates() disabled route = %+v", disabledStates[0])
	}
}

func readyRouteStore(t *testing.T) (*Catalog, *ReleaseStore, *RouteStore, model.Version, model.Deployment) {
	t.Helper()
	catalog := NewCatalog()
	if _, err := catalog.CreateFunction(validCreateFunction(1, "function-01", "echo")); err != nil {
		t.Fatalf("CreateFunction() error = %v", err)
	}
	releases := NewReleaseStore(catalog)
	version := validUploadedVersion(2, "version-01")
	if _, err := releases.CreateVersion(CreateVersionCommand{IfNoneMatch: true, AppliedIndex: 2, Version: version}); err != nil {
		t.Fatalf("CreateVersion() error = %v", err)
	}
	if _, err := releases.StartValidation(StartValidationCommand{
		VersionID: version.VersionID, ExpectedResourceRevision: 1, ValidationID: "validation-01", UpdatedAt: version.CreatedAt.Add(time.Minute), AppliedIndex: 3,
	}); err != nil {
		t.Fatalf("StartValidation() error = %v", err)
	}
	completedAt := version.CreatedAt.Add(2 * time.Minute)
	deployment := validInitialDeployment(t, version, 4, completedAt)
	if _, _, err := releases.CompleteValidation(CompleteValidationCommand{
		VersionID: version.VersionID, ExpectedResourceRevision: 2, Report: validValidatorReport(version, "validation-01"), Deployment: &deployment,
		UpdatedAt: completedAt, AppliedIndex: 4,
	}); err != nil {
		t.Fatalf("CompleteValidation() error = %v", err)
	}
	return catalog, releases, NewRouteStore(catalog, releases), version, deployment
}

func validRoute(version model.Version, deployment model.Deployment, revision, index uint64, createdAt time.Time) model.Route {
	return model.Route{
		Metadata:   model.Metadata{ID: "route-" + formatIndex(revision), Namespace: model.DefaultNamespace, CreatedAt: createdAt, UpdatedAt: createdAt, CreatedRaftIndex: index, ResourceRevision: 1},
		FunctionID: "function-01", RouteRevision: revision,
		Targets:  []model.RouteTarget{{VersionID: version.VersionID, AdmissionEpoch: version.AdmissionEpoch, DeploymentGeneration: deployment.Generation, EffectivePolicyDigest: deployment.EffectivePolicyDigest, WeightBasisPoints: model.TotalRouteWeightBasisPoints}},
		Affinity: model.AffinityRequestID, HashVersion: model.HashVersionSHA256BPSV1, SaltID: "salt-01", Salt: []byte("0123456789abcdef"), Enabled: true,
	}
}

func assertRouteProblemCode(t *testing.T, err error, wantCode problem.Code, wantField string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want code %q", wantCode)
	}
	var classified *problem.Error
	if !errors.As(err, &classified) {
		t.Fatalf("error type = %T, want *problem.Error: %v", err, err)
	}
	if classified.Code != wantCode || classified.Field != wantField {
		t.Fatalf("error = %+v, want code=%q field=%q", classified, wantCode, wantField)
	}
}
