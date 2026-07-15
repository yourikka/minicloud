package workeragent

import (
	"time"

	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/servingauth"
	"github.com/yourikka/minicloud/internal/wasmexec"
	"github.com/yourikka/minicloud/internal/workercache"
	abi "github.com/yourikka/minicloud/sdk/go/minicloudabi"
)

const (
	DefaultMaxAssignments      = 4096
	HardMaxAssignments         = 65536
	DefaultMaxConcurrent       = 4
	DefaultMaxQueued           = 64
	DefaultMaxQueuedPerReplica = 16
	DefaultPrepareTimeout      = 30 * time.Second
	HardMaxConcurrent          = 64
	HardMaxQueued              = 4096
	HardMaxQueuedPerReplica    = 4096
	HardMaxPrepareTimeout      = 5 * time.Minute
)

// ReplicaState is the Worker-observed state of one Assignment.
type ReplicaState string

const (
	ReplicaAssigned   ReplicaState = "Assigned"
	ReplicaFetching   ReplicaState = "Fetching"
	ReplicaValidating ReplicaState = "Validating"
	ReplicaCompiling  ReplicaState = "Compiling"
	ReplicaReady      ReplicaState = "Ready"
	ReplicaDraining   ReplicaState = "Draining"
	ReplicaStopped    ReplicaState = "Stopped"
	ReplicaFailed     ReplicaState = "Failed"
	ReplicaLost       ReplicaState = "Lost"
)

// Config defines one boot-local Worker Agent and its bounded admission queues.
// The Agent owns Cache and closes it after every Replica lease has drained.
type Config struct {
	Cache               *workercache.Cache
	Authorization       servingauth.Config
	MaxAssignments      int
	MaxConcurrent       int
	MaxQueued           int
	MaxQueuedPerReplica int
	PrepareTimeout      time.Duration
}

// PrepareRequest atomically binds trusted Assignment identity to the artifact
// and effective policy that the Worker will execute.
type PrepareRequest struct {
	Connection servingauth.ControlConnection
	Fence      servingauth.InvocationFence
	Module     workercache.ModuleSpec
	Policy     model.EffectivePolicy
}

// CancelRequest fences one exact Assignment on the current control session.
type CancelRequest struct {
	Connection servingauth.ControlConnection
	Fence      servingauth.InvocationFence
}

// Observation is a defensive copy of one Replica's local actual state.
type Observation struct {
	Identity          servingauth.AssignmentIdentity
	Module            workercache.ModuleSpec
	State             ReplicaState
	Failure           *model.SafeError
	Load              workercache.Result
	ActiveInvocations int
	QueuedInvocations int
}

// Inventory is one bounded, internally consistent Worker Agent snapshot.
type Inventory struct {
	Revision      uint64
	Replicas      []Observation
	Cache         workercache.Stats
	Authorization servingauth.Snapshot
	Closing       bool
	Closed        bool
}

func (s ReplicaState) terminal() bool {
	return s == ReplicaStopped || s == ReplicaFailed || s == ReplicaLost
}

func validReplicaTransition(previous, next ReplicaState) bool {
	switch previous {
	case ReplicaAssigned:
		return next == ReplicaFetching || next == ReplicaDraining || next == ReplicaLost
	case ReplicaFetching:
		return next == ReplicaValidating || next == ReplicaDraining ||
			next == ReplicaFailed || next == ReplicaLost
	case ReplicaValidating:
		return next == ReplicaCompiling || next == ReplicaDraining ||
			next == ReplicaFailed || next == ReplicaLost
	case ReplicaCompiling:
		return next == ReplicaReady || next == ReplicaDraining ||
			next == ReplicaFailed || next == ReplicaLost
	case ReplicaReady:
		return next == ReplicaDraining || next == ReplicaLost
	case ReplicaDraining:
		return next == ReplicaStopped || next == ReplicaLost
	default:
		return false
	}
}

func invocationTimeout(requested time.Duration, policy time.Duration) (time.Duration, error) {
	if requested < 0 {
		return 0, classifiedInvalid("timeout", "must not be negative")
	}
	if requested > 0 && requested < time.Millisecond {
		return 0, classifiedInvalid("timeout", "must be at least one millisecond")
	}
	if requested == 0 || requested > policy {
		return policy, nil
	}
	return requested, nil
}

func runtimeInvocationPolicy(
	policy model.EffectivePolicy,
	requestedTimeout time.Duration,
) (wasmexec.InvocationPolicy, error) {
	timeout, err := invocationTimeout(requestedTimeout, policy.ResourceLimits.Timeout)
	if err != nil {
		return wasmexec.InvocationPolicy{}, err
	}
	return wasmexec.InvocationPolicy{
		Timeout: timeout,
		RequestLimits: abi.Limits{
			RawEnvelopeBytes: responseEnvelopeBytes(policy.ResourceLimits.MaxInputBytes),
			BodyBytes:        int(policy.ResourceLimits.MaxInputBytes),
		},
		ResponseLimits: abi.Limits{
			RawEnvelopeBytes: responseEnvelopeBytes(policy.ResourceLimits.MaxOutputBytes),
			BodyBytes:        int(policy.ResourceLimits.MaxOutputBytes),
		},
		MaxLogBytes: int(policy.ResourceLimits.MaxLogBytes),
	}, nil
}

func responseEnvelopeBytes(bodyBytes int64) int64 {
	base64Bytes := 4 * ((bodyBytes + 2) / 3)
	bound := int64(abi.DefaultMetadataBytes) + base64Bytes + 1024
	return min(bound, abi.DefaultRawEnvelopeBytes)
}
