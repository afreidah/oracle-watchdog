# Changelog

All notable changes to this project are documented in this file.


## [1.4.4] - 2026-06-02

### Fixed
- Fix tracing init: schemaless resource to avoid Schema URL conflict (#55)

### Improved
- update CHANGELOG.md for v1.4.3 (#54)

## [1.4.3] - 2026-06-01

### Improved
- update CHANGELOG.md for v1.4.1 (#42)

### Other
- Make tracing config-driven and fix OTLP endpoint (#52)
- build(deps): bump github.com/oracle/oci-go-sdk/v65
- build(deps): bump go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp
- build(deps): bump go.opentelemetry.io/otel/trace from 1.43.0 to 1.44.0
- build(deps): bump github.com/hashicorp/consul/api from 1.33.7 to 1.34.3
- build(deps): bump github.com/hashicorp/consul/api from 1.33.4 to 1.33.7
- build(deps): bump github.com/oracle/oci-go-sdk/v65 from 65.81.0 to 65.110.0
- build(deps): bump go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp from 1.42.0 to 1.43.0
- build(deps): bump the go_modules group across 1 directory with 2 updates
- build(deps): bump codecov/codecov-action from 5 to 6
- build(deps): bump google.golang.org/grpc
- build(deps): bump actions/github-script from 8 to 9
- clarify optional features and correct agent loop description

## [1.4.1] - 2026-05-13

### Added
- add endpoint resolver (monitor) and WAN DNS updater (agent)

### Improved
- update CHANGELOG.md for v1.3.0 (#30)

### Other
- publish-deb: target s3:munchbox: prefix instead of root

## [1.3.0] - 2026-03-16

### Added
- Add auto-generated Go API reference to documentation site

### Improved
- update CHANGELOG.md for v1.2.0 (#27)

## [1.2.0] - 2026-03-16

### Added
- Add periodic heartbeat log to monitor mode

### Improved
- update CHANGELOG.md for v1.1.0 (#25)

## [1.1.0] - 2026-03-16

### Added
- Add publish-deb target and bump version to v1.1.0

### Improved
- update CHANGELOG.md for v0.0.9 (#24)

## [0.0.9] - 2026-03-16

### Added
- Add tracing to monitor mode Consul calls

### Improved
- update CHANGELOG.md for v0.0.8 (#22)

## [0.0.8] - 2026-03-16

### Fixed
- Fix service graph visibility in Tempo and add Consul client spans

### Improved
- update readme to have logo
- update CHANGELOG.md for v0.0.7 (#18)

### Other
- Move logo above title in README and reorder header elements

## [0.0.7] - 2026-03-15

### Added
- Add Hugo documentation site

### Improved
- update CHANGELOG.md for v0.0.6 (#15)

## [0.0.6] - 2026-03-15

### Improved
- update CHANGELOG.md for v0.0.5 (#9)

### Other
- general repo housekeeping/setup

## [0.0.4] - 2026-03-15

### Other
- Test release functionality

## [0.0.3] - 2026-03-15

### Added
- Add Grafana dashboard, GoReleaser release pipeline, and fix metrics initialization
- Add README with architecture, metrics, config, and deployment docs

### Fixed
- Fix reliability issues and improve Go best practices (#1)

### Other
- Initial standalone repo with CI/CD infrastructure
- Initial commit
