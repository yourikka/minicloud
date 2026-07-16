// Package routing implements the deterministic v1 weighted Route selector.
package routing

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"math/bits"
	"slices"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
)

const lengthPrefixBytes = 4

// Decision is the reproducible result of selecting one Route Target.
type Decision struct {
	Digest digest.SHA256
	Bucket uint32
	Target model.RouteTarget
}

// Select validates an enabled Route and deterministically selects one Target
// from its cumulative basis-point intervals. affinityKey is treated as raw
// bytes; callers are responsible for deriving it from the configured affinity
// source.
func Select(route model.Route, affinityKey []byte) (Decision, error) {
	if err := route.Validate(); err != nil {
		return Decision{}, err
	}
	if !route.Enabled {
		return Decision{}, &problem.Error{
			Code:    problem.CodeFunctionDisabled,
			Message: "route is disabled",
		}
	}
	preimage, err := hashPreimage(route, affinityKey)
	if err != nil {
		return Decision{}, err
	}
	hash := sha256.Sum256(preimage)
	digestValue := digest.SHA256("sha256:" + hex.EncodeToString(hash[:]))
	bucket := bucketFromHash(hash)

	targets := slices.Clone(route.Targets)
	slices.SortFunc(targets, compareTargets)
	target, exists := targetForBucket(targets, bucket)
	if exists {
		return Decision{Digest: digestValue, Bucket: bucket, Target: target}, nil
	}

	// Route.Validate guarantees a non-empty enabled route whose weights total
	// exactly 10000, so reaching this point indicates an internal invariant bug.
	return Decision{}, errors.New("routing: validated target intervals do not cover bucket")
}

func hashPreimage(route model.Route, affinityKey []byte) ([]byte, error) {
	revision := make([]byte, 8)
	binary.BigEndian.PutUint64(revision, route.RouteRevision)
	fields := [][]byte{
		[]byte(model.HashVersionSHA256BPSV1),
		[]byte(route.FunctionID),
		revision,
		route.Salt,
		affinityKey,
	}
	var totalBytes uint64
	for _, field := range fields {
		if uint64(len(field)) > math.MaxUint32 {
			return nil, problem.Invalid("route_hash_input", "field exceeds the v1 length-prefix limit")
		}
		totalBytes += lengthPrefixBytes + uint64(len(field))
	}
	if totalBytes > uint64(math.MaxInt) {
		return nil, problem.Invalid("route_hash_input", "fields exceed the platform input limit")
	}
	preimage := make([]byte, 0, int(totalBytes))
	for _, field := range fields {
		var prefix [lengthPrefixBytes]byte
		binary.BigEndian.PutUint32(prefix[:], uint32(len(field)))
		preimage = append(preimage, prefix[:]...)
		preimage = append(preimage, field...)
	}
	return preimage, nil
}

func bucketFromHash(hash [sha256.Size]byte) uint32 {
	u := binary.BigEndian.Uint64(hash[:8])
	hi, _ := bits.Mul64(u, uint64(model.TotalRouteWeightBasisPoints))
	return uint32(hi)
}

func targetForBucket(targets []model.RouteTarget, bucket uint32) (model.RouteTarget, bool) {
	var cumulative uint32
	for _, target := range targets {
		cumulative += target.WeightBasisPoints
		if bucket < cumulative {
			return target, true
		}
	}
	return model.RouteTarget{}, false
}

func compareTargets(left, right model.RouteTarget) int {
	if left.VersionID < right.VersionID {
		return -1
	}
	if left.VersionID > right.VersionID {
		return 1
	}
	if left.DeploymentGeneration < right.DeploymentGeneration {
		return -1
	}
	if left.DeploymentGeneration > right.DeploymentGeneration {
		return 1
	}
	return 0
}
