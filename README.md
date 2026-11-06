# vcf-salt-operator

A Kubernetes operator that automates the full **Salt minion key lifecycle** for VMs
provisioned by [VM Service](https://developer.broadcom.com/apis/vsphere-supervisor-services-and-vm-service)
on a **VCF9 vSphere Supervisor**. It integrates with
[VCF Salt RaaS](https://docs.vmware.com/en/VMware-Aria-Automation-Config/) to accept
Salt keys when VMs are created, run the bootstrap configuration chain, detect and apply
Day-2 role changes, and delete keys when VMs are destroyed.

This operator is purpose-built for VCF9 Supervisor clusters. It is not a generic
Kubernetes operator and does not support plain Kind or kubeadm clusters.

Version history is in [CHANGELOG.md](CHANGELOG.md).

## Platform support

| Platform | Installed as | Status |
|---|---|---|
| VCF 9.1.1 | [VCF Service](#deploy-as-a-vcf-service-vcf-91), from VCF Automation | Tested in 0.5.0 and 0.5.1, including in-place upgrade, fresh install, key accept, preseeded keys, early key clear, highstate and key deletion |
| VCF 9.1.1 | [Supervisor Service](#deploy-as-a-vcf9-supervisor-service), from vCenter | Expected to work, not tested |
| VCF 9.1.0 | VCF Service, from VCF Automation | Tested with 0.4.0 release candidates, including in-place upgrades and Day-2 runs |
| VCF 9.1.0 | Supervisor Service, from vCenter | Tested with 0.4.0 release candidates |
| VCF 9.0.x | Supervisor Service, from vCenter | Expected to work, not tested |
| VCF 9.0.x | VCF Service | Not available, VCF Services start with VCF 9.1 |

Tested platforms get fixes. Untested ones are expected to work because the Supervisor Service install
uses only the `v1alpha4` VirtualMachine API and objects in the operator's own namespace, and fixes for
them are best effort. VCF 9.0.x support may end in a future release.

---

## Full VM lifecycle

### Phase 1 — Key acceptance

The operator watches all `VirtualMachine` resources (`vmoperator.vmware.com/v1alpha4`)
cluster-wide. For each VM annotated `salt.vcf.io/managed=true` it:

1. Waits until the VM reaches `Running` phase with a `primaryIP4` address.
2. Queries RaaS for pending Salt keys and matches one to the VM using four
   strategies in priority order — see [Minion ID matching](#minion-id-matching).
3. Accepts the key via RaaS and records `salt.vcf.io/key-status=Accepted` and
   `salt.vcf.io/accepted-at=<RFC3339>` on the VM.
4. Adds a Kubernetes finalizer (`salt.vcf.io/cleanup`) so key deletion runs before
   the VM object is garbage-collected.

If no pending key appears within `acceptTimeout` (default `10m`), counted from the VM's
creation in vSphere, the VM is marked
`salt.vcf.io/key-status=Failed`, with one `SaltKeyTimeout` event, and the operator stops
polling. The VM is not given up on: whenever it is reconciled again, for example after an
operator restart, a VM update or a `salt.vcf.io/retry-request`, a matching pending key is
accepted and the chain continues.

### Phase 2 — Bootstrap chain

After acceptance a 30-second grace period is enforced before the first Salt command.
This gives the minion time to re-establish its ZeroMQ connection to the master after
its key is accepted. No annotations are written during this wait — a silent requeue
prevents spurious watch events from bypassing the delay.

Once the grace period expires the operator runs, in order:

```
test.ping  →  saltutil.refresh_pillar  →  state.highstate
```

Each step advances a phase annotation (`salt.vcf.io/salt-status`):

| Phase annotation value | Meaning |
|---|---|
| *(empty)* | Grace period or waiting for first ping |
| `Pinged` | `test.ping` returned `true` |
| `PillarRefreshed` | `saltutil.refresh_pillar` returned `true` |
| `HighstateDispatched` | `state.highstate` dispatched; JID stored; polling |
| `Ready` | Highstate completed successfully |
| `Failed/Ping` | `test.ping` failed and retry window expired |
| `Failed/PillarRefresh` | `refresh_pillar` failed and retry window expired |
| `Failed/HighstateDispatch` | Highstate could not be dispatched |
| `Failed/Highstate` | Highstate completed with one or more failed states |

Every `Failed/<step>` state is recoverable, not permanent — see
[Phase 3 — Ready, Day-2, and reconvergence](#phase-3--ready-day-2-and-reconvergence).

`test.ping` and `saltutil.refresh_pillar` are retried with a 60-second backoff for up
to 10 minutes from the acceptance timestamp. After that window, failure is terminal
and requires operator intervention. `state.highstate` is dispatched asynchronously;
the JID is stored in `salt.vcf.io/highstate-jid` and polled on subsequent reconciles.

`saltutil.refresh_pillar` is always run before `state.highstate` — this is a
load-bearing invariant. Pillar data is fetched from a Git-backed external pillar; if
highstate ran against a stale pillar the wrong states would be applied.

### Phase 3 — Ready, Day-2, and reconvergence

Once highstate succeeds the operator sets `salt.vcf.io/ready=true` and enters the
Day-2 watch loop. On every reconcile it hashes the full set of tenant-set
`salt.vcf.io/*` annotations (roles, `cis-profile`, `environment`, `tag/*`,
`cis-exceptions/*`, `vault-path`, and any other intent annotation) and compares it
against `salt.vcf.io/roles-hash`, written at the last highstate dispatch. A mismatch
triggers a new `refresh_pillar → highstate` cycle. This detects any intent change
pushed by GitOps (e.g. ArgoCD updating an annotation) without polling — the
annotation change itself generates the watch event. A VM with no roles configured is
included, since the baseline configuration applies regardless of roles.

Reconvergence is reached from `Ready` and from every terminal `Failed/<step>` state, and
four things can trigger it. An intent-hash change, described above, applies to both. Two
explicit triggers pull a failed VM back into the bootstrap chain:

- `salt.vcf.io/retry-request` is set to a new value (any value different from
  `salt.vcf.io/retry-handled`, which the operator writes once it acts).
- `SaltKeyConfig.spec.retryToken` is set to a new value, retrying every VM in the
  namespace currently in a `Failed/<step>` state.

The fourth is scheduled rather than event-driven: with
`SaltKeyConfig.spec.enforcementInterval` set, a `Ready` VM whose last highstate is older
than that interval re-runs the chain on its own. See
[Scheduled enforcement](#scheduled-enforcement) for how the schedule is derived and why
it never touches a failed VM.

A reconvergence resets to the start of the chain (`test.ping` first) rather than
resuming at the failed step, so a stale connection is re-verified before anything else
runs. The operator never clears `retry-request` or `retryToken` itself, it only
records what it last acted on in `salt.vcf.io/retry-handled` /
`salt.vcf.io/bulk-retry-handled` - this is what makes the trigger safe to leave set
in Git under continuous GitOps reconciliation.

### VM deletion

When a VM's `DeletionTimestamp` is set the finalizer fires:

1. The operator calls RaaS to delete the Salt key (best-effort — failure is logged but
   does not block GC).
2. The finalizer is removed; Kubernetes proceeds with VM garbage collection.
3. The Scavenger CronJob (see [Scavenger](#scavenger-cronjob)) handles keys whose VMs
   were deleted while the operator was offline.

---

## Annotations reference

### Written by the operator

| Annotation | Values | Description |
|---|---|---|
| `salt.vcf.io/key-status` | `Preseeded` · `Accepted` · `Failed` · `Deleted` · `DeleteFailed` | Key acceptance lifecycle state. `Preseeded` means the key is accepted and waiting for the VM to run |
| `salt.vcf.io/minion-id` | string | Salt minion ID matched and accepted |
| `salt.vcf.io/match-method` | `name` · `ip` · `shifted-ip` · `prefix` · `preseed` | Matching strategy used, or `preseed` for a key the operator generated |
| `salt.vcf.io/salt-status` | See phase table above | Post-accept bootstrap phase |
| `salt.vcf.io/ready` | `true` · `false` | Salt management fully complete and healthy |
| `salt.vcf.io/highstate-jid` | JID string | JID of the running or last completed highstate |
| `salt.vcf.io/highstate-status` | `InProgress` · `Success` · `Failed` | Last highstate outcome |
| `salt.vcf.io/highstate-time` | RFC3339 timestamp | Completion time of the last highstate |
| `salt.vcf.io/highstate-failed-count` | integer string | Number of failed states (`0` = all succeeded) |
| `salt.vcf.io/highstate-failed-states` | comma-separated state IDs | Failed state identifiers — contains only state map keys, never rendered values or secrets |
| `salt.vcf.io/roles-hash` | hex string, `v2:`-prefixed | Hash of the full tenant-set `salt.vcf.io/*` intent-annotation set at last highstate dispatch |
| `salt.vcf.io/key-cleared` | `true` | Preseed only. The private key in `<vm>-salt-bootstrap` was replaced after the first successful `test.ping` |
| `salt.vcf.io/accepted-at` | RFC3339 timestamp | When the key was accepted; drives grace period and retry window |
| `salt.vcf.io/retry-handled` | string | Last `salt.vcf.io/retry-request` value the operator acted on |
| `salt.vcf.io/bulk-retry-handled` | string | Last `SaltKeyConfig.spec.retryToken` value this VM acted on |

`salt.vcf.io/highstate-failed-count` and `salt.vcf.io/highstate-failed-states` are
safe to expose to application teams. They contain Salt state IDs (e.g.
`pkg_|-install_agent_|-my-package_|-installed`) — static strings defined in the state
tree with no rendered pillar values, secrets, or command output.

### Set by GitOps / user (read by the operator)

| Annotation | Purpose |
|---|---|
| `salt.vcf.io/managed` | Set to `"true"` to opt a VM into Salt key management |
| `salt.vcf.io/roles` | Comma-separated role list watched for Day-2 changes (e.g. `web_server,monitoring_agent`) |
| `salt.vcf.io/retry-request` | Any value different from `salt.vcf.io/retry-handled` re-runs the Salt chain, including from a `Failed/<step>` state |

---

## Minion ID matching

The operator tries four strategies in order, stopping at the first match:

| Strategy | Logic | Typical use |
|---|---|---|
| `name` | Case-insensitive exact match of VM name against pending key ID | Standard — minion ID equals the VM hostname |
| `ip` | Exact match of VM's `primaryIP4` against pending key ID | Minion configured with IP as its ID |
| `shifted-ip` | First octet rotated to last (e.g. `10.1.2.3` → `1.2.3.10`) | Legacy convention used by some existing Salt setups |
| `prefix` | Case-insensitive prefix match with `.` separator (FQDN key IDs) | Minion ID is a FQDN; VM name is the short hostname |

---

## Preseeded keys

With `keyAcceptance: preseed` on a `SaltKeyConfig` the operator never matches pending keys
in that namespace. It follows Salt's preseed practice instead:

1. When a managed VM appears, before it boots, the operator asks the master through RaaS
   to generate a key pair for the minion ID and accept it (`key.gen_accept`). The minion
   ID is the VM name.
2. It writes the Secret `<vm>-salt-bootstrap`, owned by the VM, with `minion.pem`,
   `minion.pub`, `minion_master.pub` and `minion_id`, and marks the VM
   `salt.vcf.io/key-status=Preseeded`.
3. The VM's cloud-init places the four files before the minion starts. The minion
   authenticates with a key the master already trusts and refuses any master whose key
   differs from `minion_master.pub`.
4. When the VM runs with an IP the key status becomes `Accepted` and the bootstrap chain
   starts as usual.
5. The operator sets `minion.pem` and `minion.pub` in the Secret to `cleared` and marks
   the VM `salt.vcf.io/key-cleared=true`, once, on whichever comes first:
   - the guest's signal that the files are written (see below),
   - the first successful `test.ping`,
   - `acceptTimeout` after the VM's creation in vSphere, or a final ping failure. A VM
     not yet created in vSphere waits without a limit. A VM still not running
     with an IP by then is marked `key-status=Failed` with event `SaltKeyTimeout` and must
     be recreated, which gives it a new key.

   Event `SaltKeyCleared` names the trigger. The VM update makes VM Operator render the
   VM's cloud-init data again, now without the private key. Cloud-init writes files once
   per VM instance, so the minion keeps its key through reboots and power cycles.

A host that guesses the minion ID and presents its own key is denied by the master. The
VM manifest references the Secret, which only the operator creates, so VM Operator waits
for it before the VM boots. Use the structured `cloudConfig` with `write_files`, since a
`rawCloudConfig` cannot take files from a second Secret:

```yaml
spec:
  readinessProbe:
    guestInfo:
    - key: salt-key-written
  bootstrap:
    cloudInit:
      cloudConfig:
        runcmd:
        - vmtoolsd --cmd "info-set guestinfo.salt-key-written 1"
        write_files:
        - path: /etc/salt/pki/minion/minion.pem
          permissions: "0400"
          content: {name: <vm>-salt-bootstrap, key: minion.pem}
        - path: /etc/salt/pki/minion/minion.pub
          content: {name: <vm>-salt-bootstrap, key: minion.pub}
        - path: /etc/salt/pki/minion/minion_master.pub
          content: {name: <vm>-salt-bootstrap, key: minion_master.pub}
        - path: /etc/salt/minion_id
          content: {name: <vm>-salt-bootstrap, key: minion_id}
```

The image must not carry a Salt key, a minion ID or a cached master key, and nothing in
cloud-init may delete `/etc/salt/pki` after `write_files`.

The `readinessProbe` and the `runcmd` line are optional and give the guest signal. Keep
the `vmtoolsd` line first in `runcmd`. cloud-init runs `runcmd` after `write_files`, so
the key `guestinfo.salt-key-written` means the files are on disk. VM Operator then sets
the VM condition `Ready=True`, and the operator clears the key, usually minutes before
the first ping. It honours `Ready=True` only from a probe on `salt-key-written` and only
the first time, so a later change of the key or the condition does nothing.

Without further settings any process in the guest can set that key, and a non-root
process could make the operator clear the Secret before cloud-init has read it. That VM
then never works, but no key is exposed. Set the VM configuration parameter
`guest_rpc.rpci.auth.cmd.info-set = "TRUE"` in the VM class, under
`spec.configSpec.extraConfig`, so that only root can set GuestInfo keys and only through
`vmtoolsd --cmd` (Broadcom KB 377294). cloud-init keeps working with it and logs refused
writes of the `guestinfo.local-ipv4` and `local-ipv6` values it publishes. Do not set the
companion `guest_rpc.rpci.auth.cmd.info-get`: cloud-init 23.1.1, as on Photon OS 5, reads
GuestInfo only through `vmware-rpctool`, which that parameter refuses, so the VM boots
with no network and no files.

Key state rules:

| Found on the master for the minion ID | Operator action |
|---|---|
| The VM already shows `key-status=Accepted` | Keeps the key. A namespace switching from discovery adopts its running VMs |
| Another VM records this minion ID | Generates nothing, marks the VM `Failed` with event `SaltMinionIDInUse`, or waits while that VM is being deleted |
| No key, or only a denied one | Generates a key |
| A pending or rejected key | Replaces it and raises event `SaltForeignKeyReplaced` |
| An accepted key no VM records | Replaces it, the recreated VM case |

Preseed for enrolled namespaces needs only `enrolment.keyAcceptance: preseed`, or
`enrolKeyAcceptance` in the VCF Service install form (see Automatic enrolment). For a team's
own `SaltKeyConfig` it needs install value `rbac.preseedKeys: true`, or `preseedKeys` in the VCF Service
install form, which grants the operator cluster-wide create, update and patch of Secrets, never
read. The private key is part of the RaaS job result and stays in the RaaS database.

VM Operator delivers the rendered cloud-init data, Secret content included, to the guest
as `guestinfo.userdata`, which any process in the guest can read through VMware Tools and
any vCenter user with a permission on the VM, the `Read-only` role included, can read in
its advanced settings or through the API. Step 5 limits that exposure of the private
key to the time from the VM's creation in vSphere to the guest signal or the first
`test.ping`, and to `acceptTimeout` at most.

The minion trusts only the master key in `minion_master.pub`. If the master's key changes,
preseeded minions refuse it until that file is replaced on each of them. Masters that share
one key pair pass this check. The minion's key is accepted on the master named by the RaaS
connection, so with several masters the others must share its minion keys, as a Salt master
cluster does through `cluster_pki_dir`. Several masters are not tested yet.

---

## SaltKeyConfig reference

One `SaltKeyConfig` per enrolled namespace connects the operator to a RaaS endpoint. It
either uses the operator's default connection, set in the install values under
`raas.connection`, or names a credentials Secret in its own namespace. With the default
connection every field is optional and `spec: {}` is enough.

```yaml
apiVersion: salt.vcf.io/v1alpha1
kind: SaltKeyConfig
metadata:
  name: default
  namespace: <tenant-namespace>
spec:
  raasURL: https://<raas-host>          # RaaS API base URL. Optional with the default connection
  masterID: <salt-master-id>            # Salt master ID registered in RaaS. Optional with the default connection
  credentialsSecret: <secret-name>      # Secret with keys username and password. Empty uses the default connection
  skipTLSVerify: false                  # Set true for self-signed RaaS certificates
  keyAcceptance: discovery              # discovery or preseed, see Preseeded keys
  vmSelector:                           # Optional. Only annotated VMs matching it are onboarded
    matchLabels:
      salt: enabled
  acceptTimeout: "10m"                  # Pending key wait, and in preseed the latest key clear
  requeueInterval: "30s"               # How often to re-check for pending keys
  retryToken: ""                        # Any new value retries every Failed/<step> VM in this namespace
  enforcementInterval: ""               # e.g. "24h". Re-runs a Ready VM whose last highstate is older. Empty disables
```

The `SaltKeyConfigReconciler` resolves the connection and sets
`status.conditions[Ready=True]`. On the default connection the message reads "Using the
operator's default RaaS connection" and states the TLS mode. A config on the default
connection may restate its URL and master ID. One that names another URL or master, or
asks to skip TLS verification the default does not allow, is refused with reason
`ConnectionMismatch`. A config waiting for its Secret is checked again every 30 seconds
and turns `Ready` on its own once the Secret is there. The VM reconciler only acts in
namespaces where this condition is met.

`vmSelector` limits which annotated VMs in the namespace are onboarded. A VM that is
being deleted always has its key removed, whatever the selector, so narrowing it never
strands a key. An invalid selector sets `Ready=False` with reason `InvalidSelector`.

---

## Prerequisites

- **VCF9 Supervisor** with VM Service enabled (API group `vmoperator.vmware.com/v1alpha4`).
  See [Platform support](#platform-support) for the versions each release is tested on.
- **VCF Salt RaaS** instance reachable from the Supervisor worker nodes.
- **Container registry** reachable from the Supervisor — public
  (`ghcr.io/stanimir-kozarev/vcf-salt-operator`) or private (see air-gap path below).
- **Carvel tools** (`imgpkg`, `kbld`, `ytt`) on the build host (required for both Path A and Path B):
  ```bash
  curl -L https://carvel.dev/install.sh | sudo bash
  ```
- **`kubectl`** configured for the Supervisor.

---

## Deploy as a VCF9 Supervisor Service

VCF9 Supervisor users cannot create cluster-scoped RBAC resources directly. The
operator must be installed by a vSphere administrator as a **Supervisor Service** via
the vSphere Client. The Carvel kapp-controller bundled in the Supervisor applies the
manifests with the elevated permissions the service package declares.

On VCF 9.1 the same release can instead be installed from VCF Automation, see
[Deploy as a VCF Service](#deploy-as-a-vcf-service-vcf-91).

Set these variables once for your environment before running any command below:

```bash
export IMAGE_REPO=<image-repository>   # e.g. ghcr.io/stanimir-kozarev/vcf-salt-operator
export BUNDLE_REPO=<bundle-repository> # e.g. ghcr.io/stanimir-kozarev/vcf-salt-operator-bundle
export VERSION=<version>               # e.g. 0.2.0
```

---

### Path A — Internet-connected Supervisor (public or reachable registry)

If your Supervisor nodes can pull images from your registry directly, the flow is:

**1. Build and push the operator image**

Build on a **native `linux/amd64` host**. Cross-compiling from Apple Silicon
(`--platform=linux/amd64`) produces a correct binary but QEMU emulation makes the
build take hours rather than minutes. Use a Linux x86\_64 build host for production
builds.

```bash
# On your linux/amd64 build host
git clone https://github.com/stanimir-kozarev/vcf-salt-operator
cd vcf-salt-operator

docker build -t ${IMAGE_REPO}:${VERSION} .
docker push ${IMAGE_REPO}:${VERSION}
```

**2. Build and push the Supervisor Service bundle**

```bash
mkdir -p dist
make supervisor-bundle \
    VERSION=${VERSION} \
    IMG=${IMAGE_REPO}:${VERSION} \
    BUNDLE_IMG=${BUNDLE_REPO}:${VERSION}
```

This runs `ytt` + `kbld` + `imgpkg push`. `kbld` rewrites image references to
immutable digests — `docker push` in step 1 must complete successfully before this
step.

**3. Generate the Supervisor Service YAML**

```bash
make supervisor-service-yaml VERSION=${VERSION} BUNDLE_IMG=${BUNDLE_REPO}:${VERSION}
# Produces: dist/vcf-salt-operator-supervisorservice-${VERSION}.yaml
```

**4. Upload to vSphere Client**

vSphere Client → **Workload Management → Services → Add New Service** →
upload `dist/vcf-salt-operator-supervisorservice-${VERSION}.yaml` →
register at cluster scope → **Install on Supervisors**.

**5. Set install values**

Paste the following into **YAML Service Config** during install, editing to match your
environment:

```yaml
#@data/values
---
namespace: vcf-salt-operator-system

image:
  repository: <image-repository>
  tag: "<version>"
  pullPolicy: IfNotPresent

imagePullSecret:
  dockerconfigjson: "<base64-encoded-dockerconfigjson>"

raas:
  connection:                        # the default RaaS connection, see Enrol a namespace
    url: https://<raas-host>
    masterID: <salt-master-id>
    username: <raas-username>
    password: <raas-password>
    insecureSkipVerify: false        # set true if RaaS uses a self-signed certificate
```

Every value is optional. `config/supervisor-service/sample-values.yaml` lists them all,
including `enrolment`, `scavenger.excludeMinions` and `rbac.tenantCredentialSecrets`. The
values schema in `config/supervisor-service/package/package.yaml` describes each one, and
vCenter shows those descriptions on install.

Generate the `dockerconfigjson` value on your build host:

```bash
cat ~/.docker/config.json | base64 -w0
```

---

### Path B — Air-gap / private registry

Use this path when Supervisor nodes have no internet access. All images travel as a
single tar archive from the build machine to the lab jump host.

#### On the build machine (internet access, any OS)

**1. Package the source**

On macOS, use `COPYFILE_DISABLE=1` to prevent macOS extended-attribute sidecars
(`._*` files) that cause `ytt` parse errors on the lab host.

```bash
cd <repo-root>
COPYFILE_DISABLE=1 tar -czf /tmp/vcf-salt-operator-src-${VERSION}.tar.gz \
  --exclude='._*' --exclude='.DS_Store' \
  --exclude='.git' --exclude='bin' --exclude='dist' \
  --exclude='cover.out' \
  --exclude='config/supervisor-service/bundle/.imgpkg/images.yml' \
  --exclude='config/supervisor-service/bundle/config/002-crd.yaml' \
  -C .. vcf-salt-operator
```

Transfer the tar to the build host:

```bash
scp /tmp/vcf-salt-operator-src-${VERSION}.tar.gz <user>@<build-host>:/tmp/
```

#### On the build host (linux/amd64, internet access)

**2. Install build tools**

```bash
# Carvel tools (imgpkg, kbld, ytt)
curl -L https://carvel.dev/install.sh | sudo bash

# Docker must also be installed and the daemon running
docker info
```

**3. Build the operator image and bundle, export as offline tar**

```bash
cd /tmp
tar -xzf vcf-salt-operator-src-${VERSION}.tar.gz
cd vcf-salt-operator

export VERSION=<version>
export IMAGE_REPO=<image-repository>
export BUNDLE_REPO=<bundle-repository>

# Run a local staging registry
docker run -d -p 5000:5000 --restart=always --name localreg registry:2

# Build and stage locally
make supervisor-bundle \
    VERSION=${VERSION} \
    IMG=localhost:5000/vcf-salt-operator:${VERSION} \
    BUNDLE_IMG=localhost:5000/vcf-salt-operator-bundle:${VERSION}

make supervisor-service-yaml \
    VERSION=${VERSION} \
    BUNDLE_IMG=localhost:5000/vcf-salt-operator-bundle:${VERSION}

# Pack bundle + operator image into a single offline tar
mkdir -p dist
make supervisor-offline-tar \
    VERSION=${VERSION} \
    BUNDLE_IMG=localhost:5000/vcf-salt-operator-bundle:${VERSION}
```

Transfer these three files to the lab jump host:

```bash
scp dist/vcf-salt-operator-airgap-${VERSION}.tar \
    dist/vcf-salt-operator-supervisorservice-${VERSION}.yaml \
    config/supervisor-service/sample-values.yaml \
    <user>@<lab-jump-host>:/tmp/
```

#### On the lab jump host (linux/amd64, access to private registry and vSphere)

**4. Install imgpkg on the jump host**

```bash
curl -L https://carvel.dev/install.sh | sudo bash
# Verify: imgpkg version
```

**5. Push to private registry and rewrite service YAML**

```bash
export DEST_REPO=<private-registry>/<project>/vcf-salt-operator-bundle
export VERSION=<version>

docker login <private-registry>
imgpkg login -r <private-registry> --username <user> --password <password>

make supervisor-offline-import \
    TAR=/tmp/vcf-salt-operator-airgap-${VERSION}.tar \
    DEST_REPO=${DEST_REPO} \
    SERVICE_YAML=/tmp/vcf-salt-operator-supervisorservice-${VERSION}.yaml
```

If `make` is not available on the jump host, run the two underlying commands directly:

```bash
imgpkg copy \
    --tar /tmp/vcf-salt-operator-airgap-${VERSION}.tar \
    --to-repo ${DEST_REPO} \
    --lock-output /tmp/bundle.lock.yml

DIGEST=$(awk '/image:/{print $2; exit}' /tmp/bundle.lock.yml)
sed -E -i.bak \
    "/imgpkgBundle:/,/image:/ s|image:[[:space:]]+.*|image: ${DIGEST}|" \
    /tmp/vcf-salt-operator-supervisorservice-${VERSION}.yaml
```

**6. Trust the private registry CA on the Supervisor**

vSphere Client → **Workload Management → Supervisor → Configure →
Image Registry → Trusted CAs** → upload the registry's CA certificate PEM.

This is required for the Supervisor container runtime to pull images from a
self-signed registry. The `imagePullSecret` handles authentication; the CA trust
handles TLS.

**7. Upload the service YAML and install**

vSphere Client → **Workload Management → Services → Add New Service** →
upload `/tmp/vcf-salt-operator-supervisorservice-${VERSION}.yaml` →
register at cluster scope → **Install on Supervisors**.

Edit `/tmp/sample-values.yaml` to point at your private registry:

```yaml
#@data/values
---
namespace: vcf-salt-operator-system

image:
  repository: <private-registry>/<project>/vcf-salt-operator
  tag: "<version>"
  pullPolicy: IfNotPresent

imagePullSecret:
  dockerconfigjson: "<base64-encoded-dockerconfigjson>"

raas:
  connection:
    url: https://<raas-host>
    masterID: <salt-master-id>
    username: <raas-username>
    password: <raas-password>
    insecureSkipVerify: false
```

Paste the edited YAML into **YAML Service Config** and complete the install.

**8. Verify the install**

```bash
# Operator namespace (name is assigned by the Supervisor)
SVC_NS=$(kubectl get ns -o name | grep 'svc-vcf-salt-operator' | head -1 | cut -d/ -f2)

kubectl -n ${SVC_NS} get pods -o wide
kubectl -n ${SVC_NS} logs deploy/controller-manager --tail=80
kubectl get crd saltkeyconfigs.salt.vcf.io
```

Expected: pod `controller-manager-*` in `Running`; log lines show both controllers
starting (`"controller": "saltkeyconfig"` and `"controller": "virtualmachine"`);
CRD `saltkeyconfigs.salt.vcf.io` present cluster-wide. The first log lines also show the
default connection's URL, master ID and TLS setting, or why it is not usable yet.

### Upgrade

In the vSphere Client add the new version to the existing service, then upgrade the
Supervisor's install to it. Do not uninstall first: uninstalling the service deletes
every `SaltKeyConfig` on the Supervisor.

From 0.3.x, namespaces with their own credentials Secret stay `Ready`, since
`rbac.tenantCredentialSecrets` defaults to true. A `SaltKeyConfig` that sets
`spec.vmSelector`, which 0.3.x ignored, onboards only the VMs it matches from 0.4.0 on.

---

## Deploy as a VCF Service (VCF 9.1)

On VCF 9.1, VCF Automation installs partner services as VCF Services on the Supervisors of
a region, from one bundle uploaded in Provider Management, Service Management, Partner
Services. The release attaches `vcf-salt-operator.salt.vcf.io-<version>.tar` for this. It
wraps the same Carvel payload as the Supervisor Service.

What differs from the Supervisor Service:

- The bundle is unsigned, so VCF Automation shows a warning on upload. VCF 9.1 refuses an
  unsigned service's cluster-wide Secret reads, so every namespace uses the default
  connection and `rbac.tenantCredentialSecrets` is false.
- Enter the install values in the VCF Automation install form. For CI/CD pipelines and
  other automation, send the same values through the Service Manager API as the user
  overwrite. The upgrade and Edit forms open with the installed values. A pipeline that
  upgrades through the API sends the values to the new version right away, since each
  version stores its own. Until then the operator runs without them and removes the
  `SaltKeyConfig`s it enrolled.
- The Supervisor Service and the VCF Service share a service ID. Moving from one to the
  other means uninstalling and deleting the Supervisor Service first, which deletes every
  `SaltKeyConfig`. Re-apply enrolment from Git afterwards, or use automatic enrolment.

[`config/vcf-service/README.md`](config/vcf-service/README.md) has the steps, the API
calls and the platform behaviour measured on VCF 9.1.

---

## Enrol a namespace

A namespace is enrolled by a `SaltKeyConfig` in it. There are three ways to get one there.

### With the default connection

When the install values set `raas.connection`, a namespace needs only an empty
`SaltKeyConfig`, with no Secret and no RBAC grant:

```bash
TENANT_NS=<tenant-namespace>
kubectl apply -f - <<EOF
apiVersion: salt.vcf.io/v1alpha1
kind: SaltKeyConfig
metadata:
  name: default
  namespace: ${TENANT_NS}
spec: {}
EOF
```

Expected: condition `Ready=True` with the message "Using the operator's default RaaS
connection, TLS verification on". This keeps RaaS credentials out of tenant namespaces.

### Automatic enrolment

Install values under `enrolment` let the operator create the `SaltKeyConfig` itself, named
`salt-config` and using the default connection:

```yaml
enrolment:
  namespaces: [team-a, team-b]   # by name
  all: false                     # or every namespace
  namespaceSelector: ""          # or by label, for example salt=enabled
  keyAcceptance: discovery       # or preseed, see Preseeded keys
```

Enrolment is off by default. Namespaces whose names start with `kube-`, `vmware-system-`,
`svc-`, `tkg-system` or `kapp-controller` are never enrolled, and a non-empty
`excludePrefixes` replaces that list. A namespace that already has a `SaltKeyConfig` is
left alone, and when a team adds its own later, the operator removes `salt-config`.
Removing a namespace from enrolment, or turning enrolment off, deletes only the configs
the operator created, those labelled `salt.vcf.io/managed-by=vcf-salt-operator`.

`keyAcceptance` sets the mode of every `salt-config` the operator creates, and changing it
changes them in place. Running VMs keep their keys either way. With `preseed` the install
also grants the operator create, update and patch of Secrets, never read, so `rbac.preseedKeys`
is not needed. A team's own `SaltKeyConfig` keeps the mode it states. The VCF Service
install form calls this value `enrolKeyAcceptance`.

### With a credentials Secret per namespace

Each namespace holds its own RaaS credentials:

1. A `Secret` with RaaS credentials.
2. A `SaltKeyConfig` pointing at the RaaS endpoint.
3. A namespace-scoped RBAC grant so the operator can read the Secret. Needed only when
   `rbac.tenantCredentialSecrets` is false. By default the operator may `get` Secrets
   cluster-wide.

This route is not available on a VCF Service install.

```bash
TENANT_NS=<tenant-namespace>
SVC_NS=$(kubectl get ns -o name | grep 'svc-vcf-salt-operator' | head -1 | cut -d/ -f2)

# RBAC — allows the operator's ServiceAccount to read Secrets in this namespace only
sed "s/TARGET_NAMESPACE/${TENANT_NS}/g" \
    config/vcf9/namespace-enroll.yaml | kubectl apply -f -

# Credentials and SaltKeyConfig
kubectl apply -f - <<EOF
---
apiVersion: v1
kind: Secret
metadata:
  name: raas-credentials
  namespace: ${TENANT_NS}
type: Opaque
stringData:
  username: <raas-username>
  password: <raas-password>
---
apiVersion: salt.vcf.io/v1alpha1
kind: SaltKeyConfig
metadata:
  name: default
  namespace: ${TENANT_NS}
spec:
  raasURL: https://<raas-host>
  masterID: <salt-master-id>
  skipTLSVerify: false
  credentialsSecret: raas-credentials
  requeueInterval: "30s"
  acceptTimeout: "10m"
EOF

# Confirm Ready
kubectl -n ${TENANT_NS} get saltkeyconfig default -o jsonpath='{.status.conditions}' | jq .
```

Expected: condition `Ready=True`.

---

## Opt a VM in

Add `salt.vcf.io/managed=true` to the VM's annotations. The operator picks it up on
the next watch event. If the namespace's `SaltKeyConfig` sets `spec.vmSelector`, the VM
must match it too.

For new VMs, set it in the manifest and include cloud-init configuration that installs
and configures the salt-minion with the correct `minion_id` and master address:

```yaml
apiVersion: vmoperator.vmware.com/v1alpha4
kind: VirtualMachine
metadata:
  name: <vm-name>
  namespace: <tenant-namespace>
  annotations:
    salt.vcf.io/managed: "true"
    salt.vcf.io/roles: "web_server,monitoring_agent"  # optional; triggers Day-2 on change
spec:
  className: <vm-class>
  image:
    kind: VirtualMachineImage
    name: <image-name>
  imageName: <image-name>
  storageClass: <storage-class>
  powerState: PoweredOn
  network:
    interfaces:
    - name: eth0
      network:
        apiVersion: crd.nsx.vmware.com/v1alpha1
        kind: SubnetSet
        name: vm-default
  bootstrap:
    cloudInit:
      cloudConfig:
        users:
        - name: user
          lock_passwd: false
          sudo: ALL=(ALL) NOPASSWD:ALL
          passwd:
            name: <vm-name>-bootstrap-secret  # Secret with key: user-passwd (hashed)
            key: user-passwd
        write_files:
        - path: /etc/salt/minion_id
          content: |
            <vm-name>
        - path: /etc/salt/minion.d/master.conf
          content: |
            master: <raas-host>
            id: <vm-name>
        runcmd:
        # Purge any stale Salt state baked into the VM image before installing.
        # Without this, a pre-existing salt-minion or leftover /etc/salt/pki
        # re-registers under the image hostname instead of the intended minion_id.
        - systemctl stop salt-minion 2>/dev/null || true
        - rm -rf /etc/salt/pki /var/cache/salt/minion
        - rm -f  /etc/salt/minion
        - echo <vm-name> > /etc/salt/minion_id
        # Install from the Broadcom Salt repository (distro repos ship outdated versions).
        - apt-get update -y
        - apt-get install -y curl ca-certificates gnupg
        - mkdir -p /etc/apt/keyrings
        - curl -fsSL https://packages.broadcom.com/artifactory/api/security/keypair/SaltProjectKey/public -o /etc/apt/keyrings/salt-archive-keyring.pgp
        - echo 'deb [signed-by=/etc/apt/keyrings/salt-archive-keyring.pgp arch=amd64] https://packages.broadcom.com/artifactory/saltproject-deb stable main' > /etc/apt/sources.list.d/salt.list
        - apt-get update -y
        - apt-get install -y salt-minion
        - systemctl enable salt-minion
        - systemctl restart salt-minion
```

Create the bootstrap Secret before applying the VM manifest:

```bash
HASHED_PASS=$(python3 -c \
  "import crypt; print(crypt.crypt('<password>', crypt.mksalt(crypt.METHOD_SHA512)))")

kubectl -n <tenant-namespace> create secret generic <vm-name>-bootstrap-secret \
  --from-literal=user-passwd="${HASHED_PASS}" \
  --dry-run=client -o yaml | kubectl apply -f -
```

> The bootstrap Secret is consumed by cloud-init at **first boot only**. Updating it
> after the VM exists has no effect. To apply a new password, delete and recreate the VM.

---

## Verifying the lifecycle

**Watch operator logs during VM provisioning:**

```bash
SVC_NS=$(kubectl get ns -o name | grep 'svc-vcf-salt-operator' | head -1 | cut -d/ -f2)
kubectl -n ${SVC_NS} logs deploy/controller-manager -f
```

Expected log progression:

```
Reconciling VirtualMachine          name=<vm-name>  saltConfig=default
Waiting for VM to be running        name=<vm-name>  phase=""
Waiting for VM to be running        name=<vm-name>  phase=Running  primaryIP=""
Waiting for minion to connect       name=<vm-name>  remaining=28s
Running test.ping                   name=<vm-name>  minionID=<vm-name>
test.ping succeeded                 name=<vm-name>  minionID=<vm-name>
Running saltutil.refresh_pillar     name=<vm-name>  minionID=<vm-name>
refresh_pillar succeeded            name=<vm-name>  minionID=<vm-name>
Dispatching state.highstate         name=<vm-name>  minionID=<vm-name>
Highstate not yet complete          name=<vm-name>  jid=<jid>
Highstate succeeded                 name=<vm-name>  minionID=<vm-name>  jid=<jid>
```

**Check VM annotations after completion:**

```bash
kubectl -n <tenant-namespace> get vm <vm-name> \
  -o jsonpath='{.metadata.annotations}' | jq .
```

Expected on success:

```json
{
  "salt.vcf.io/key-status":               "Accepted",
  "salt.vcf.io/minion-id":                "<vm-name>",
  "salt.vcf.io/match-method":             "name",
  "salt.vcf.io/salt-status":              "Ready",
  "salt.vcf.io/ready":                    "true",
  "salt.vcf.io/highstate-status":         "Success",
  "salt.vcf.io/highstate-time":           "2025-06-01T10:00:00Z",
  "salt.vcf.io/highstate-failed-count":   "0",
  "salt.vcf.io/highstate-failed-states":  ""
}
```

Expected on highstate failure (example):

```json
{
  "salt.vcf.io/salt-status":              "Failed/Highstate",
  "salt.vcf.io/ready":                    "false",
  "salt.vcf.io/highstate-status":         "Failed",
  "salt.vcf.io/highstate-failed-count":   "2",
  "salt.vcf.io/highstate-failed-states":  "pkg_|-install_agent_|-my-agent_|-installed,file_|-config_write_|-/etc/app/config.conf_|-managed"
}
```

The failed state IDs identify exactly which states in the Salt tree need attention.
The full job output (including any error messages) is available in the RaaS UI or via
`salt-run jobs.lookup_jid <jid>` on the Salt master.

**Trigger a Day-2 role change:**

```bash
kubectl annotate vm <vm-name> -n <tenant-namespace> \
  salt.vcf.io/roles='web_server,monitoring_agent,database_client' --overwrite
```

The operator detects the intent-hash change on the next reconcile and re-runs
`refresh_pillar → highstate`. The log will show:

```
Reconvergence triggered, running refresh_pillar + highstate   minionID=<vm-name>
Day-2 highstate dispatched                                    minionID=<vm-name>  jid=<jid>
```

**Retry a VM stuck in a `Failed/<step>` state:**

```bash
kubectl annotate vm <vm-name> -n <tenant-namespace> \
  salt.vcf.io/retry-request="$(date +%s)" --overwrite
```

**Retry every failed VM in a namespace at once:**

```bash
kubectl patch saltkeyconfig default -n <tenant-namespace> --type=merge \
  -p '{"spec":{"retryToken":"'"$(date +%s)"'"}}'
```

Either trigger resets the affected VM to the start of the bootstrap chain. The log
will show:

```
Reconvergence triggered from a terminal state, resetting the Salt chain   minionID=<vm-name>  from=Failed/Highstate
```

---

## Scheduled enforcement

The bootstrap chain and Day-2 detection are both event-driven: a VM converges when it is
created and whenever its intent changes. Nothing re-checks it in between, so
configuration changed in-guest after the last highstate stays changed until something
else happens to trigger a run.

`SaltKeyConfig.spec.enforcementInterval` closes that window. Set it to a duration
(for example `"24h"`) and a VM reporting `salt-status=Ready` whose last highstate
completed longer ago than that interval re-runs `refresh_pillar -> highstate`,
re-asserting the configuration Git describes. It is unset by default, so enforcement
stays purely event-driven until you opt in and upgrading changes nothing for an existing
deployment.

The schedule is derived per VM rather than shared. Each VM's interval is measured from
its own `salt.vcf.io/highstate-time`, so VMs that bootstrapped at different moments come
due at different moments and the load spreads itself across the estate without batching
or rate limiting. VMs onboarded together in one batch would otherwise stay in lockstep,
so a jitter of up to a tenth of the interval is added, derived from the VM's UID and
therefore stable across reconciles and operator restarts rather than drifting on each
evaluation.

Enforcement applies only to VMs reporting `salt-status=Ready`. A VM in a terminal
`Failed/<step>` state is never picked up by the interval, which preserves the rule that
a broken VM stays visibly broken until something explicitly retries it rather than
looping against a failure nobody has looked at.

A malformed duration is rejected by the CRD at admission, so a typo is a failed apply
rather than enforcement that silently never runs.

---

## Scavenger CronJob

The operator uses a Kubernetes finalizer to delete Salt keys when VMs are removed.
If the operator was offline when a VM was deleted the finalizer never fires and the
key remains in RaaS as an orphan.

The **Scavenger CronJob** runs nightly inside the operator namespace and reconciles
this: it lists all accepted keys in RaaS and checks whether any VM in any Supervisor
namespace still carries the matching `salt.vcf.io/minion-id` annotation.

The Scavenger runs in report-only mode by default (`--dry-run=true`): a key with no
correlated VM is logged at error level but never deleted. This also covers a VM whose
CR was recreated after its key was already accepted, which legitimately has no
`salt.vcf.io/minion-id` annotation yet but is still live — surfacing it rather than
deleting it avoids acting on that false positive. Pass `--dry-run=false` to the
CronJob's container args to enable deletion once the report has been reviewed.

Minions with no VirtualMachine by design, such as the Salt master's own minion, are
reported too, and would be deleted with `--dry-run=false`. List them in the install value
`scavenger.excludeMinions` as comma-separated glob patterns, and the Scavenger never
reports or deletes them.

The Scavenger is included in the Supervisor Service bundle and needs no other
configuration.

---

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| Pod `exec format error` on Supervisor | Image built for `arm64` instead of `amd64` | Build on a native `linux/amd64` host; verify with `docker image inspect $IMG --format '{{.Architecture}}'` |
| `ImagePullBackOff: x509: certificate signed by unknown authority` | Supervisor container runtime does not trust the registry CA | vSphere Client → Workload Management → Supervisor → Configure → Image Registry → Trusted CAs → upload the registry CA PEM |
| `ImagePullBackOff: unauthorized` | `imagePullSecret.dockerconfigjson` missing or expired | Regenerate `cat ~/.docker/config.json \| base64 -w0` and reinstall the Service with the fresh value via vSphere Client → Edit values |
| `ytt: control characters are not allowed` on `._001-namespace.yaml` | macOS `tar` embedded AppleDouble sidecars | Re-package with `COPYFILE_DISABLE=1` and `--exclude='._*'` (see air-gap step 1) |
| `kbld: MANIFEST_UNKNOWN` during `supervisor-bundle` | Operator image not in registry | Complete `docker push` before running `make supervisor-bundle` |
| Minion appears as reverse-DNS name instead of VM name | `salt-minion` started before `/etc/salt/minion_id` was written | Pre-seed `minion_id` and add the pre-install purge `runcmd` shown in the VM manifest above |
| Minion registered under wrong ID despite correct `minion_id` file | VM image had `salt-minion` pre-installed with leftover `/etc/salt/pki` | The `runcmd` purge block in the VM manifest handles this — do not omit it |
| `salt.vcf.io/salt-status=Failed/Ping` immediately after acceptance | Minion not yet reconnected after key acceptance | The 30s grace period handles this; if still failing, check that the VM can reach the Salt master on port 4505/4506 |
| `SaltKeyConfig` stays `Ready=False` | RaaS unreachable or credentials wrong | Check `skipTLSVerify` for self-signed certs; verify credentials with `kubectl logs` on the operator pod |
| `SaltKeyConfig` `Ready=False`, reason `ConnectionMismatch` | It names a RaaS URL or master other than the default, or asks to skip TLS verification the default does not allow | Remove `raasURL`, `masterID` and `skipTLSVerify`, or make them match the default |
| `SaltKeyConfig` `Ready=False`, reason `SecretNotFound` | Its credentials Secret does not exist, or on the default connection the install values set no `raas.connection`, so `raas-connection` is missing from the operator's namespace | Create the Secret, or set `raas.connection` in the install values. The config is checked again every 30 seconds |
| `SaltKeyConfig` `Ready=False`, reason `InvalidSelector` | `spec.vmSelector` is not a valid label selector | Fix the selector |
| Event `SaltMasterSilent` on a VM | `test.ping` or `refresh_pillar` failed and the Salt master's presence record in RaaS did not change for a minute or more | The master is down or cut off from RaaS. Fix the master, then retry the VM with `salt.vcf.io/retry-request` |
| Operator upgrade has no effect despite new image push | Same-tag push without bundle rebuild — Supervisor pulls old digest | Run the full build → bundle → service-yaml cycle and use **Upgrade** (not re-install) in vSphere Client |

---

## Development checks

`make render-check` renders the operator offline with ytt for each install mode: Supervisor
Service with and without the tenant Secret read, with preseed, and the VCF Service with and
without preseed enrolment. It asserts each mode's Secret permissions and that the VCF Service
install form offers every schema field and keeps the values a provider entered.

`make local-e2e` installs a lab build through kapp-controller, the installer a Supervisor
uses, into a kind cluster on podman, in both install modes. It runs VMs through discovery,
preseed and the guest signal against a real RaaS, with a Salt minion container standing in
for the guest. VM Operator, the Supervisor's service namespace, its signing rules and the
install screens are not covered.

```bash
make local-e2e-up
make lab-build VERSION=<version> LAB_REGISTRY=<registry>/<project>
make local-e2e VERSION=<version> RAAS_URL=https://<raas-host> RAAS_MASTER_ID=<salt-master-id> \
  RAAS_USERNAME=<user> RAAS_PASSWORD=<password>
make local-e2e-down
```

| Variable | Meaning |
|---|---|
| `RAAS_INSECURE` | skip TLS verification of RaaS, default `true` |
| `SALT_MASTER` | address the minion connects to, default the host of `RAAS_URL` |
| `VM_NAME` | minion ID and VM name, default `e2e-vm-01` |
| `SCAVENGER_EXCLUDE` | scavenger exclude globs, such as the master's own minion ID |

---

## License

Licensed under the [MIT No Attribution License (MIT-0)](LICENSE).
No attribution is required: use, modify, redistribute or rebrand this code freely, with no
obligation to the author and no warranty from the author.

## Disclaimer

This operator interfaces with VMware VCF Salt via its public REST API. VCF Salt and
its API are governed by VMware/Broadcom's licensing terms, not by this MIT-0 licensed
code. This project is not affiliated with or endorsed by VMware or Broadcom.
