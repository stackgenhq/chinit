// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package fail classifies errors as Retryable or Fatal for the chinit retry
// engine. Fatal errors abort immediately without retrying, whereas other
// errors are retried according to the active step policy.
package fail
