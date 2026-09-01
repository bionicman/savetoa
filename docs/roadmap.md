# Roadmap

Development should proceed through short restore-tested increments.

1. Freeze the versioned config and manifest schemas.
2. Implement local destination, durable spool, locking, completion markers,
   checksums, and optional age encryption.
3. Implement the MariaDB driver and materialization into a separate datadir.
4. Complete a real MariaDB backup and disposable restore cycle.
5. Implement idempotent S3 delivery of an existing local backup ID.
6. Implement MongoDB full archive plus oplog capture and restore replay.
7. Implement Redis BGSAVE capture and RDB restore verification.
8. Implement retention with a distinct maintenance permission boundary.
9. Add structured status output and monitoring integration.
10. Add files/ACME capture.
11. Design Garage metadata backup and S3-to-S3 migration as separate drivers.

Full/incremental optimization follows measured data growth. Correct,
restore-tested full backups are preferable to an early complex incremental
chain.
