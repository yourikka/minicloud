package strictjson_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/yourikka/minicloud/internal/strictjson"
)

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		depth   int
		wantErr error
	}{
		{name: "valid", input: `{"a":[1,{"b":true}]}`, depth: 3},
		{name: "duplicate root key", input: `{"a":1,"a":2}`, depth: 32, wantErr: strictjson.ErrDuplicateKey},
		{name: "duplicate nested key", input: `{"a":{"b":1,"b":2}}`, depth: 32, wantErr: strictjson.ErrDuplicateKey},
		{name: "depth exceeded", input: `[[[0]]]`, depth: 2, wantErr: strictjson.ErrTooDeep},
		{name: "multiple values", input: `{} {}`, depth: 32, wantErr: errors.New("multiple")},
		{name: "truncated", input: `{"a":`, depth: 32, wantErr: errors.New("truncated")},
		{name: "invalid utf8", input: string([]byte{'"', 0xff, '"'}), depth: 32, wantErr: errors.New("utf8")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := strictjson.Validate([]byte(tt.input), tt.depth)
			if tt.wantErr == nil && err != nil {
				t.Fatalf("Validate() unexpected error: %v", err)
			}
			if tt.wantErr != nil && err == nil {
				t.Fatal("Validate() error = nil, want error")
			}
			if errors.Is(tt.wantErr, strictjson.ErrDuplicateKey) && !errors.Is(err, strictjson.ErrDuplicateKey) {
				t.Fatalf("Validate() error = %v, want duplicate key", err)
			}
			if errors.Is(tt.wantErr, strictjson.ErrTooDeep) && !errors.Is(err, strictjson.ErrTooDeep) {
				t.Fatalf("Validate() error = %v, want depth error", err)
			}
		})
	}
}

func TestReadStopsAtLimitPlusOne(t *testing.T) {
	t.Parallel()

	source := &countingReader{Reader: strings.NewReader(strings.Repeat("x", 64))}
	_, err := strictjson.Read(source, 8)
	if !errors.Is(err, strictjson.ErrTooLarge) {
		t.Fatalf("Read() error = %v, want ErrTooLarge", err)
	}
	if source.count != 9 {
		t.Fatalf("Read() consumed %d bytes, want 9", source.count)
	}
}

type countingReader struct {
	*strings.Reader
	count int
}

func (r *countingReader) Read(buffer []byte) (int, error) {
	read, err := r.Reader.Read(buffer)
	r.count += read
	return read, err
}
