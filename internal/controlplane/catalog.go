package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
)

const (
	CatalogStateDigestDomain  = "control-catalog-state"
	CatalogStateDigestVersion = "v1"
	DefaultMaxFunctions       = 1_000
)

// AuthPolicy defines the persisted authentication rule of the default HTTP
// Trigger. Token verifier data is deliberately a digest, never a plaintext
// credential.
type AuthPolicy string

const (
	AuthPolicyToken  AuthPolicy = "token"
	AuthPolicyPublic AuthPolicy = "public"
)

// HTTPTrigger is the default invocation Trigger created atomically with a
// Function. Additional Trigger types are outside this catalog slice.
type HTTPTrigger struct {
	model.Metadata
	FunctionID          string         `json:"function_id"`
	Enabled             bool           `json:"enabled"`
	AuthPolicy          AuthPolicy     `json:"auth_policy"`
	TokenVerifierDigest *digest.SHA256 `json:"token_verifier_digest,omitempty"`
}

// Validate checks the persisted, secret-free HTTP Trigger contract.
func (t HTTPTrigger) Validate() error {
	if err := t.Metadata.Validate(); err != nil {
		return err
	}
	if !identifierPattern.MatchString(t.FunctionID) {
		return problem.Invalid("function_id", "must be a valid identifier")
	}
	switch t.AuthPolicy {
	case AuthPolicyToken:
		if t.TokenVerifierDigest == nil {
			return problem.Invalid("token_verifier_digest", "is required for token authentication")
		}
		if _, err := digest.ParseSHA256(t.TokenVerifierDigest.String()); err != nil {
			return problem.Invalid("token_verifier_digest", "must be a lowercase SHA-256 digest")
		}
	case AuthPolicyPublic:
		if t.TokenVerifierDigest != nil {
			return problem.Invalid("token_verifier_digest", "is not allowed for public authentication")
		}
	default:
		return problem.Invalid("auth_policy", "must be token or public")
	}
	return nil
}

// CreateFunctionCommand contains every value that changes catalog state. A
// Raft adapter supplies the IDs, UTC timestamp, and applied index before Apply;
// Catalog never generates values or reads local time.
type CreateFunctionCommand struct {
	IfNoneMatch  bool           `json:"if_none_match"`
	AppliedIndex uint64         `json:"applied_index"`
	Function     model.Function `json:"function"`
	Trigger      HTTPTrigger    `json:"trigger"`
}

// SetFunctionLifecycleCommand conditionally changes only the Function desired
// lifecycle. It intentionally does not update the Trigger or Route.
type SetFunctionLifecycleCommand struct {
	FunctionID               string                  `json:"function_id"`
	ExpectedResourceRevision uint64                  `json:"expected_resource_revision"`
	Lifecycle                model.FunctionLifecycle `json:"lifecycle"`
	UpdatedAt                time.Time               `json:"updated_at"`
	AppliedIndex             uint64                  `json:"applied_index"`
}

// Catalog is a concurrency-safe, deterministic core for the Function and
// default HTTP Trigger portion of a future replicated FSM.
type Catalog struct {
	mu sync.Mutex

	functionsByID    map[string]model.Function
	functionIDByName map[string]string
	triggersByID     map[string]HTTPTrigger
	triggerIDByFunc  map[string]string
}

// CatalogSnapshot is an ordered, defensive copy of persisted catalog state.
type CatalogSnapshot struct {
	Functions []model.Function `json:"functions"`
	Triggers  []HTTPTrigger    `json:"triggers"`
}

// RevisionConflict describes a deterministic optimistic-concurrency mismatch.
// A future Management adapter can publish these safe values as revision_kind,
// expected_revision, and actual_revision without parsing an error message.
type RevisionConflict struct {
	RevisionKind string
	Expected     uint64
	Actual       uint64
}

func (e *RevisionConflict) Error() string {
	if e == nil {
		return ""
	}
	return "revision_conflict: expected " + e.RevisionKind + " does not match current resource"
}

// Unwrap preserves the stable public classification for existing callers.
func (e *RevisionConflict) Unwrap() error {
	return &problem.Error{Code: problem.CodeRevisionConflict, Message: "expected revision does not match current resource"}
}

// Details returns the safe API detail names required by FN-007.
func (e *RevisionConflict) Details() map[string]any {
	if e == nil {
		return nil
	}
	return map[string]any{
		"revision_kind":     e.RevisionKind,
		"expected_revision": e.Expected,
		"actual_revision":   e.Actual,
	}
}

// NewCatalog returns an empty catalog with fixed v1 semantics.
func NewCatalog() *Catalog {
	return &Catalog{
		functionsByID:    make(map[string]model.Function),
		functionIDByName: make(map[string]string),
		triggersByID:     make(map[string]HTTPTrigger),
		triggerIDByFunc:  make(map[string]string),
	}
}

// CreateFunction atomically creates one Function and its default HTTP Trigger.
// The caller owns idempotency via Ledger in the future encompassing FSM
// transaction; this primitive only evaluates the deterministic catalog write.
func (c *Catalog) CreateFunction(command CreateFunctionCommand) (CatalogSnapshot, error) {
	if c == nil {
		return CatalogSnapshot{}, errors.New("control-plane catalog is nil")
	}
	if err := validateCreateFunction(command); err != nil {
		return CatalogSnapshot{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.functionsByID[command.Function.ID]; exists {
		return CatalogSnapshot{}, classified(problem.CodeConflict, "function id already exists")
	}
	if _, exists := c.functionIDByName[command.Function.Name]; exists {
		return CatalogSnapshot{}, classified(problem.CodeConflict, "function name already exists")
	}
	if _, exists := c.triggersByID[command.Trigger.ID]; exists {
		return CatalogSnapshot{}, classified(problem.CodeConflict, "trigger id already exists")
	}
	if _, exists := c.triggerIDByFunc[command.Function.ID]; exists {
		return CatalogSnapshot{}, classified(problem.CodeConflict, "function already has a default HTTP trigger")
	}
	if len(c.functionsByID) >= DefaultMaxFunctions {
		return CatalogSnapshot{}, classified(problem.CodeOverloaded, "function catalog is full")
	}
	c.functionsByID[command.Function.ID] = cloneFunction(command.Function)
	c.functionIDByName[command.Function.Name] = command.Function.ID
	c.triggersByID[command.Trigger.ID] = cloneHTTPTrigger(command.Trigger)
	c.triggerIDByFunc[command.Function.ID] = command.Trigger.ID
	return CatalogSnapshot{
		Functions: []model.Function{cloneFunction(command.Function)},
		Triggers:  []HTTPTrigger{cloneHTTPTrigger(command.Trigger)},
	}, nil
}

// SetFunctionLifecycle compares the exact current resource revision before
// changing an Active Function to Disabled or restoring a Disabled Function.
func (c *Catalog) SetFunctionLifecycle(command SetFunctionLifecycleCommand) (model.Function, error) {
	if c == nil {
		return model.Function{}, errors.New("control-plane catalog is nil")
	}
	if err := validateLifecycleCommand(command); err != nil {
		return model.Function{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	function, exists := c.functionsByID[command.FunctionID]
	if !exists {
		return model.Function{}, classified(problem.CodeNotFound, "function was not found")
	}
	if function.ResourceRevision != command.ExpectedResourceRevision {
		return model.Function{}, revisionConflict("resource_revision", command.ExpectedResourceRevision, function.ResourceRevision)
	}
	if function.Lifecycle != model.FunctionActive && function.Lifecycle != model.FunctionDisabled {
		return model.Function{}, classified(problem.CodeConflict, "function lifecycle cannot be changed in its current state")
	}
	if function.Lifecycle == command.Lifecycle {
		return cloneFunction(function), nil
	}
	function.Lifecycle = command.Lifecycle
	function.UpdatedAt = command.UpdatedAt.Round(0)
	function.ResourceRevision++
	if err := function.Validate(); err != nil {
		return model.Function{}, fmt.Errorf("validating updated function: %w", err)
	}
	c.functionsByID[function.ID] = function
	return cloneFunction(function), nil
}

// GetFunction returns a Function and its default HTTP Trigger by immutable ID.
func (c *Catalog) GetFunction(functionID string) (model.Function, HTTPTrigger, error) {
	if c == nil {
		return model.Function{}, HTTPTrigger{}, errors.New("control-plane catalog is nil")
	}
	if !identifierPattern.MatchString(functionID) {
		return model.Function{}, HTTPTrigger{}, problem.Invalid("function_id", "must be a valid identifier")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	function, exists := c.functionsByID[functionID]
	if !exists {
		return model.Function{}, HTTPTrigger{}, classified(problem.CodeNotFound, "function was not found")
	}
	triggerID := c.triggerIDByFunc[functionID]
	trigger, exists := c.triggersByID[triggerID]
	if !exists {
		return model.Function{}, HTTPTrigger{}, errors.New("catalog invariant: function has no default HTTP trigger")
	}
	return cloneFunction(function), cloneHTTPTrigger(trigger), nil
}

// GetFunctionByName returns one Function and Trigger by its namespace-unique
// name without exposing the internal name index.
func (c *Catalog) GetFunctionByName(name string) (model.Function, HTTPTrigger, error) {
	if c == nil {
		return model.Function{}, HTTPTrigger{}, errors.New("control-plane catalog is nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	functionID, exists := c.functionIDByName[name]
	if !exists {
		return model.Function{}, HTTPTrigger{}, classified(problem.CodeNotFound, "function was not found")
	}
	function := c.functionsByID[functionID]
	trigger := c.triggersByID[c.triggerIDByFunc[functionID]]
	return cloneFunction(function), cloneHTTPTrigger(trigger), nil
}

// Snapshot returns all persisted values ordered by Function creation position.
func (c *Catalog) Snapshot() CatalogSnapshot {
	if c == nil {
		return CatalogSnapshot{Functions: []model.Function{}, Triggers: []HTTPTrigger{}}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return snapshotLocked(c)
}

// StateDigest produces a stable, domain-separated digest over this catalog
// slice. It is a building block for the future complete FSM State Digest.
func (c *Catalog) StateDigest() (digest.SHA256, error) {
	if c == nil {
		return "", errors.New("control-plane catalog is nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	snapshot := snapshotLocked(c)
	encoded, err := json.Marshal(canonicalCatalog(snapshot))
	if err != nil {
		return "", fmt.Errorf("marshaling canonical catalog: %w", err)
	}
	return digest.CanonicalJSON(CatalogStateDigestDomain, CatalogStateDigestVersion, encoded)
}

func validateCreateFunction(command CreateFunctionCommand) error {
	if !command.IfNoneMatch {
		return problem.Invalid("if_none_match", "is required when creating a function")
	}
	if command.AppliedIndex == 0 {
		return problem.Invalid("applied_index", "must be greater than zero")
	}
	if err := command.Function.Validate(); err != nil {
		return err
	}
	if command.Function.Lifecycle != model.FunctionActive {
		return problem.Invalid("function.lifecycle", "must be Active when creating a function")
	}
	if command.Function.ActiveRouteRevision != 0 {
		return problem.Invalid("function.active_route_revision", "must be zero before the first Route is published")
	}
	if command.Function.CreatedRaftIndex != command.AppliedIndex || command.Function.ResourceRevision != 1 {
		return problem.Invalid("function", "must be created at this applied index with resource revision one")
	}
	if err := command.Trigger.Validate(); err != nil {
		return err
	}
	if command.Trigger.FunctionID != command.Function.ID || !command.Trigger.Enabled {
		return problem.Invalid("trigger", "must be enabled and belong to the created function")
	}
	if command.Trigger.CreatedRaftIndex != command.AppliedIndex || command.Trigger.ResourceRevision != 1 ||
		!command.Trigger.CreatedAt.Equal(command.Function.CreatedAt) || !command.Trigger.UpdatedAt.Equal(command.Function.UpdatedAt) {
		return problem.Invalid("trigger", "must share the creation command and timestamp with its function")
	}
	return nil
}

func validateLifecycleCommand(command SetFunctionLifecycleCommand) error {
	if !identifierPattern.MatchString(command.FunctionID) {
		return problem.Invalid("function_id", "must be a valid identifier")
	}
	if command.ExpectedResourceRevision == 0 {
		return problem.Invalid("expected_resource_revision", "must be greater than zero")
	}
	if command.Lifecycle != model.FunctionActive && command.Lifecycle != model.FunctionDisabled {
		return problem.Invalid("lifecycle", "must be Active or Disabled")
	}
	if err := validateUTC("updated_at", command.UpdatedAt); err != nil {
		return err
	}
	if command.AppliedIndex == 0 {
		return problem.Invalid("applied_index", "must be greater than zero")
	}
	return nil
}

func snapshotLocked(c *Catalog) CatalogSnapshot {
	snapshot := CatalogSnapshot{
		Functions: make([]model.Function, 0, len(c.functionsByID)),
		Triggers:  make([]HTTPTrigger, 0, len(c.triggersByID)),
	}
	for _, function := range c.functionsByID {
		snapshot.Functions = append(snapshot.Functions, cloneFunction(function))
	}
	slices.SortFunc(snapshot.Functions, compareFunction)
	for _, function := range snapshot.Functions {
		trigger := c.triggersByID[c.triggerIDByFunc[function.ID]]
		snapshot.Triggers = append(snapshot.Triggers, cloneHTTPTrigger(trigger))
	}
	return snapshot
}

func cloneFunction(function model.Function) model.Function {
	function.Labels = mapsClone(function.Labels)
	return function
}

func cloneHTTPTrigger(trigger HTTPTrigger) HTTPTrigger {
	if trigger.TokenVerifierDigest != nil {
		value := *trigger.TokenVerifierDigest
		trigger.TokenVerifierDigest = &value
	}
	return trigger
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

func compareFunction(left, right model.Function) int {
	if left.CreatedRaftIndex < right.CreatedRaftIndex {
		return -1
	}
	if left.CreatedRaftIndex > right.CreatedRaftIndex {
		return 1
	}
	return strings.Compare(left.ID, right.ID)
}

func revisionConflict(kind string, expected, actual uint64) error {
	return &RevisionConflict{RevisionKind: kind, Expected: expected, Actual: actual}
}

type canonicalCatalogState struct {
	Functions []canonicalFunction    `json:"functions"`
	Triggers  []canonicalHTTPTrigger `json:"triggers"`
}

type canonicalFunction struct {
	ID                  string                  `json:"id"`
	Namespace           string                  `json:"namespace"`
	CreatedAt           string                  `json:"created_at"`
	UpdatedAt           string                  `json:"updated_at"`
	CreatedRaftIndex    uint64                  `json:"created_raft_index"`
	ResourceRevision    uint64                  `json:"resource_revision"`
	Name                string                  `json:"name"`
	Description         string                  `json:"description,omitempty"`
	ActiveRouteRevision uint64                  `json:"active_route_revision"`
	Labels              []canonicalLabel        `json:"labels"`
	Lifecycle           model.FunctionLifecycle `json:"lifecycle"`
}

type canonicalLabel struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type canonicalHTTPTrigger struct {
	ID                  string     `json:"id"`
	Namespace           string     `json:"namespace"`
	CreatedAt           string     `json:"created_at"`
	UpdatedAt           string     `json:"updated_at"`
	CreatedRaftIndex    uint64     `json:"created_raft_index"`
	ResourceRevision    uint64     `json:"resource_revision"`
	FunctionID          string     `json:"function_id"`
	Enabled             bool       `json:"enabled"`
	AuthPolicy          AuthPolicy `json:"auth_policy"`
	TokenVerifierDigest *string    `json:"token_verifier_digest,omitempty"`
}

func canonicalCatalog(snapshot CatalogSnapshot) canonicalCatalogState {
	state := canonicalCatalogState{
		Functions: make([]canonicalFunction, 0, len(snapshot.Functions)),
		Triggers:  make([]canonicalHTTPTrigger, 0, len(snapshot.Triggers)),
	}
	for _, function := range snapshot.Functions {
		labels := make([]canonicalLabel, 0, len(function.Labels))
		for key, value := range function.Labels {
			labels = append(labels, canonicalLabel{Key: key, Value: value})
		}
		slices.SortFunc(labels, func(left, right canonicalLabel) int { return strings.Compare(left.Key, right.Key) })
		state.Functions = append(state.Functions, canonicalFunction{
			ID: function.ID, Namespace: function.Namespace,
			CreatedAt: function.CreatedAt.Format(time.RFC3339Nano), UpdatedAt: function.UpdatedAt.Format(time.RFC3339Nano),
			CreatedRaftIndex: function.CreatedRaftIndex, ResourceRevision: function.ResourceRevision,
			Name: function.Name, Description: function.Description, ActiveRouteRevision: function.ActiveRouteRevision,
			Labels: labels, Lifecycle: function.Lifecycle,
		})
	}
	for _, trigger := range snapshot.Triggers {
		var verifier *string
		if trigger.TokenVerifierDigest != nil {
			value := trigger.TokenVerifierDigest.String()
			verifier = &value
		}
		state.Triggers = append(state.Triggers, canonicalHTTPTrigger{
			ID: trigger.ID, Namespace: trigger.Namespace,
			CreatedAt: trigger.CreatedAt.Format(time.RFC3339Nano), UpdatedAt: trigger.UpdatedAt.Format(time.RFC3339Nano),
			CreatedRaftIndex: trigger.CreatedRaftIndex, ResourceRevision: trigger.ResourceRevision,
			FunctionID: trigger.FunctionID, Enabled: trigger.Enabled, AuthPolicy: trigger.AuthPolicy, TokenVerifierDigest: verifier,
		})
	}
	return state
}
