package problem_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/yourikka/minicloud/internal/problem"
)

func TestHTTPStatusCoversStableCatalog(t *testing.T) {
	t.Parallel()

	codes := problem.Codes()
	if got, want := len(codes), 28; got != want {
		t.Fatalf("stable code count = %d, want %d", got, want)
	}
	for _, code := range codes {
		if got := problem.HTTPStatus(code); got == http.StatusInternalServerError {
			t.Errorf("HTTPStatus(%q) is not classified", code)
		}
	}
}

func TestNewEnvelope(t *testing.T) {
	t.Parallel()

	classified := &problem.Error{
		Code:    problem.CodeRevisionConflict,
		Message: "resource changed",
		Field:   "resource_revision",
	}
	envelope := problem.NewEnvelope(
		classified,
		"req_01",
		map[string]any{"expected": uint64(17), "actual": uint64(18)},
	)

	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("json.Marshal(): %v", err)
	}
	want := `{"error":{"code":"revision_conflict","message":"resource changed","request_id":"req_01","details":{"actual":18,"expected":17,"field":"resource_revision"}}}`
	if string(encoded) != want {
		t.Fatalf("encoded envelope = %s, want %s", encoded, want)
	}
}
