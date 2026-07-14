package servingauth

import (
	"regexp"
	"time"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/problem"
)

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// Mode identifies whether an Assignment serves normal traffic or only pinned
// asynchronous drain work.
type Mode string

const (
	ModeNormal    Mode = "normal"
	ModeDrainOnly Mode = "drain-only"
)

// Lifetime identifies the Worker-side expiry rule for one authorization.
type Lifetime string

const (
	LifetimeTTL      Lifetime = "ttl"
	LifetimeLiveOnly Lifetime = "live_only"
)

// WorkerSession is the immutable Worker process/session fence.
type WorkerSession struct {
	WorkerID     string
	BootID       string
	SessionEpoch uint64
}

// WorkerProcess is stable only for the lifetime of one Worker boot. Session
// Epoch changes on every accepted control registration or reconnect.
type WorkerProcess struct {
	WorkerID string
	BootID   string
}

// AssignmentIdentity contains persistent Assignment identity. Discovery Epoch
// is intentionally separate so a new Leader can refresh the same Assignment.
type AssignmentIdentity struct {
	Worker               WorkerSession
	AssignmentID         string
	VersionID            string
	AdmissionEpoch       uint64
	DeploymentGeneration uint64
	PolicyDigest         digest.SHA256
	Mode                 Mode
}

// InvocationFence is carried by a Worker invocation RPC.
type InvocationFence struct {
	Assignment     AssignmentIdentity
	DiscoveryEpoch uint64
}

// Authorization is one independently refreshable Assignment permission.
type Authorization struct {
	Fence    InvocationFence
	Lifetime Lifetime
	TTL      time.Duration
}

// ControlConnection identifies one authenticated Leader-to-Worker connection.
type ControlConnection struct {
	ConnectionID   string
	SessionEpoch   uint64
	DiscoveryEpoch uint64
}

func (p WorkerProcess) validate() error {
	if !identifierPattern.MatchString(p.WorkerID) {
		return problem.Invalid("worker_id", "must be a valid identifier")
	}
	if !identifierPattern.MatchString(p.BootID) {
		return problem.Invalid("boot_id", "must be a valid identifier")
	}
	return nil
}

func (s WorkerSession) validate() error {
	if !identifierPattern.MatchString(s.WorkerID) {
		return problem.Invalid("worker_id", "must be a valid identifier")
	}
	if !identifierPattern.MatchString(s.BootID) {
		return problem.Invalid("boot_id", "must be a valid identifier")
	}
	if s.SessionEpoch == 0 {
		return problem.Invalid("session_epoch", "must be greater than zero")
	}
	return nil
}

func (a AssignmentIdentity) validate() error {
	if err := a.Worker.validate(); err != nil {
		return err
	}
	if !identifierPattern.MatchString(a.AssignmentID) {
		return problem.Invalid("assignment_id", "must be a valid identifier")
	}
	if !identifierPattern.MatchString(a.VersionID) {
		return problem.Invalid("version_id", "must be a valid identifier")
	}
	if a.AdmissionEpoch == 0 {
		return problem.Invalid("admission_epoch", "must be greater than zero")
	}
	if a.DeploymentGeneration == 0 {
		return problem.Invalid("deployment_generation", "must be greater than zero")
	}
	if _, err := digest.ParseSHA256(a.PolicyDigest.String()); err != nil {
		return problem.Invalid("policy_digest", "must be a lowercase sha-256 digest")
	}
	if a.Mode != ModeNormal && a.Mode != ModeDrainOnly {
		return problem.Invalid("assignment_mode", "must be normal or drain-only")
	}
	return nil
}

func (f InvocationFence) validate() error {
	if err := f.Assignment.validate(); err != nil {
		return err
	}
	if f.DiscoveryEpoch == 0 {
		return problem.Invalid("discovery_epoch", "must be greater than zero")
	}
	return nil
}

func (a Authorization) validate(maxTTL time.Duration) error {
	if err := a.Fence.validate(); err != nil {
		return err
	}
	switch a.Lifetime {
	case LifetimeTTL:
		if a.TTL <= 0 || a.TTL > maxTTL {
			return problem.Invalid("authorization_ttl", "must be positive and within the worker maximum")
		}
	case LifetimeLiveOnly:
		if a.TTL != 0 {
			return problem.Invalid("authorization_ttl", "must be zero for live-only authorization")
		}
	default:
		return problem.Invalid("authorization_lifetime", "must be ttl or live_only")
	}
	return nil
}

func (c ControlConnection) validate() error {
	if !identifierPattern.MatchString(c.ConnectionID) {
		return problem.Invalid("control_connection_id", "must be a valid identifier")
	}
	if c.SessionEpoch == 0 {
		return problem.Invalid("session_epoch", "must be greater than zero")
	}
	if c.DiscoveryEpoch == 0 {
		return problem.Invalid("discovery_epoch", "must be greater than zero")
	}
	return nil
}
