# Roadmap

Development should proceed through short restore-tested increments.

1. [x] Freeze the versioned config and manifest schemas.
2. [x] Implement local destination, durable spool, locking, completion markers,
   checksums, and optional age encryption.
3. [x] Extend the implemented MariaDB capture driver with materialization into a
   separate datadir.
4. [x] Complete a disposable MariaDB restore cycle from a SaveToA artifact.
5. [x] Implement idempotent S3 delivery of an existing local backup ID.
6. [x] Recover a completed S3 backup into the durable local spool for restore.
7. [x] Implement MongoDB full archive plus oplog capture and disposable restore replay.
8. [x] Implement Redis BGSAVE capture and RDB restore verification.
9. [x] Implement retention with a distinct maintenance permission boundary.
10. [x] Add deterministic structured repository status output.
11. [ ] Connect status freshness and completeness checks to monitoring alerts.
12. [x] Add optional root-managed lifecycle event hooks for external telemetry.
13. [x] Add generic tar capture for filesystem and ACME state.
14. [x] Add consistent SQLite online-backup capture.
15. [x] Add PostgreSQL physical standby and single-database logical capture,
    with disposable restore smoke for both formats.
16. [x] Add single-database MariaDB logical capture with disposable restore smoke.
17. [x] Design Garage metadata backup and generic disk/S3 store migration as
    separate workflows, without claiming a live whole-store snapshot.
18. [ ] Implement and restore-test Garage metadata snapshot capture.
19. [ ] Implement resumable store migration, starting with disk-to-disk and
    extending to disk-to-S3 and S3-to-S3.

Full/incremental optimization follows measured data growth. Correct,
restore-tested full backups are preferable to an early complex incremental
chain.
