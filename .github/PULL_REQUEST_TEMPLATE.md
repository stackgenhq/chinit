<!--
  Copyright (C) 2026 StackGen, Inc. All rights reserved.
  SPDX-License-Identifier: Apache-2.0
-->

## Summary

<!-- Briefly describe what this pull request changes and why. -->

## Checklist

- [ ] `go test ./...` passes
- [ ] `go vet ./...` passes
- [ ] New or changed behavior is covered by tests
- [ ] No cloud SDK imports outside `pkg/chinit/infra/<cloud>`
- [ ] No `domain/` dependencies on `infra/`
- [ ] Error paths are classified as `Retryable` vs. `Fatal` appropriately
- [ ] Release asset names and checksum verification remain unchanged
- [ ] User-visible changes are documented in `docs/` and `images/BOOT.md`
