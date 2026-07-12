package digest_test

import (
	"testing"

	"github.com/yourikka/minicloud/internal/digest"
)

func TestParseSHA256(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "valid", value: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		{name: "uppercase rejected", value: "sha256:0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef", wantErr: true},
		{name: "wrong algorithm", value: "sha1:0123456789abcdef0123456789abcdef01234567", wantErr: true},
		{name: "wrong length", value: "sha256:00", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := digest.ParseSHA256(tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseSHA256() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCanonicalJSON(t *testing.T) {
	t.Parallel()

	first, err := digest.CanonicalJSON("manifest", "v1", []byte(`{"b":2,"a":1}`))
	if err != nil {
		t.Fatalf("CanonicalJSON(first): %v", err)
	}
	second, err := digest.CanonicalJSON("manifest", "v1", []byte("{\n  \"a\": 1, \"b\": 2\n}"))
	if err != nil {
		t.Fatalf("CanonicalJSON(second): %v", err)
	}
	if first != second {
		t.Fatalf("equivalent JSON digests differ: %q != %q", first, second)
	}

	otherDomain, err := digest.CanonicalJSON("policy", "v1", []byte(`{"a":1,"b":2}`))
	if err != nil {
		t.Fatalf("CanonicalJSON(other domain): %v", err)
	}
	if first == otherDomain {
		t.Fatal("different digest domains produced the same digest")
	}
}

func TestCanonicalJSONRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		source string
	}{
		{name: "truncated", source: `{"a":`},
		{name: "duplicate key", source: `{"a":1,"a":2}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := digest.CanonicalJSON("manifest", "v1", []byte(tt.source)); err == nil {
				t.Fatal("CanonicalJSON() accepted invalid JSON")
			}
		})
	}
}
