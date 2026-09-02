# Configuration contract

The default configuration path is `/etc/savetoa/config.yml`. The installed
package contains no runnable targets.

```yaml
config_version: 1
targets: {}
groups: {}
```

`environment` is required when at least one target exists and becomes the first
component of every backup-set path. The package's empty, non-runnable default
configuration may omit it.

See `packaging/config.example.yml` for the accepted config v1 shape currently
implemented by the parser.

Configuration is limited to 1 MiB and exactly one YAML document. Unknown and
duplicate fields are errors at every level. Environment, target, group, and
destination names use lowercase ASCII letters, digits, `_`, and `-`; they must
begin with a letter or digit and may contain at most 63 characters.

The first implemented target schema is `mariadb`. It requires an absolute
credential-file reference, a local socket, mandatory replica and preparation
safety gates, and at least one destination. Other capture driver names fail
closed until their own typed schemas are implemented.

## Target semantics

Each target declares:

- `driver`: capture implementation;
- `credentials`: reference to protected source credentials;
- `source`: endpoint, socket, and topology/health requirements;
- `capture`: driver-specific capture options;
- `compression`: optional compression transform;
- `encryption`: optional client-side encryption transform;
- `destinations`: one or more named local or remote sinks;
- `retention`: policy for completed backup sets.

Credentials are referenced by file. Secret values must not be embedded in the
main configuration, target names, destination URLs, or manifests.

Config v1 currently accepts `zstd` compression and `age` recipient-file
encryption. Local destinations require a clean absolute path other than `/`.
S3 destinations require a separate credential-file reference, an HTTPS
endpoint without URL credentials, a bucket, and an optional clean relative
object-key prefix.

The `age` recipients file is public-key material, not an identity file. It uses
one X25519 `age1...` recipient per line; blank lines and `#` comments are
allowed. Private identities, SSH recipients, and plugin recipients fail
closed.

Retention is optional. When present, its daily, weekly, and monthly counts
must be non-negative and at least one completed backup must be retained.

## Groups

Groups provide `run-group` ergonomics:

```yaml
groups:
  production-databases:
    targets:
      - production-mariadb
      - production-mongodb
      - production-redis
```

Failure behavior, concurrency, and ordering must be explicit before groups are
implemented. A group is not a distributed transaction.

## Scheduling

Scheduling is intentionally outside the YAML contract. A Debian installation
provides `savetoa@.service` and `savetoa@.timer`; configuration management
creates and enables instances with the desired `OnCalendar` override.
