# Changelog

All notable changes to SaveToA will be documented here. The project follows
Semantic Versioning after its first stable release.

## Unreleased

### Added

- Initial Go CLI scaffold.
- Strict config v1 and manifest v1 parsing and validation contracts.
- Durable local backup-set writes with manifest-bound completion markers.
- Cross-process target locks and idempotent spool-to-local delivery.
- Streaming X25519 age encryption with non-secret recipient fingerprints.
- Debian packaging, systemd templates, sysusers, and tmpfiles manifests.
- Architecture, design rationale, security, configuration, and roadmap docs.
