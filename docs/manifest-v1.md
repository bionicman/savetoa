# Manifest format v1

Each backup set contains one `manifest.json` describing the stored artifact.
The manifest is non-secret and contains enough information to choose a restore
driver without the current target configuration.

The parser accepts one JSON object of at most 1 MiB. Unknown fields, duplicate
object keys, unsupported drivers, unsafe filenames, and invalid semantic values
are rejected.

## Example

```json
{
  "format_version": 1,
  "backup_id": "20260902-example",
  "target": "example-mariadb",
  "capture_driver": "mariadb",
  "started_at": "2026-09-02T10:00:00Z",
  "completed_at": "2026-09-02T10:05:00Z",
  "tool": {
    "name": "mariadb-backup",
    "version": "12.3.0"
  },
  "source": {
    "server_version": "12.3.0",
    "replication": {
      "gtid": "0-1-42"
    }
  },
  "artifact": {
    "filename": "payload.tar.zst.age",
    "size_bytes": 1024,
    "checksum": {
      "algorithm": "sha256",
      "value": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    }
  },
  "transformations": [
    {
      "driver": "zstd",
      "level": 3
    },
    {
      "driver": "age",
      "recipient_fingerprints": [
        "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
      ]
    }
  ]
}
```

The checked-in machine-readable copy is
[`manifest-v1.example.json`](manifest-v1.example.json) and is validated by the
test suite.

## Fields

- `format_version` is exactly `1`.
- `backup_id` and `target` are safe path components, not paths.
- `capture_driver` selects the restore implementation.
- `started_at` and `completed_at` are RFC 3339 timestamps; completion cannot
  precede capture start.
- `tool` identifies the native capture utility and its version.
- `source.server_version` records the captured service version when the source
  has one.
- `source.replication` contains driver-defined, non-secret replication
  coordinates. It is required and non-empty for database captures, and keys
  associated with passwords, tokens, credentials, and private keys are
  rejected.
- `artifact.filename` is a single path component relative to the backup-set
  directory.
- `artifact.size_bytes` is the stored artifact size.
- `artifact.checksum` is a lowercase SHA-256 digest of the stored bytes. When
  encryption is enabled, it therefore covers ciphertext.
- `transformations` is an ordered array. Config v1 supports `zstd` followed by
  `age`; recipient fingerprints use `sha256:<lowercase hex>` over each
  canonical public recipient and never embed decryption identities.

## Completion boundary

A valid manifest does not make a backup set complete. The payload and manifest
must first be durable, then the `complete` marker is written and made durable
last. Restore, listing, and retention must ignore sets without a valid marker.

The marker contains exactly one lowercase digest and a final newline:

```text
sha256:<SHA-256 of the exact manifest.json bytes>
```

Binding the marker to the manifest prevents a stale or partially replaced
manifest from being accepted as complete. Loading a set checks this digest and
the manifest schema. Full verification additionally streams the payload and
checks its recorded size and SHA-256 digest.
