// Package problem defines stable, safe error categories shared by protocol
// boundaries and deterministic domain validation.
package problem

import (
	"fmt"
	"net/http"
	"slices"
)

// Code is a stable machine-readable error category.
type Code string

const (
	CodeInvalidArgument         Code = "invalid_argument"
	CodeUnauthenticated         Code = "unauthenticated"
	CodeForbidden               Code = "forbidden"
	CodeNotFound                Code = "not_found"
	CodeConflict                Code = "conflict"
	CodeRevisionConflict        Code = "revision_conflict"
	CodeRollbackUnavailable     Code = "rollback_unavailable"
	CodeOperationUnknown        Code = "operation_unknown"
	CodeOperationExpired        Code = "operation_expired"
	CodeCredentialNotReplayable Code = "credential_not_replayable"
	CodeNoQuorum                Code = "no_quorum"
	CodeControlPlaneStale       Code = "control_plane_stale"
	CodeFunctionDisabled        Code = "function_disabled"
	CodeNoReadyReplica          Code = "no_ready_replica"
	CodeOverloaded              Code = "overloaded"
	CodeColdStartTimeout        Code = "cold_start_timeout"
	CodeFunctionTimeout         Code = "function_timeout"
	CodeInvalidModule           Code = "invalid_module"
	CodeCapabilityDenied        Code = "capability_denied"
	CodeFunctionTrap            Code = "function_trap"
	CodeInvalidFunctionResponse Code = "invalid_function_response"
	CodeOutputLimit             Code = "output_limit"
	CodeWorkerLost              Code = "worker_lost"
	CodeStaleAssignment         Code = "stale_assignment"
	CodeStaleGeneration         Code = "stale_generation"
	CodeArtifactUnavailable     Code = "artifact_unavailable"
	CodeAsyncUnavailable        Code = "async_unavailable"
	CodeResultExpired           Code = "result_expired"
)

var allCodes = []Code{
	CodeInvalidArgument,
	CodeUnauthenticated,
	CodeForbidden,
	CodeNotFound,
	CodeConflict,
	CodeRevisionConflict,
	CodeRollbackUnavailable,
	CodeOperationUnknown,
	CodeOperationExpired,
	CodeCredentialNotReplayable,
	CodeNoQuorum,
	CodeControlPlaneStale,
	CodeFunctionDisabled,
	CodeNoReadyReplica,
	CodeOverloaded,
	CodeColdStartTimeout,
	CodeFunctionTimeout,
	CodeInvalidModule,
	CodeCapabilityDenied,
	CodeFunctionTrap,
	CodeInvalidFunctionResponse,
	CodeOutputLimit,
	CodeWorkerLost,
	CodeStaleAssignment,
	CodeStaleGeneration,
	CodeArtifactUnavailable,
	CodeAsyncUnavailable,
	CodeResultExpired,
}

// Codes returns a defensive copy of the complete v1 public error catalog.
func Codes() []Code {
	return slices.Clone(allCodes)
}

// Known reports whether code belongs to the stable v1 public catalog.
func Known(code Code) bool {
	return slices.Contains(allCodes, code)
}

// Error carries safe structured context without exposing implementation errors.
type Error struct {
	Code    Code
	Message string
	Field   string
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Field == "" {
		return string(e.Code) + ": " + e.Message
	}
	return fmt.Sprintf("%s: %s: %s", e.Code, e.Field, e.Message)
}

// Invalid reports a validation failure at an external or persistence boundary.
func Invalid(field, message string) error {
	return &Error{
		Code:    CodeInvalidArgument,
		Message: message,
		Field:   field,
	}
}

// HTTPStatus returns the default HTTP status for a stable code. Callers may use
// a documented alternate status for operation_unknown and overloaded.
func HTTPStatus(code Code) int {
	switch code {
	case CodeInvalidArgument:
		return http.StatusBadRequest
	case CodeUnauthenticated:
		return http.StatusUnauthorized
	case CodeForbidden, CodeCapabilityDenied:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict, CodeRevisionConflict, CodeRollbackUnavailable,
		CodeOperationUnknown, CodeCredentialNotReplayable, CodeStaleAssignment:
		return http.StatusConflict
	case CodeOperationExpired, CodeResultExpired:
		return http.StatusGone
	case CodeOverloaded:
		return http.StatusTooManyRequests
	case CodeInvalidModule:
		return http.StatusUnprocessableEntity
	case CodeFunctionTrap, CodeInvalidFunctionResponse, CodeOutputLimit, CodeWorkerLost:
		return http.StatusBadGateway
	case CodeColdStartTimeout, CodeFunctionTimeout:
		return http.StatusGatewayTimeout
	case CodeNoQuorum, CodeControlPlaneStale, CodeFunctionDisabled,
		CodeNoReadyReplica, CodeStaleGeneration, CodeArtifactUnavailable, CodeAsyncUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
