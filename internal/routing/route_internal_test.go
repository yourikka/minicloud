package routing

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"testing"

	"github.com/yourikka/minicloud/internal/model"
)

func TestBucketFromHashUsesMultiplyHighBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		u      uint64
		bucket uint32
	}{
		{name: "zero", u: 0, bucket: 0},
		{name: "half", u: 1 << 63, bucket: 5000},
		{name: "first nonzero bucket", u: math.MaxUint64/10_000 + 1, bucket: 1},
		{name: "maximum", u: math.MaxUint64, bucket: 9999},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var hash [sha256.Size]byte
			binary.BigEndian.PutUint64(hash[:8], tt.u)
			if got := bucketFromHash(hash); got != tt.bucket {
				t.Fatalf("bucketFromHash(%d) = %d, want %d", tt.u, got, tt.bucket)
			}
		})
	}
}

func TestTargetForBucketUsesHalfOpenIntervals(t *testing.T) {
	t.Parallel()
	targets := []model.RouteTarget{
		{VersionID: "ver_a", WeightBasisPoints: 4500},
		{VersionID: "ver_b", WeightBasisPoints: 3000},
		{VersionID: "ver_c", WeightBasisPoints: 2500},
	}
	tests := []struct {
		name    string
		bucket  uint32
		version string
	}{
		{name: "first start", bucket: 0, version: "ver_a"},
		{name: "first end", bucket: 4499, version: "ver_a"},
		{name: "second start", bucket: 4500, version: "ver_b"},
		{name: "second end", bucket: 7499, version: "ver_b"},
		{name: "third start", bucket: 7500, version: "ver_c"},
		{name: "last bucket", bucket: 9999, version: "ver_c"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			target, exists := targetForBucket(targets, tt.bucket)
			if !exists || target.VersionID != tt.version {
				t.Fatalf("targetForBucket(%d) = (%q, %t), want %q", tt.bucket, target.VersionID, exists, tt.version)
			}
		})
	}
	if _, exists := targetForBucket(targets, model.TotalRouteWeightBasisPoints); exists {
		t.Fatal("targetForBucket() accepted a bucket outside 0..9999")
	}
}
