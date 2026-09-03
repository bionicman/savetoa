# Lifecycle hooks v1

SaveToA can emit bounded lifecycle events to optional administrator-managed
executables. Hooks adapt SaveToA to external smoke checks or telemetry; they do
not implement an alert transport and are not part of target YAML.

## Discovery and execution

For an action and outcome, SaveToA reads this fixed directory:

```text
/etc/savetoa/hooks.d/<action>/<outcome>.d/
```

For example, successful backup hooks live in
`/etc/savetoa/hooks.d/run/success.d/` and failed freshness-check hooks live in
`/etc/savetoa/hooks.d/status/failure.d/`. A missing directory means that no hook
is configured. Entries run directly, without a shell, in bytewise filename
order. SaveToA supplies no arguments and writes one JSON object followed by a
newline to stdin.

The complete hook batch has a 15-second timeout. Hook stdout and stderr are
discarded. A timeout, unsafe entry, or nonzero hook exit is reported without
captured output and without changing the original action's exit code. In
particular, a durable backup remains successful and still permits its systemd
`OnSuccess` retention unit to run when telemetry delivery fails.

The v1 actions are `run`, `deliver`, `fetch`, `prune`, `status`, and `doctor`.
Argument/configuration errors which occur before a valid target is resolved do
not emit hooks. Restore operations are outside v1 because they have a separate
operator and privilege boundary.

## Event schema

```json
{
  "schema_version": 1,
  "action": "run",
  "outcome": "success",
  "environment": "production",
  "target": "production-mariadb",
  "backup_id": "20260903t171739z-148669f88b491087",
  "started_at": "2026-09-03T17:17:39.246124175Z",
  "finished_at": "2026-09-03T17:17:40.279421291Z",
  "duration_ms": 1033,
  "exit_code": 0
}
```

`backup_id` is omitted when an action has not produced or selected one.
`repository` is present for `deliver` and `fetch`. `outcome` is `success` only
when `exit_code` is zero. Events intentionally contain no raw error, command
output, credentials, configuration, or hook response.

## Filesystem contract

The hook root, action directory, outcome directory, and every directory entry
must be owned by uid 0 and must not be writable by group or other. Each hook
must be a regular executable file. Symbolic links, subdirectories,
non-executable files, and more than 32 entries fail the hook batch safely.

Hook processes receive only `PATH=/usr/bin:/bin` and `LANG=C.UTF-8`. A hook
which calls `curl` should read its webhook credential from a protected file and
pass it through stdin or a curl config stream; credentials must not appear in
process arguments or hook output. SaveToA does not inspect or log such files.

Example skeleton:

```sh
#!/bin/sh
set -eu
event=$(mktemp)
trap 'rm -f "$event"' EXIT
cat >"$event"
/usr/bin/curl --fail --silent --show-error \
  --config /etc/savetoa/hook-credentials/smoke.curl \
  --header 'Content-Type: application/json' \
  --data-binary "@$event"
```

The external script is responsible for its own secret handling. Its shebang is
an executable-file property; SaveToA never evaluates hook content or target
configuration with a shell.

The packaged service templates permit hook state below
`/var/lib/savetoa/hook-events` when that operator-created directory exists.
No other additional writable path is granted to status or retention hooks.
