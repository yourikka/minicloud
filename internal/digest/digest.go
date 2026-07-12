// Package digest implements versioned, domain-separated protocol digests.
package digest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"

	"github.com/gowebpki/jcs"
)

const prefix = "minicloud\x00"

var encodedPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// SHA256 is a lowercase, self-describing SHA-256 digest.
type SHA256 string

// ParseSHA256 validates and returns an encoded digest.
func ParseSHA256(value string) (SHA256, error) {
	if !encodedPattern.MatchString(value) {
		return "", errors.New("invalid sha256 digest")
	}
	return SHA256(value), nil
}

// Sum hashes opaque bytes without protocol domain separation. It is intended
// for content-addressed artifacts, whose identity is exactly their bytes.
func Sum(data []byte) SHA256 {
	sum := sha256.Sum256(data)
	return SHA256("sha256:" + hex.EncodeToString(sum[:]))
}

// CanonicalJSON hashes RFC 8785 canonical JSON in a versioned protocol domain.
func CanonicalJSON(domain, schemaVersion string, source []byte) (SHA256, error) {
	if domain == "" {
		return "", errors.New("digest domain is required")
	}
	if schemaVersion == "" {
		return "", errors.New("digest schema version is required")
	}

	canonical, err := jcs.Transform(source)
	if err != nil {
		return "", fmt.Errorf("canonicalizing json: %w", err)
	}

	preimage := make([]byte, 0, len(prefix)+len(domain)+len(schemaVersion)+len(canonical)+2)
	preimage = append(preimage, prefix...)
	preimage = append(preimage, domain...)
	preimage = append(preimage, 0)
	preimage = append(preimage, schemaVersion...)
	preimage = append(preimage, 0)
	preimage = append(preimage, canonical...)

	return Sum(preimage), nil
}

func (d SHA256) String() string {
	return string(d)
}
