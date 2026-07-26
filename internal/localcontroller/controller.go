// Package localcontroller connects Local Core I/O boundaries to deterministic
// control-plane state transitions. It is a single-process development profile,
// not a substitute for the replicated M1 controller.
package localcontroller

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/yourikka/minicloud/internal/artifact"
	"github.com/yourikka/minicloud/internal/controlplane"
	"github.com/yourikka/minicloud/internal/digest"
	validatorprotocol "github.com/yourikka/minicloud/internal/validator/protocol"
)

const (
	randomIDBytes    = 16
	randomTokenBytes = 32
	routeSaltBytes   = 16
)

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// ArtifactStore is the content-addressed byte authority used by Local Core.
// OpenVerified returns an open file that the caller must close unless it is
// handed to Validator, which then owns closing it.
type ArtifactStore interface {
	Put(context.Context, digest.SHA256, io.Reader) (artifact.Info, error)
	OpenVerified(context.Context, digest.SHA256) (*os.File, artifact.Info, error)
}

// Validator executes one isolated admission attempt. Validate takes ownership
// of artifact and closes it on every return path.
type Validator interface {
	Validate(context.Context, validatorprotocol.Request, io.ReadCloser) (validatorprotocol.Report, error)
}

// CommandMeta contains the non-deterministic values required by exactly one
// deterministic control-plane command.
type CommandMeta struct {
	AppliedIndex uint64
	At           time.Time
}

// CommandSource supplies command metadata at the Local Core boundary. A future
// Raft adapter will replace it with committed log indexes and leader time.
type CommandSource interface {
	Next() (CommandMeta, error)
}

// IDSource supplies opaque control-plane identifiers outside deterministic
// state application.
type IDSource interface {
	NewID(prefix string) (string, error)
}

// SaltSource creates the 128-bit Route salt used by the locked v1 hash
// contract. Salt generation is outside deterministic state application.
type SaltSource interface {
	NewSalt() ([]byte, error)
}

// TokenSource creates high-entropy Invocation Token plaintext outside
// deterministic state application.
type TokenSource interface {
	NewToken() (string, error)
}

// Config declares the I/O and non-deterministic Local Core dependencies.
type Config struct {
	Artifacts ArtifactStore
	Validator Validator
	Commands  CommandSource
	IDs       IDSource
	Salts     SaltSource
	Tokens    TokenSource
}

// Controller is the sole Local Core write entry point for the Catalog and
// ReleaseStore it creates. It never exposes their mutable internals.
type Controller struct {
	artifacts ArtifactStore
	validator Validator
	commands  CommandSource
	ids       IDSource
	salts     SaltSource
	tokens    TokenSource

	catalog     *controlplane.Catalog
	releases    *controlplane.ReleaseStore
	routes      *controlplane.RouteStore
	assignments *controlplane.AssignmentStore
	ledger      *controlplane.Ledger

	operationMu sync.RWMutex

	validationMu sync.Mutex
	validating   map[string]struct{}

	commandMu   sync.Mutex
	lastCommand CommandMeta
}

// New creates one empty Local Core controller.
func New(config Config) (*Controller, error) {
	if config.Artifacts == nil {
		return nil, errors.New("local controller artifact store is required")
	}
	if config.Validator == nil {
		return nil, errors.New("local controller validator is required")
	}
	if config.Commands == nil {
		config.Commands = NewMonotonicCommandSource(nil)
	}
	if config.IDs == nil {
		config.IDs = NewRandomIDSource(nil)
	}
	if config.Salts == nil {
		config.Salts = NewRandomSaltSource(nil)
	}
	if config.Tokens == nil {
		config.Tokens = NewRandomTokenSource(nil)
	}

	catalog := controlplane.NewCatalog()
	releases := controlplane.NewReleaseStore(catalog)
	routes := controlplane.NewRouteStore(catalog, releases)
	assignments, err := controlplane.NewAssignmentStore(routes)
	if err != nil {
		return nil, fmt.Errorf("creating local assignment store: %w", err)
	}
	return &Controller{
		artifacts:   config.Artifacts,
		validator:   config.Validator,
		commands:    config.Commands,
		ids:         config.IDs,
		salts:       config.Salts,
		tokens:      config.Tokens,
		catalog:     catalog,
		releases:    releases,
		routes:      routes,
		assignments: assignments,
		ledger:      controlplane.New(),
		validating:  make(map[string]struct{}),
	}, nil
}

func (c *Controller) nextCommand() (CommandMeta, error) {
	if c == nil || c.commands == nil {
		return CommandMeta{}, errors.New("local controller command source is required")
	}
	c.commandMu.Lock()
	defer c.commandMu.Unlock()
	command, err := c.commands.Next()
	if err != nil {
		return CommandMeta{}, fmt.Errorf("getting command metadata: %w", err)
	}
	if command.AppliedIndex == 0 {
		return CommandMeta{}, errors.New("local controller command source returned zero applied index")
	}
	if command.At.IsZero() || command.At.Location() != time.UTC {
		return CommandMeta{}, errors.New("local controller command source returned a non-UTC timestamp")
	}
	command = CommandMeta{
		AppliedIndex: command.AppliedIndex,
		At:           command.At.Round(0),
	}
	if c.lastCommand.AppliedIndex != 0 && command.AppliedIndex <= c.lastCommand.AppliedIndex {
		return CommandMeta{}, errors.New("local controller command source did not advance applied index")
	}
	if !c.lastCommand.At.IsZero() && command.At.Before(c.lastCommand.At) {
		return CommandMeta{}, errors.New("local controller command source regressed timestamp")
	}
	c.lastCommand = command
	return command, nil
}

func (c *Controller) newID(prefix string) (string, error) {
	if c == nil || c.ids == nil {
		return "", errors.New("local controller id source is required")
	}
	id, err := c.ids.NewID(prefix)
	if err != nil {
		return "", fmt.Errorf("generating %s id: %w", prefix, err)
	}
	if id == "" {
		return "", fmt.Errorf("generating %s id: source returned an empty id", prefix)
	}
	if !identifierPattern.MatchString(id) {
		return "", fmt.Errorf("generating %s id: source returned an invalid id", prefix)
	}
	return id, nil
}

func (c *Controller) newRouteSalt() ([]byte, error) {
	if c == nil || c.salts == nil {
		return nil, errors.New("local controller salt source is required")
	}
	salt, err := c.salts.NewSalt()
	if err != nil {
		return nil, fmt.Errorf("generating route salt: %w", err)
	}
	if len(salt) != routeSaltBytes {
		return nil, errors.New("generating route salt: source returned an invalid length")
	}
	return append([]byte(nil), salt...), nil
}

func (c *Controller) claimValidation(versionID string) bool {
	c.validationMu.Lock()
	defer c.validationMu.Unlock()
	if _, exists := c.validating[versionID]; exists {
		return false
	}
	c.validating[versionID] = struct{}{}
	return true
}

func (c *Controller) releaseValidation(versionID string) {
	c.validationMu.Lock()
	defer c.validationMu.Unlock()
	delete(c.validating, versionID)
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("local controller context is required")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("local controller context is unavailable: %w", err)
	}
	return nil
}

// MonotonicCommandSource creates strictly increasing process-local command
// metadata. Its indexes are not Raft log indexes and reset on process restart.
type MonotonicCommandSource struct {
	mu sync.Mutex

	clock func() time.Time
	index uint64
	last  time.Time
}

// NewMonotonicCommandSource returns a Local Core command source. A nil clock
// uses the current wall clock at this process boundary.
func NewMonotonicCommandSource(clock func() time.Time) *MonotonicCommandSource {
	if clock == nil {
		clock = time.Now
	}
	return &MonotonicCommandSource{clock: clock}
}

// Next returns one UTC timestamp and applied index that cannot regress within
// this process, even if the injected wall clock moves backward.
func (s *MonotonicCommandSource) Next() (CommandMeta, error) {
	if s == nil || s.clock == nil {
		return CommandMeta{}, errors.New("local command source clock is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.index == math.MaxUint64 {
		return CommandMeta{}, errors.New("local command source applied index is exhausted")
	}

	now := s.clock()
	if now.IsZero() {
		return CommandMeta{}, errors.New("local command source clock returned zero time")
	}
	now = now.UTC().Round(0)
	if !s.last.IsZero() && !now.After(s.last) {
		now = s.last.Add(time.Nanosecond)
	}
	s.index++
	s.last = now
	return CommandMeta{AppliedIndex: s.index, At: now}, nil
}

// RandomIDSource creates opaque identifiers from a cryptographically secure
// byte source. Randomness remains at the controller boundary, never in state
// application.
type RandomIDSource struct {
	mu     sync.Mutex
	random io.Reader
}

// NewRandomIDSource returns an identifier source. A nil reader uses crypto/rand.
func NewRandomIDSource(random io.Reader) *RandomIDSource {
	if random == nil {
		random = cryptorand.Reader
	}
	return &RandomIDSource{random: random}
}

// NewID returns a prefix-qualified 128-bit random identifier.
func (s *RandomIDSource) NewID(prefix string) (string, error) {
	if s == nil || s.random == nil {
		return "", errors.New("local id source random reader is required")
	}
	if prefix == "" {
		return "", errors.New("local id prefix is required")
	}

	bytes := make([]byte, randomIDBytes)
	s.mu.Lock()
	_, err := io.ReadFull(s.random, bytes)
	s.mu.Unlock()
	if err != nil {
		return "", fmt.Errorf("reading local id randomness: %w", err)
	}
	return prefix + "-" + hex.EncodeToString(bytes), nil
}

// RandomSaltSource creates 128-bit route salts from a cryptographically secure
// byte source.
type RandomSaltSource struct {
	mu     sync.Mutex
	random io.Reader
}

// NewRandomSaltSource returns a Route salt source. A nil reader uses crypto/rand.
func NewRandomSaltSource(random io.Reader) *RandomSaltSource {
	if random == nil {
		random = cryptorand.Reader
	}
	return &RandomSaltSource{random: random}
}

// NewSalt returns one 128-bit random Route salt.
func (s *RandomSaltSource) NewSalt() ([]byte, error) {
	if s == nil || s.random == nil {
		return nil, errors.New("local route salt source random reader is required")
	}

	salt := make([]byte, routeSaltBytes)
	s.mu.Lock()
	_, err := io.ReadFull(s.random, salt)
	s.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("reading route salt randomness: %w", err)
	}
	return salt, nil
}

// RandomTokenSource creates 256-bit URL-safe Invocation Tokens.
type RandomTokenSource struct {
	mu     sync.Mutex
	random io.Reader
}

// NewRandomTokenSource returns a token source. A nil reader uses crypto/rand.
func NewRandomTokenSource(random io.Reader) *RandomTokenSource {
	if random == nil {
		random = cryptorand.Reader
	}
	return &RandomTokenSource{random: random}
}

// NewToken returns one unpadded URL-safe token containing 256 random bits.
func (s *RandomTokenSource) NewToken() (string, error) {
	if s == nil || s.random == nil {
		return "", errors.New("local invocation token source random reader is required")
	}
	random := make([]byte, randomTokenBytes)
	s.mu.Lock()
	_, err := io.ReadFull(s.random, random)
	s.mu.Unlock()
	if err != nil {
		return "", fmt.Errorf("reading invocation token randomness: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(random), nil
}
