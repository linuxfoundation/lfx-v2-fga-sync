// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package main provides the fga-sync service entry point and supporting types.
package main

import (
	"errors"
	"fmt"
)

// terminalValidationError marks an error as a proven, locally detected
// validation failure that must terminate JetStream redelivery (TERM) rather
// than being retried as transient.
type terminalValidationError struct {
	err error
}

func (e *terminalValidationError) Error() string {
	return e.err.Error()
}

func (e *terminalValidationError) Unwrap() error {
	return e.err
}

// newTerminalValidationError wraps err so isTerminalValidationError reports true.
func newTerminalValidationError(err error) error {
	return &terminalValidationError{err: err}
}

// isTerminalValidationError reports whether err (or a wrapped cause) was
// produced by newTerminalValidationError.
func isTerminalValidationError(err error) bool {
	var terminalErr *terminalValidationError
	return errors.As(err, &terminalErr)
}

// safeErrorType returns a sanitized, low-cardinality string describing err's
// classification, suitable for logs and trace attributes without leaking
// payload contents.
func safeErrorType(err error) string {
	var statusErr fgaStatusCoder
	if errors.As(err, &statusErr) {
		return fmt.Sprintf("openfga_http_%d", statusErr.ResponseStatusCode())
	}
	if isTerminalValidationError(err) {
		return "terminal_validation"
	}
	return fmt.Sprintf("%T", err)
}
