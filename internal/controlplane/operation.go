// Package controlplane contains deterministic state primitives for the future
// Raft-backed Controller. It performs no I/O and never reads the local clock.
package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/strictjson"
)

const (
	DefaultNamespace = "default"

	OperationDigestDomain  = "control-operation-request"
	OperationDigestVersion = "v1"

	DefaultMaxOperations = 100_000
	HardMaxOperations    = 100_000

	DefaultOperationTTL          = 24 * time.Hour
	DefaultOperationTombstoneTTL = 7 * 24 * time.Hour
	MinOperationTTL              = time.Hour
	MaxOperationTTL              = 30 * 24 * time.Hour
	MinOperationTombstoneTTL     = 24 * time.Hour
	MaxOperationTombstoneTTL     = 30 * 24 * time.Hour

	MaxOperationBodyBytes = 1 << 20
	MaxOperationPathBytes = 4_096
	MaxOperationMessage   = 512
)

var (
	identifierPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	httpMethodPattern = regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]{1,16}$`)
)

// OperationKey scopes a client-supplied Operation ID to its authenticated
// principal and namespace. Principal comes from the management authentication
// layer and is never derived from a request body.
type OperationKey struct {
	Principal   string `json:"principal"`
	Namespace   string `json:"namespace"`
	OperationID string `json:"operation_id"`
}

// Preconditions are the full optimistic-concurrency inputs included in a
// canonical management request digest. Nil is deliberately distinct from zero
// for the two revision pointers.
type Preconditions struct {
	IfNoneMatch                 bool    `json:"if_none_match"`
	ExpectedResourceRevision    *uint64 `json:"expected_resource_revision"`
	ExpectedActiveRouteRevision *uint64 `json:"expected_active_route_revision"`
}

// Request is the non-secret material used to derive one operation digest.
// Body is never stored in Ledger; it exists only while calculating Digest.
type Request struct {
	Method         string          `json:"method"`
	Path           string          `json:"path"`
	Preconditions  Preconditions   `json:"preconditions"`
	BodyPresent    bool            `json:"body_present"`
	Body           json.RawMessage `json:"body"`
	ArtifactDigest *digest.SHA256  `json:"artifact_digest"`
}

// OutcomeStatus is the terminal management-operation result class.
type OutcomeStatus string

const (
	OutcomeSucceeded OutcomeStatus = "succeeded"
	OutcomeFailed    OutcomeStatus = "failed"
)

// Failure is a safe, stable terminal error. It intentionally has no arbitrary
// details payload because Ledger is persisted in the control-plane state.
type Failure struct {
	Code    problem.Code `json:"code"`
	Message string       `json:"message"`
}

// AffectedResource carries only the stable identifiers and revisions needed by
// an idempotent retry. It must never contain a secret, verifier, or raw body.
type AffectedResource struct {
	Kind             string  `json:"kind"`
	ID               string  `json:"id"`
	ResourceRevision *uint64 `json:"resource_revision"`
	RouteRevision    *uint64 `json:"route_revision"`
}

// Outcome is the de-sensitized terminal result retained for an Operation.
// CredentialIssued records that a one-time credential was returned in the
// original response, while intentionally omitting that credential itself.
type Outcome struct {
	Status            OutcomeStatus      `json:"status"`
	Failure           *Failure           `json:"failure,omitempty"`
	AffectedResources []AffectedResource `json:"affected_resources"`
	CredentialIssued  bool               `json:"credential_issued"`
}

// Completion is the deterministic command input that records a final result.
// CompletedAt and AppliedIndex must be supplied by the committed command; the
// Ledger never calls time.Now.
type Completion struct {
	Key          OperationKey `json:"key"`
	Request      Request      `json:"request"`
	Outcome      Outcome      `json:"outcome"`
	CompletedAt  time.Time    `json:"completed_at"`
	AppliedIndex uint64       `json:"applied_index"`
}

// Record is a completed operation's safely replayable state.
type Record struct {
	Key          OperationKey  `json:"key"`
	Digest       digest.SHA256 `json:"digest"`
	Outcome      Outcome       `json:"outcome"`
	CompletedAt  time.Time     `json:"completed_at"`
	AppliedIndex uint64        `json:"applied_index"`
}

// Tombstone reserves an expired Operation ID without retaining its outcome.
type Tombstone struct {
	Key          OperationKey  `json:"key"`
	Digest       digest.SHA256 `json:"digest"`
	TombstonedAt time.Time     `json:"tombstoned_at"`
}

// Snapshot is a deterministic, defensive projection suitable for a future
// Raft snapshot codec. Records and Tombstones use canonical key ordering.
type Snapshot struct {
	Records    []Record    `json:"records"`
	Tombstones []Tombstone `json:"tombstones"`
}

// Status reports bounded ledger occupancy without exposing request bodies.
type Status struct {
	Records       int
	Tombstones    int
	MaxOperations int
}

// CompletionDisposition describes whether Complete inserted a new record or
// returned one from a prior committed operation.
type CompletionDisposition string

const (
	CompletionApplied                 CompletionDisposition = "applied"
	CompletionReplay                  CompletionDisposition = "replay"
	CompletionCredentialNotReplayable CompletionDisposition = "credential_not_replayable"
)

// CompletionResult contains a defensive copy of the retained terminal record.
type CompletionResult struct {
	Disposition CompletionDisposition
	Record      Record
}

// GCResult reports deterministic state changes produced by an explicit
// committed cutoff command.
type GCResult struct {
	Tombstoned int
	Deleted    int
}

type ledgerConfig struct {
	MaxOperations int
	OperationTTL  time.Duration
	TombstoneTTL  time.Duration
}

// Ledger is safe for concurrent callers. Future Raft application can use it
// while holding its FSM transaction lock; the mutex also makes direct tests and
// management adapters safe without changing state semantics.
type Ledger struct {
	mu sync.Mutex

	maxOperations int
	operationTTL  time.Duration
	tombstoneTTL  time.Duration
	records       map[operationKey]Record
	tombstones    map[operationKey]Tombstone
}

type operationKey struct {
	principal   string
	namespace   string
	operationID string
}

// New creates a ledger with immutable v1 retention constants. Configurable
// retention belongs to replicated future FSM configuration, never node-local
// constructor input, because it changes Complete and GC state transitions.
func New() *Ledger {
	return newLedgerWithConfig(ledgerConfig{
		MaxOperations: DefaultMaxOperations,
		OperationTTL:  DefaultOperationTTL,
		TombstoneTTL:  DefaultOperationTombstoneTTL,
	})
}

func newLedgerWithConfig(config ledgerConfig) *Ledger {
	return &Ledger{
		maxOperations: config.MaxOperations,
		operationTTL:  config.OperationTTL,
		tombstoneTTL:  config.TombstoneTTL,
		records:       make(map[operationKey]Record),
		tombstones:    make(map[operationKey]Tombstone),
	}
}

// Digest validates and canonicalizes one control request into a versioned,
// domain-separated digest. Authentication, request IDs, connection metadata,
// and retry headers are intentionally not represented by Request.
func (r Request) Digest() (digest.SHA256, error) {
	method, err := canonicalMethod(r.Method)
	if err != nil {
		return "", err
	}
	path, err := canonicalPath(r.Path)
	if err != nil {
		return "", err
	}
	if err := r.Preconditions.validate(); err != nil {
		return "", err
	}
	if err := validateBody(r.BodyPresent, r.Body); err != nil {
		return "", err
	}
	if r.ArtifactDigest != nil {
		if _, err := digest.ParseSHA256(r.ArtifactDigest.String()); err != nil {
			return "", problem.Invalid("artifact_digest", "must be a lowercase sha-256 digest")
		}
	}

	canonical := canonicalRequest{
		Method:                      method,
		Path:                        path,
		IfNoneMatch:                 r.Preconditions.IfNoneMatch,
		ExpectedResourceRevision:    r.Preconditions.ExpectedResourceRevision,
		ExpectedActiveRouteRevision: r.Preconditions.ExpectedActiveRouteRevision,
		BodyPresent:                 r.BodyPresent,
		Body:                        r.Body,
		ArtifactDigest:              r.ArtifactDigest,
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("marshaling canonical control operation request: %w", err)
	}
	return digest.CanonicalJSON(OperationDigestDomain, OperationDigestVersion, encoded)
}

// Complete atomically persists a terminal result or returns the original result
// for the same completed request. Callers must invoke it in the same future
// FSM transaction as the associated resource mutation.
func (l *Ledger) Complete(completion Completion) (CompletionResult, error) {
	if l == nil {
		return CompletionResult{}, errors.New("control-plane operation ledger is nil")
	}
	if err := completion.Key.Validate(); err != nil {
		return CompletionResult{}, err
	}
	key := makeOperationKey(completion.Key)
	// Tombstones have precedence over any caller-supplied retry material: an
	// Operation ID remains expired until a committed GC command removes it.
	l.mu.Lock()
	_, tombstoned := l.tombstones[key]
	l.mu.Unlock()
	if tombstoned {
		return CompletionResult{}, classified(problem.CodeOperationExpired, "operation id is retained as an expired tombstone")
	}
	digestValue, err := completion.Request.Digest()
	if err != nil {
		return CompletionResult{}, err
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.tombstones[key]; exists {
		return CompletionResult{}, classified(problem.CodeOperationExpired, "operation id is retained as an expired tombstone")
	}
	if current, exists := l.records[key]; exists {
		if current.Digest != digestValue {
			return CompletionResult{}, classified(problem.CodeConflict, "operation id was already used with a different request")
		}
		if current.Outcome.CredentialIssued {
			// The future Management layer must resolve these safe resource IDs
			// against its current authoritative state before returning revisions.
			return CompletionResult{
				Disposition: CompletionCredentialNotReplayable,
				Record:      cloneRecord(current),
			}, nil
		}
		return CompletionResult{
			Disposition: CompletionReplay,
			Record:      cloneRecord(current),
		}, nil
	}
	if err := completion.validateForInsert(); err != nil {
		return CompletionResult{}, err
	}
	if len(l.records)+len(l.tombstones) >= l.maxOperations {
		return CompletionResult{}, classified(problem.CodeOverloaded, "control-plane operation ledger is full")
	}
	record := Record{
		Key:          completion.Key,
		Digest:       digestValue,
		Outcome:      cloneOutcome(completion.Outcome),
		CompletedAt:  completion.CompletedAt,
		AppliedIndex: completion.AppliedIndex,
	}
	l.records[key] = record
	return CompletionResult{Disposition: CompletionApplied, Record: cloneRecord(record)}, nil
}

// Lookup returns the retained safe outcome. Missing keys are not equivalent to
// an expired tombstone: callers can distinguish a never-seen operation from
// one whose retry window has ended.
func (l *Ledger) Lookup(key OperationKey) (Record, error) {
	if l == nil {
		return Record{}, errors.New("control-plane operation ledger is nil")
	}
	if err := key.Validate(); err != nil {
		return Record{}, err
	}
	internalKey := makeOperationKey(key)
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.tombstones[internalKey]; exists {
		return Record{}, classified(problem.CodeOperationExpired, "operation id is retained as an expired tombstone")
	}
	record, exists := l.records[internalKey]
	if !exists {
		return Record{}, classified(problem.CodeNotFound, "operation was not found")
	}
	return cloneRecord(record), nil
}

// GC applies a Leader-supplied UTC cutoff. It first removes retained outcomes
// whose completed-at retention has elapsed, leaving only tombstones, then
// removes tombstones whose own retention has elapsed. It never reads local time.
func (l *Ledger) GC(cutoff time.Time) (GCResult, error) {
	if l == nil {
		return GCResult{}, errors.New("control-plane operation ledger is nil")
	}
	if err := validateUTC("cutoff", cutoff); err != nil {
		return GCResult{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	result := GCResult{}
	for key, record := range l.records {
		if record.CompletedAt.Add(l.operationTTL).After(cutoff) {
			continue
		}
		delete(l.records, key)
		l.tombstones[key] = Tombstone{
			Key:          record.Key,
			Digest:       record.Digest,
			TombstonedAt: cutoff,
		}
		result.Tombstoned++
	}
	for key, tombstone := range l.tombstones {
		if tombstone.TombstonedAt.Add(l.tombstoneTTL).After(cutoff) {
			continue
		}
		delete(l.tombstones, key)
		result.Deleted++
	}
	return result, nil
}

// Snapshot returns an ordered defensive copy of the entire persisted state.
func (l *Ledger) Snapshot() Snapshot {
	if l == nil {
		return Snapshot{Records: []Record{}, Tombstones: []Tombstone{}}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	snapshot := Snapshot{
		Records:    make([]Record, 0, len(l.records)),
		Tombstones: make([]Tombstone, 0, len(l.tombstones)),
	}
	for _, record := range l.records {
		snapshot.Records = append(snapshot.Records, cloneRecord(record))
	}
	for _, tombstone := range l.tombstones {
		snapshot.Tombstones = append(snapshot.Tombstones, tombstone)
	}
	slices.SortFunc(snapshot.Records, func(left, right Record) int {
		return compareOperationKey(left.Key, right.Key)
	})
	slices.SortFunc(snapshot.Tombstones, func(left, right Tombstone) int {
		return compareOperationKey(left.Key, right.Key)
	})
	return snapshot
}

// Status returns bounded ledger occupancy without exposing persisted outcomes.
func (l *Ledger) Status() Status {
	if l == nil {
		return Status{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return Status{
		Records:       len(l.records),
		Tombstones:    len(l.tombstones),
		MaxOperations: l.maxOperations,
	}
}

func (k OperationKey) Validate() error {
	if !identifierPattern.MatchString(k.Principal) {
		return problem.Invalid("principal", "must be a valid identifier")
	}
	if k.Namespace != DefaultNamespace {
		return problem.Invalid("namespace", "only default is supported in v1")
	}
	if !identifierPattern.MatchString(k.OperationID) {
		return problem.Invalid("operation_id", "must be a valid identifier")
	}
	return nil
}

func (p Preconditions) validate() error {
	if p.IfNoneMatch && (p.ExpectedResourceRevision != nil || p.ExpectedActiveRouteRevision != nil) {
		return problem.Invalid("preconditions", "if-none-match cannot be combined with expected revisions")
	}
	if p.ExpectedResourceRevision != nil && *p.ExpectedResourceRevision == 0 {
		return problem.Invalid("expected_resource_revision", "must be greater than zero")
	}
	return nil
}

func (o Outcome) validate() error {
	if len(o.AffectedResources) > 256 {
		return problem.Invalid("affected_resources", "exceeds the v1 operation result limit")
	}
	if o.CredentialIssued && o.Status != OutcomeSucceeded {
		return problem.Invalid("credential_issued", "is only valid for a successful operation")
	}
	if o.CredentialIssued && len(o.AffectedResources) == 0 {
		return problem.Invalid("credential_issued", "requires at least one affected resource")
	}
	switch o.Status {
	case OutcomeSucceeded:
		if o.Failure != nil {
			return problem.Invalid("failure", "is only valid for a failed operation")
		}
	case OutcomeFailed:
		if err := o.Failure.validate(); err != nil {
			return err
		}
	default:
		return problem.Invalid("status", "must be a terminal operation status")
	}
	seen := make(map[string]struct{}, len(o.AffectedResources))
	for _, resource := range o.AffectedResources {
		if err := resource.validate(); err != nil {
			return err
		}
		key := resource.Kind + "\x00" + resource.ID
		if _, exists := seen[key]; exists {
			return problem.Invalid("affected_resources", "contains a duplicate resource")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func (f *Failure) validate() error {
	if f == nil {
		return problem.Invalid("failure", "is required for a failed operation")
	}
	if !problem.Known(f.Code) {
		return problem.Invalid("failure.code", "must be a stable v1 error code")
	}
	if f.Message == "" || len(f.Message) > MaxOperationMessage || !utf8.ValidString(f.Message) {
		return problem.Invalid("failure.message", "must be valid UTF-8 and within 512 bytes")
	}
	return nil
}

func (r AffectedResource) validate() error {
	if !identifierPattern.MatchString(r.Kind) {
		return problem.Invalid("affected_resources.kind", "must be a valid identifier")
	}
	if !identifierPattern.MatchString(r.ID) {
		return problem.Invalid("affected_resources.id", "must be a valid identifier")
	}
	if r.ResourceRevision == nil && r.RouteRevision == nil {
		return problem.Invalid("affected_resources", "must include a resource or route revision")
	}
	if r.ResourceRevision != nil && *r.ResourceRevision == 0 {
		return problem.Invalid("affected_resources.resource_revision", "must be greater than zero")
	}
	if r.RouteRevision != nil && *r.RouteRevision == 0 {
		return problem.Invalid("affected_resources.route_revision", "must be greater than zero")
	}
	return nil
}

func (c Completion) validateForInsert() error {
	if err := c.Outcome.validate(); err != nil {
		return err
	}
	if err := validateUTC("completed_at", c.CompletedAt); err != nil {
		return err
	}
	if c.AppliedIndex == 0 {
		return problem.Invalid("applied_index", "must be greater than zero")
	}
	return nil
}

func canonicalMethod(value string) (string, error) {
	if !httpMethodPattern.MatchString(value) {
		return "", problem.Invalid("method", "must be a valid HTTP token within 16 bytes")
	}
	return strings.ToUpper(value), nil
}

func canonicalPath(value string) (string, error) {
	if len(value) == 0 || len(value) > MaxOperationPathBytes || !utf8.ValidString(value) ||
		strings.ContainsRune(value, '\x00') || !strings.HasPrefix(value, "/") {
		return "", problem.Invalid("path", "must be a valid absolute path within 4096 bytes")
	}
	if strings.ContainsAny(value, "?#") {
		return "", problem.Invalid("path", "must not contain a query or fragment")
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Host != "" || !utf8.ValidString(parsed.Path) {
		return "", problem.Invalid("path", "must be a valid absolute URI path")
	}
	canonical := (&url.URL{Path: parsed.Path}).EscapedPath()
	if canonical != value {
		return "", problem.Invalid("path", "must use canonical percent encoding")
	}
	return canonical, nil
}

func validateBody(present bool, body json.RawMessage) error {
	if !present {
		if len(body) != 0 {
			return problem.Invalid("body", "must be absent when body_present is false")
		}
		return nil
	}
	if len(body) == 0 || len(body) > MaxOperationBodyBytes {
		return problem.Invalid("body", "must be a JSON value within the management body limit")
	}
	if err := strictjson.Validate(body, 32); err != nil {
		return problem.Invalid("body", "must be bounded strict JSON")
	}
	return nil
}

func validateUTC(field string, value time.Time) error {
	if value.IsZero() {
		return problem.Invalid(field, "is required")
	}
	_, offset := value.Zone()
	if offset != 0 {
		return problem.Invalid(field, "must use UTC")
	}
	return nil
}

func makeOperationKey(key OperationKey) operationKey {
	return operationKey{principal: key.Principal, namespace: key.Namespace, operationID: key.OperationID}
}

func cloneRecord(record Record) Record {
	record.Outcome = cloneOutcome(record.Outcome)
	return record
}

func cloneOutcome(outcome Outcome) Outcome {
	if outcome.Failure != nil {
		failure := *outcome.Failure
		outcome.Failure = &failure
	}
	resources := outcome.AffectedResources
	outcome.AffectedResources = make([]AffectedResource, len(outcome.AffectedResources))
	copy(outcome.AffectedResources, resources)
	for index := range outcome.AffectedResources {
		resource := &outcome.AffectedResources[index]
		if resource.ResourceRevision != nil {
			revision := *resource.ResourceRevision
			resource.ResourceRevision = &revision
		}
		if resource.RouteRevision != nil {
			revision := *resource.RouteRevision
			resource.RouteRevision = &revision
		}
	}
	slices.SortFunc(outcome.AffectedResources, func(left, right AffectedResource) int {
		if left.Kind != right.Kind {
			return strings.Compare(left.Kind, right.Kind)
		}
		return strings.Compare(left.ID, right.ID)
	})
	return outcome
}

func compareOperationKey(left, right OperationKey) int {
	if left.Principal != right.Principal {
		return strings.Compare(left.Principal, right.Principal)
	}
	if left.Namespace != right.Namespace {
		return strings.Compare(left.Namespace, right.Namespace)
	}
	return strings.Compare(left.OperationID, right.OperationID)
}

func classified(code problem.Code, message string) error {
	return &problem.Error{Code: code, Message: message}
}

type canonicalRequest struct {
	Method                      string          `json:"method"`
	Path                        string          `json:"path"`
	IfNoneMatch                 bool            `json:"if_none_match"`
	ExpectedResourceRevision    *uint64         `json:"expected_resource_revision"`
	ExpectedActiveRouteRevision *uint64         `json:"expected_active_route_revision"`
	BodyPresent                 bool            `json:"body_present"`
	Body                        json.RawMessage `json:"body"`
	ArtifactDigest              *digest.SHA256  `json:"artifact_digest"`
}
