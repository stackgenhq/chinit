<!--
  Copyright (C) 2026 StackGen, Inc. All rights reserved.
  SPDX-License-Identifier: Apache-2.0
-->

# Contributing to chinit

Thanks for helping make ClickHouse on cloud VMs reliable and easy to operate.

`chinit` is a single static Go binary that replaces cloud-init shell scripts for stateful ClickHouse clusters running on EC2 Auto Scaling Groups or Azure Virtual Machine Scale Sets. It detects the cloud environment, downloads and verifies a configuration bundle, converges the node to a declarative `BootSpec`, and runs recurring maintenance tasks as subcommands.

## Ways to Contribute

- **Report bugs**: Open an [issue](https://github.com/stackgenhq/chinit/issues) with the `chinit version` output, cloud provider, node role, and relevant boot or maintenance logs.
- **Propose features**: Open an issue describing the use case before writing code so we can align on the design first.
- **Add a cloud provider**: Implement the interfaces in `pkg/chinit/domain/port` under a new `pkg/chinit/infra/<cloud>` package (or in your own Go module) and register it in the provider factory (`port.RegisterProvider`). The provider conformance suite in `pkg/chinit/domain/service/conformance` verifies the implementation.
- **Improve documentation**: Updates to the [Operator's Guide](./docs/chinit.md) and [Node Boot Procedure](./images/BOOT.md) are always welcome. The [GitHub Pages docs site](https://stackgenhq.github.io/chinit/) is built from [`docs/`](./docs) — preview changes locally exactly as GitHub Pages builds them:
  ```bash
  cd docs
  bundle install
  bundle exec jekyll serve --baseurl ""   # http://localhost:4000
  ```

## Development Workflow

```bash
git clone https://github.com/stackgenhq/chinit
cd chinit
go build ./...
go vet ./...
go test ./...
golangci-lint run ./...
```

Linting runs through [golangci-lint](https://golangci-lint.run) with the repo's [.golangci.yaml](./.golangci.yaml) (the same config CI enforces). Tests are written with Ginkgo v2 and Gomega. Every bug fix or new feature should include tests that cover the change.

### Architectural Guidelines

- **Domain independence**: `pkg/chinit/domain/` must compile without cloud SDKs or Linux-specific dependencies. Dependencies point inward toward `domain/`.
- **Port interfaces**: Cloud-specific operations belong in `pkg/chinit/infra/<cloud>` behind `domain/port` interfaces. Optional cloud capabilities should degrade with a clear warning rather than failing silently.
- **Library & CLI extensibility**: Core packages live under `pkg/chinit/` so external Go projects can import the domain models, ports, services, and conformance suite to extend `chinit` or register additional cloud providers. Release asset names (`chinit-<os>-<arch>` and `chinit-SHA256SUMS`) must also remain stable because Packer builds and bootstrap scripts depend on them.

See [docs/chinit-architecture.md](./docs/chinit-architecture.md) for a detailed overview of the package structure and design decisions.

## Submitting Pull Requests

1. Fork the repository, create a topic branch (`fix/...`, `feat/...`, `docs/...`), and use Conventional Commits (`fix(boot): ...`, `feat(aws): ...`).
2. Open a pull request against `main`. Make sure `go build ./...`, `go vet ./...`, `go test ./...`, and `golangci-lint run ./...` pass.
3. Complete the pull request checklist:
   - [ ] `go test ./...`, `go vet ./...`, and `golangci-lint run ./...` pass
   - [ ] New or changed behavior is covered by unit or conformance tests
   - [ ] No cloud SDK imports outside `pkg/chinit/infra/<cloud>`
   - [ ] Errors are classified as `Retryable` vs. `Fatal` appropriately
   - [ ] User-visible changes are documented in [docs/](./docs)

## Releases

- Pushing a `v0.*` tag triggers `.github/workflows/release-chinit.yaml`, which builds static binaries (`linux`/`darwin` × `arm64`/`amd64`) and `chinit-SHA256SUMS` via GoReleaser.
- Golden image definitions in `images/` pin a specific release version and verify the binary's SHA-256 checksum during image build.

## License

By contributing to `chinit`, you agree that your contributions will be licensed under the [Apache License 2.0](./LICENSE).
