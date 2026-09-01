# Design rationale

This document records the decisions that shaped SaveToA before implementation.
It explains the intended product boundary and avoids tying the engine to any
one deployment.

## Product shape

- SaveToA is one extensible Go CLI, built as a static binary where practical.
- A target is a named job that selects its capture driver, source settings,
  transforms, destinations, and retention policy. A driver is not a target.
- Native database tools perform capture and restore; SaveToA validates source
  health and coordinates the surrounding workflow.
- Systemd owns scheduling and supervision. Configuration management owns
  target files, secrets, host permissions, schedules, and alert wiring.
- Shell orchestration is not the engine because the workflow needs explicit
  health gates, cancellation, manifests, fan-out, retention, and restore
  safety.

## Evaluated building blocks

- Restic and Kopia provide strong encrypted repositories, but they do not
  understand database replica health or native capture semantics. Their
  repository model is therefore better treated as a possible destination than
  as the orchestration layer.
- Rclone is a transport rather than a database backup orchestrator.
- WAL-G remains a possible future adapter, but its database integrations do
  not currently provide one uniform contract for the planned drivers.
- GoBackup was rejected after source review because its process and credential
  handling did not meet this project's safety contract, and its database
  capture behavior did not cover the required replica checks and preparation.

These choices should be revisited only when requirements change materially or
an upstream project gains capabilities that alter the comparison.

## Artifact and delivery choices

- Capture and delivery are separate phases. A failed remote delivery can retry
  the same durable artifact without capturing a different database state.
- Backup sets are immutable, uniquely named, and explicitly versioned.
- A manifest records non-secret capture and restore metadata.
- A durable completion marker is written last. Restore and retention ignore
  incomplete sets.
- Checksums cover the stored representation, including ciphertext when
  client-side encryption is enabled.
- Local filesystems and S3-compatible storage are both first-class
  destinations.

## Encryption and permissions

- Client-side encryption is optional in the product contract and explicit in
  target configuration.
- Recipient-based `age` encryption is the preferred initial transform so a
  backup runner can hold public recipients while decryption identities remain
  offline.
- Upload and destructive maintenance credentials should be separable.
- Native tool prerequisites and narrowly scoped source permissions belong to
  each deployment, not to package installation.

## Implementation order

The first implementation increment freezes the configuration and manifest
schemas, then establishes local storage, durable spool behavior, locking,
completion markers, checksums, and optional encryption. Capture drivers and
remote delivery follow in restore-tested increments rather than being added
simultaneously.
