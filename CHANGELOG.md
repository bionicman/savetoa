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
- Read-only MariaDB doctor checks for replica identity, GTID, lag and matching
  server/backup-tool versions.
- End-to-end MariaDB `run` with safe replica capture, guaranteed SQL-thread
  resume attempts, preparation validation, current and legacy metadata support,
  tar/zstd streaming, durable spool, and verified local fan-out.
- Safe archive traversal that rejects symlinks and non-regular capture entries.
- Verified local restore with reverse age/zstd transforms and traversal-safe tar
  materialization into an explicit new directory.
- Idempotent S3-compatible delivery from the durable spool, with conditional
  writes, byte-verified retries, conflict refusal, and completion-last publication.
- Verified S3 recovery into an atomically published durable spool set, with
  bounded metadata, canonical-manifest, identity, size, and checksum checks.
- MongoDB hidden-secondary health gates, full archive+oplog capture through the
  common pipeline, and replay verification in a disposable local mongod.
- Redis read-only replica health gates, fresh BGSAVE/RDB capture through the
  common pipeline, and load verification in a disposable local redis-server.
- Fail-closed GFS retention across the spool, local destinations, and S3, with
  marker-first deletion and separate maintenance credentials and execution identity.
- Deterministic text and JSON repository inventory with monitoring exit codes
  for empty, degraded, stale, and clock-skewed latest backup sets.
- Disabled hardened systemd status templates with operator-owned per-target
  freshness thresholds.
- Optional bounded root-managed lifecycle hooks with versioned non-secret JSON
  events and best-effort outcome semantics.
- Debian packaging, systemd templates, sysusers, and tmpfiles manifests.
- Architecture, design rationale, security, configuration, and roadmap docs.

### Changed

- Set Go 1.27.1 as the project toolchain and update Go modules and CI actions.
