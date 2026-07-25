package discovery

import (
	"errors"
	"math"
	"sync"
)

// Publisher owns the global Serving Sequence for one Discovery Epoch. It does
// not establish Leader or quorum authority; callers must prove that first.
type Publisher struct {
	builder *Builder

	mu       sync.Mutex
	epoch    uint64
	sequence uint64
}

// NewPublisher creates a sequence allocator for a positive committed
// Discovery Epoch.
func NewPublisher(epoch uint64, builder *Builder) (*Publisher, error) {
	if epoch == 0 {
		return nil, errors.New("discovery publisher epoch must be greater than zero")
	}
	if builder == nil {
		return nil, errors.New("discovery publisher builder is required")
	}
	return &Publisher{builder: builder, epoch: epoch}, nil
}

// Build assigns the next global Sequence and builds one complete Function
// snapshot. Input Epoch/Sequence are overwritten so callers cannot create
// duplicate or per-Function sequence spaces.
func (p *Publisher) Build(input Input) (Result, error) {
	if p == nil {
		return Result{}, errors.New("discovery publisher is nil")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sequence == math.MaxUint64 {
		return Result{}, errors.New("discovery serving sequence is exhausted; a new epoch is required")
	}
	next := p.sequence + 1
	input.DiscoveryEpoch = p.epoch
	input.ServingSequence = next
	result, err := p.builder.Build(input)
	if err != nil {
		return Result{}, err
	}
	p.sequence = next
	return result, nil
}

// BuildFull assigns one global Sequence to every Function in an atomic Full
// Sync batch. Empty batches are valid and still consume a Sequence.
func (p *Publisher) BuildFull(inputs []Input) ([]Result, uint64, error) {
	if p == nil {
		return nil, 0, errors.New("discovery publisher is nil")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sequence == math.MaxUint64 {
		return nil, 0, errors.New("discovery serving sequence is exhausted; a new epoch is required")
	}
	next := p.sequence + 1
	results := make([]Result, 0, len(inputs))
	seen := make(map[string]struct{}, len(inputs))
	for _, input := range inputs {
		if _, exists := seen[input.Function.ID]; exists {
			return nil, 0, errors.New("discovery full sync contains a duplicate function id")
		}
		seen[input.Function.ID] = struct{}{}
		input.DiscoveryEpoch = p.epoch
		input.ServingSequence = next
		result, err := p.builder.Build(input)
		if err != nil {
			return nil, 0, err
		}
		results = append(results, result)
	}
	p.sequence = next
	return results, next, nil
}

// Position returns the immutable epoch and last successfully published global
// Sequence.
func (p *Publisher) Position() (epoch, sequence uint64) {
	if p == nil {
		return 0, 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.epoch, p.sequence
}
