// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package fail

import (
	"errors"
	"fmt"
)

// FatalError marks an operation whose failure must not be retried:
// IAM AccessDenied, a SHA256 mismatch, a missing golden-AMI binary.
// Retrying masks a config bug and burns the boot window.
type FatalError struct {
	// Step, when set, is the failing step name (filled by the engine).
	Step string
	Err  error
}

func (e *FatalError) Error() string {
	if e.Step != "" {
		return fmt.Sprintf("step %s: fatal: %s", e.Step, e.Err)
	}
	return "fatal: " + e.Err.Error()
}

func (e *FatalError) Unwrap() error { return e.Err }

// Fatal wraps err as a FatalError. nil maps to nil.
func Fatal(err error) error {
	if err == nil {
		return nil
	}
	var fe *FatalError
	if errors.As(err, &fe) {
		return err
	}
	return &FatalError{Err: err}
}

// IsFatal reports whether err (or anything in its chain) is a FatalError.
func IsFatal(err error) bool {
	var fe *FatalError
	return errors.As(err, &fe)
}
