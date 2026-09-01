# Repository instructions

Read `docs/architecture.md`, `docs/design-rationale.md`, and
`docs/security-model.md` before changing the product contract or implementing
a new capture or destination integration.

SaveToA is an independent open-source product. Deployment-specific policy must
remain in target configuration or deployment documentation; do not hard-code
consumer names, topology, credentials, paths, or retention into the engine.

Core safety rules:

- do not execute user configuration through a shell;
- never place secrets in process arguments, logs, manifests, or errors;
- never report success before an artifact and its completion marker are durable;
- restore into an explicit destination and never replace a live service by default;
- unknown configuration fields and unknown driver options must fail closed;
- package installation must not enable a backup schedule or grant database access;
- all destructive retention behavior requires tests against incomplete and valid
  backup sets.

Run `make check` before handing over code. If Debian tooling is available, also
run `make deb` and `lintian` against the resulting changes file.
