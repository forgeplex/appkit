package refs

import (
	"net/http"

	"github.com/forgeplex/appkit/apperr"
)

// Stable error codes work with apperr.Is and survive the contract boundary.
const (
	CodeInvalidSchema      = "REFS_SCHEMA_INVALID"
	CodeInvalidValues      = "REFS_VALUES_INVALID"
	CodeUnknownReference   = "REFS_UNKNOWN"
	CodeRequiredReference  = "REFS_REQUIRED"
	CodeInvalidID          = "REFS_ID_INVALID"
	CodeImmutableReference = "REFS_IMMUTABLE"
)

func schemaError(message string) *apperr.Error {
	return apperr.New(CodeInvalidSchema, http.StatusInternalServerError, message)
}

func valueError(message string) *apperr.Error {
	return apperr.New(CodeInvalidValues, http.StatusUnprocessableEntity, message)
}

func referenceError(code, message, key string) *apperr.Error {
	status := http.StatusUnprocessableEntity
	if code == CodeImmutableReference {
		status = http.StatusConflict
	}
	return apperr.New(code, status, message).WithDetail("key", key)
}
