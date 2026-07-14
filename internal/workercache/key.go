package workercache

import (
	"errors"
	"runtime"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/wasmprofile"
)

// ModuleSpec contains immutable Version fields used for load compatibility.
type ModuleSpec struct {
	ArtifactDigest        digest.SHA256
	ArtifactSize          int64
	ABI                   string
	HostAPIProfile        string
	RuntimeFeatureProfile string
}

// Key includes every v1 compilation compatibility input.
type Key struct {
	ArtifactDigest        digest.SHA256
	ArtifactSize          int64
	RuntimeName           string
	RuntimeVersion        string
	ABI                   string
	HostAPIProfile        string
	RuntimeFeatureProfile string
	Engine                string
	MemoryLimitMiB        uint32
	GOOS                  string
	GOARCH                string
}

// Profile binds cache keys to one real Worker Engine.
type Profile struct {
	runtime wasmprofile.Profile
}

func newProfile(runtimeProfile wasmprofile.Profile) (Profile, error) {
	validated, err := wasmprofile.New(runtimeProfile.Engine, runtimeProfile.MemoryLimitMiB)
	if err != nil {
		return Profile{}, errors.New("worker cache compiler profile is invalid")
	}
	return Profile{runtime: validated}, nil
}

func (p Profile) key(spec ModuleSpec) (Key, error) {
	if _, err := digest.ParseSHA256(spec.ArtifactDigest.String()); err != nil {
		return Key{}, errors.New("worker cache artifact digest is invalid")
	}
	if spec.ArtifactSize < 1 || spec.ArtifactSize > model.MaxArtifactBytes {
		return Key{}, errors.New("worker cache artifact size is outside v1 bounds")
	}
	if spec.ABI != model.ABIWASICommandV1 {
		return Key{}, errors.New("worker cache abi is unsupported")
	}
	if spec.HostAPIProfile != model.HostAPIProfileNone {
		return Key{}, errors.New("worker cache host api profile is unsupported")
	}
	if spec.RuntimeFeatureProfile != wasmprofile.FeatureProfile {
		return Key{}, errors.New("worker cache runtime feature profile is unsupported")
	}
	return Key{
		ArtifactDigest:        spec.ArtifactDigest,
		ArtifactSize:          spec.ArtifactSize,
		RuntimeName:           wasmprofile.RuntimeName,
		RuntimeVersion:        wasmprofile.RuntimeVersion,
		ABI:                   spec.ABI,
		HostAPIProfile:        spec.HostAPIProfile,
		RuntimeFeatureProfile: spec.RuntimeFeatureProfile,
		Engine:                p.runtime.Engine,
		MemoryLimitMiB:        p.runtime.MemoryLimitMiB,
		GOOS:                  runtime.GOOS,
		GOARCH:                runtime.GOARCH,
	}, nil
}
