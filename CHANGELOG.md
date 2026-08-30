# Changelog

Notable changes per release. Dates are release dates. Each version corresponds to a Git
tag and a published GitHub release carrying the Supervisor Service YAML and its cosign
signature.

Entries are named after the feature they introduced, using the same names the
[README](README.md) uses for its sections, so a capability can be traced from how it
works to when it shipped.

## v0.3.0 - 2026-08-30

### Added

- **[Scheduled enforcement](README.md#scheduled-enforcement).**
  `SaltKeyConfig.spec.enforcementInterval` re-runs the Salt chain for a `Ready` VM whose
  last highstate completed longer ago than the interval, correcting configuration that
  drifted in-guest between events. Optional: enforcement stays purely event-driven when
  the field is unset, so upgrading changes no existing deployment's behaviour.

  The interval is measured from each VM's own `salt.vcf.io/highstate-time` rather than
  from a shared schedule, so VMs that bootstrap at different moments come due at
  different moments and no batching is needed. A per-VM jitter derived from the VM UID,
  stable across reconciles and operator restarts, separates VMs onboarded together.

  Applies only to VMs reporting `salt-status=Ready`. A VM in a terminal `Failed/<step>`
  state is never picked up by a timer, so a broken VM stays visibly broken rather than
  looping against a failure nobody has looked at. A malformed duration is rejected by
  the CRD at admission rather than silently disabling enforcement.

## v0.2.2 - 2026-08-24

### Fixed

- **Supervisor Service packaging.** The `Package` hardcoded `metadata.name`,
  `spec.version` and the image tag default to `0.2.0`. `make supervisor-service-yaml`
  only rewrote the `imgpkgBundle` digest, so a released Package silently carried a stale
  version label. Both fields are now templated from `VERSION`.
- **Supervisor Service namespace.** `package.yaml` and `package-metadata.yaml` defaulted
  to `namespace: tkg-system`, a generic kapp-controller default. VCF9 activates
  Supervisor Service packages under `vmware-system-supervisor-services`, confirmed
  against a live install.
- **Air-gap signature preservation.** Cosign signatures are now carried across bundle
  relocation, so a bundle relocated into a private registry keeps a verifiable
  signature.

## v0.2.1 - 2026-08-23

### Added

- **Signed Supervisor Service bundle.** The release pipeline builds and signs the Carvel
  bundle. See `config/supervisor-service/README.md` for the signing details.

## v0.2.0 - 2026-08-23

### Added

- **[Reconvergence from terminal failed states](README.md#phase-3--ready-day-2-and-reconvergence).**
  A VM in `salt.vcf.io/salt-status=Failed/<step>` is recoverable rather than stuck. The
  bootstrap chain re-runs when the VM's intent annotations change, when
  `salt.vcf.io/retry-request` is set to a new value, or when
  `SaltKeyConfig.spec.retryToken` retries every failed VM in the namespace at once. A
  reconvergence resets to the start of the chain so a stale connection is re-verified
  first. The operator never clears a trigger itself, only recording what it acted on,
  which keeps the trigger safe to leave set in Git under continuous reconciliation.
- **[Day-2 detection across the full intent-annotation set](README.md#phase-3--ready-day-2-and-reconvergence).**
  Change detection covers any tenant-set `salt.vcf.io/*` annotation the operator does not
  itself write, not just `salt.vcf.io/roles`. This includes `cis-profile`, `environment`,
  `tag/*`, `cis-exceptions/*` and `vault-path`. A VM with no roles is included, since the
  baseline applies to every managed VM.

### Changed

- **[Scavenger CronJob](README.md#scavenger-cronjob) is report-only by default.**
  `--dry-run` defaults to `true`, so deleting a key requires passing `--dry-run=false`
  explicitly. A key with no correlated VM is logged at error level in both modes, so
  alerting can catch the condition before a scheduled run deletes anything.

### Fixed

- **RaaS body-level errors.** `AcceptKey` and `DeleteKey` silently succeeded when RaaS
  returned HTTP 200 with an error in the response body.

## v0.1.0 - 2026-04-23

Initial release. A VCF9 Supervisor Service, packaged as a Carvel imgpkg bundle, that
watches tenant-namespace `VirtualMachine` resources and accepts or rotates their Salt
minion keys against a RaaS master. Built for air-gapped enterprise deployments where a
private registry hosts both the operator image and the Service bundle.

- **[SaltKeyConfig CRD](README.md#saltkeyconfig-reference).** Namespace-scoped, with
  Secret-backed RaaS credentials, per-namespace `raasURL` and `masterID`, and
  configurable requeue and accept timeouts.
- **VirtualMachine reconciler.** Supports both the v1alpha1 (`status.phase`) and
  v1alpha4 (`status.powerState`) VM Operator shapes, gating on
  `status.network.primaryIP4`.
