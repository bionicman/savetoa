# Contributing

Thank you for considering a contribution to SaveToA.

## Development

1. Discuss significant configuration, artifact-format, security, or restore
   changes in an issue before implementation.
2. Keep drivers behind explicit interfaces and avoid consumer-specific logic.
3. Add tests for success, native-tool failure, cancellation, partial output,
   and secret redaction.
4. Run `make check`.
5. Update the relevant documentation and `CHANGELOG.md` for user-visible work.

Commits should be focused and use Conventional Commit subjects where practical.

## Compatibility

Configuration and manifest formats are versioned. Backward-incompatible changes
must introduce a new version and document the migration or rejection behavior.

## Security

Do not open public issues containing credentials, backup contents, private
topology, or exploitable security details. Follow [SECURITY.md](SECURITY.md).
