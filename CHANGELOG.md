# Changelog

Notable changes per release. Dates are release dates. Each version corresponds to a Git
tag and a published GitHub release carrying the Supervisor Service YAML and its cosign
signature.

Entries are named after the feature they introduced, using the same names the
[README](README.md) uses for its sections, so a capability can be traced from how it
works to when it shipped.

## v0.5.0 - 2026-11-01

Tested on VCF 9.1.1 as a VCF Service. See [Platform support](README.md#platform-support).

### Added

- **[Preseeded keys](README.md#preseeded-keys).** `SaltKeyConfig.spec.keyAcceptance:
  preseed` makes the operator generate and accept each VM's key pair on the master
  through RaaS before the VM boots, and write it with the master's public key to the
  Secret `<vm>-salt-bootstrap` for cloud-init. No pending key is matched, a host guessing
  the minion ID is denied, and a recreated VM gets a new key. VMs already accepted keep
  their key. Needs install value `rbac.preseedKeys`, or `preseedKeys` in the VCF Service
  install form, which grants create, update and patch of Secrets without read. The default stays
  `discovery`.
- **[Automatic enrolment](README.md#automatic-enrolment) in preseed.** Install value
  `enrolment.keyAcceptance`, `enrolKeyAcceptance` in the VCF Service install form, sets
  `discovery` or `preseed` on every `SaltKeyConfig` the operator enrols and changes them in
  place when it changes. `preseed` also grants the Secret create, update and patch that preseed
  needs, so a VCF Service install can run enrolled namespaces in preseed without a team
  config. The default stays `discovery`.
- **[Private key cleared early and within a bound](README.md#preseeded-keys).** In preseed
  mode the operator sets `minion.pem` and `minion.pub` in `<vm>-salt-bootstrap` to
  `cleared` and marks the VM `salt.vcf.io/key-cleared=true`, once, on the first of: the
  guest's signal that cloud-init wrote the files, the first `test.ping`, or
  `acceptTimeout` after VM creation. The guest signal is an optional VM readiness probe on
  the GuestInfo key `salt-key-written`, set by the first `runcmd` line. VM Operator then
  renders the VM's `guestinfo.userdata` without the private key. A preseeded VM not
  running with an IP after `acceptTimeout` is marked `key-status: Failed` with event
  `SaltKeyTimeout`. Event `SaltKeyCleared` names the trigger.

### Fixed

- **A key deletion is no longer reported as failed after it succeeded.** When a VM is
  deleted, a second pass that finds the key already deleted now only removes the
  finalizer instead of asking RaaS again. A deletion that does fail is recorded as a
  `SaltKeyDeleteFailed` warning event instead of under `SaltKeyDeleted`.
- **One `SaltKeyTimeout` event per timeout.** A VM already marked `key-status: Failed`
  after `acceptTimeout` is no longer marked again with a new event on every reconcile.
  It still accepts a matching key that appears later, whenever it is reconciled.

### Known issues

- **The private key stays in RaaS.** `key.gen_accept` returns the key pair as a RaaS job
  result, which RaaS keeps in its database and backups. Protect them as a key store.
- **One master key per minion.** Preseed pins the key of the master behind the RaaS
  connection. If that master's key changes, every preseeded minion refuses it until its
  `minion_master.pub` is replaced. Masters that share one key pair pass this check. The
  minion's key is accepted on the master named by the RaaS connection, so with several
  masters the others must share its minion keys, as a Salt master cluster does through
  `cluster_pki_dir`. Several masters are not tested yet.

## v0.4.0 - 2026-10-29

### Added

- **[Default RaaS connection](README.md#with-the-default-connection).**
  One Secret in the operator's own namespace, created from the install values under
  `raas.connection`, holds the RaaS URL, master ID and credentials. A
  `SaltKeyConfig` that leaves `spec.credentialsSecret` empty uses it, so no RaaS
  credentials need to live in tenant namespaces. Such a `SaltKeyConfig` may restate the
  default URL and master ID, and is refused with `ConnectionMismatch` if it states others
  or asks to skip TLS verification the default does not allow. The scavenger resolves
  connections the same way.
- **[VM selector](README.md#saltkeyconfig-reference) enforced.** `SaltKeyConfig.spec.vmSelector`, defined but never applied in
  earlier releases, now limits which annotated VirtualMachines in the namespace are
  onboarded. A VM that is being deleted always has its key removed, whatever the
  selector, so narrowing it never strands a key or a finalizer. An invalid selector sets
  `Ready=False` with reason `InvalidSelector`.
- **[Automatic enrolment](README.md#automatic-enrolment).** Install values under `enrolment` name the namespaces the
  operator enrols itself, by name, by label selector or all, each with a `SaltKeyConfig`
  named `salt-config` that uses the default connection. Off by default. A namespace that
  already has a `SaltKeyConfig` is never enrolled, and when a team's own config appears in
  an enrolled namespace the operator removes its `salt-config`, so a namespace never holds
  both. The Supervisor's system namespaces are always excluded. When a namespace leaves
  enrolment, or enrolment is turned off altogether, only the configs the operator created
  are removed.
- **[VCF Service](README.md#deploy-as-a-vcf-service-vcf-91) packaging.** `config/vcf-service/` wraps the same payload as a VCF 9.1 VCF
  Service installed from VCF Automation. Each release attaches the VCF Service bundle as
  `vcf-salt-operator.salt.vcf.io-<version>.tar`, signed like the other assets, with its
  payload pinned by digest to the released bundle. `make lab-build` builds the operator image from
  host-compiled binaries, the payload tar, the VCF Service tar and a lab-pinned Service YAML
  through a throwaway local registry. `make render-check` renders all three install modes
  offline and asserts their permission shape.
- **Ready ConfigMap.** The payload creates `vcf-salt-operator-ready`, which the VCF Service
  reads as its ready condition.
- **[Scavenger](README.md#scavenger-cronjob) exclusions.** Install value `scavenger.excludeMinions`, and
  `scavengerExcludeMinions` in the VCF Service, takes comma-separated glob patterns of
  minion IDs the scavenger never reports or deletes. The master's own minion and any
  minion that is not a VM have no VirtualMachine, so without an exclusion a run with
  `--dry-run=false` would delete their keys.
- **A silent Salt master is named.** When test.ping or refresh_pillar fails after waiting a
  minute or more and the master's presence record in RaaS did not change meanwhile, the
  Event has reason `SaltMasterSilent` and says the master is down or cut off from RaaS,
  instead of blaming the minion. The VM status is the same `Failed/<step>` as before, so
  retries work unchanged.
- **Local end-to-end check.** `make local-e2e-up` and `make local-e2e` install a lab build
  through kapp-controller in a kind cluster on podman, in both the Supervisor Service and
  the VCF Service shape, and run a minion container through key accept, highstate and key
  deletion against a real RaaS.

### Fixed

- **One run per retry request after a Day-2 failure.** A Day-2 run that failed at
  refresh_pillar or at the highstate dispatch left its retry request, bulk token and intent
  unrecorded, so the next reconcile took them as new and ran the chain once more unasked.
  They are now recorded with the failure.
- **Retry before the first highstate.** A VM that failed at ping, refresh_pillar or
  dispatch before its first highstate ignored `salt.vcf.io/retry-request` and
  `spec.retryToken`, and stayed failed until it was deleted. Either trigger now restarts
  its Salt chain.
- **A `SaltKeyConfig` waiting for its Secret becomes `Ready` on its own.** A config that
  was not `Ready` because its credentials Secret or the default connection Secret was
  missing, incomplete, unreadable or naming another endpoint stayed that way until the
  operator restarted, since Secrets are read by name and never watched. It is now checked
  again every 30 seconds.
- **The RaaS password stays out of the logs.** Debug logging of the RaaS login wrote the
  request body, password included, and the scavenger always ran with debug logging. The
  login is now logged with the username only, and the scavenger logs at info level like
  the operator.

### Changed

- **[Platform support](README.md#platform-support).** Releases are tested on VCF 9.1. VCF 9.0.x, as a Supervisor Service, is expected
  to work but is not tested, and fixes for it are best effort. The README's Platform support table
  lists what each release was tested on.
- **Secret permissions.** The operator and scavenger read Secrets with `get` only. A Role
  in the operator's namespace grants it on the default connection Secret alone. Install
  value `rbac.tenantCredentialSecrets`, true by default so existing enrolments keep
  working on upgrade, adds the cluster-wide `get` that a credentials Secret in a tenant
  namespace needs. Set it false once every namespace uses the default connection, as a
  VCF Service install requires. `raasURL`, `masterID` and `credentialsSecret` are now
  optional in the CRD.
- **Startup log of the default connection.** The operator reads the connection Secret at
  startup and logs its URL, master ID and TLS setting, or why it is not usable yet.
- **`skipTLSVerify` is no longer defaulted.** A `SaltKeyConfig` shows the field only when
  it is set. On the default connection the `Ready` message states the TLS mode in use.
- **Fewer RaaS polls.** A VM update that changes nothing but the operator's own
  annotations, or VM Operator's own bookkeeping (the
  `vmoperator.vmware.com/backup-version` and `vmoperator.vmware.com/conversion` annotations
  and the `VirtualMachineBackupUpToDate` condition), no longer starts a reconcile, so a
  running highstate is polled once per requeue interval instead of several times a second.
- **`raas.defaultInsecureSkipVerify` is deprecated.** It set an environment variable the
  operator never read. The value is still accepted so existing install values validate.

### Known issues

- **A key deletion can be reported as failed after it succeeded.** When a VM is deleted,
  the key deletion can run twice. The first run deletes the key, the second finds it gone,
  and the operator records a second `SaltKeyDeleted` event saying the deletion failed with
  `Minion(s) not attached to Master`. The key is deleted. A fix is planned for the next
  release.
- **The accept timeout is checked again on every reconcile.** A VM marked
  `key-status: Failed` after `acceptTimeout` is looked at again whenever it is
  reconciled, for example after an operator restart or a VM update. Without a pending key
  it is marked `Failed` again with another `SaltKeyTimeout` event. With a matching pending
  key it is accepted. Setting `salt.vcf.io/retry-request` starts such a reconcile on
  demand.

## v0.3.1 - 2026-09-14

### Changed

- **License.** Relicensed from MIT to MIT No Attribution (MIT-0). The only condition MIT
  imposed, retaining the copyright notice in copies, is removed. Anyone may use, modify,
  redistribute or rebrand this code with no attribution and no obligation of any kind.
  The warranty and liability disclaimer is unchanged. Releases up to and including v0.3.0
  remain available under their original MIT terms.

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
