# Status report v1

`list --format json TARGET` and `status --format json TARGET` emit one JSON
object. `schema_version` is `1`. Fields are ordered deterministically, and
repositories are sorted by name while backups are sorted newest first.

```json
{
  "schema_version": 1,
  "generated_at": "2026-09-03T12:00:00Z",
  "environment": "production",
  "target": "production-redis",
  "status": "complete",
  "latest_backup_id": "20260903t105715z-ba8bd940f25c2abd",
  "age_seconds": 3765,
  "repositories": [
    {
      "name": "offsite",
      "driver": "s3",
      "completed_sets": 1,
      "latest_backup_id": "20260903t105715z-ba8bd940f25c2abd"
    },
    {
      "name": "spool",
      "driver": "local",
      "completed_sets": 1,
      "latest_backup_id": "20260903t105715z-ba8bd940f25c2abd"
    }
  ],
  "backups": [
    {
      "backup_id": "20260903t105715z-ba8bd940f25c2abd",
      "relative_path": "production/production-redis/2026/09/03/20260903t105715z-ba8bd940f25c2abd",
      "started_at": "2026-09-03T10:57:15Z",
      "completed_at": "2026-09-03T10:57:15Z",
      "size_bytes": 1024,
      "repositories": ["offsite", "spool"]
    }
  ]
}
```

Status values are:

- `complete`: the newest completed backup is present in every repository;
- `empty`: no completed backup exists;
- `degraded`: the newest completed backup is absent from at least one repository;
- `stale`: `status --max-age` found the newest completed backup too old;
- `clock-skew`: the newest completion time is later than report generation.

`list` exits successfully for every valid report, including `empty` and
`degraded`. `status` exits zero only for `complete`; `stale`, `clock-skew`,
`empty`, and `degraded` exit with code 1. Invalid arguments exit with code 2.
Any unreadable or corrupt completed set, repository scan failure, duplicate
repository identity, or conflicting metadata for one backup ID is an error and
produces no successful report.
