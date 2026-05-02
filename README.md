# vcf-salt-operator

A Kubernetes operator that automates the full **Salt minion key lifecycle** for VMs
provisioned by [VM Service](https://developer.broadcom.com/apis/vsphere-supervisor-services-and-vm-service)
on a **VCF9 vSphere Supervisor**. It integrates with
[VCF Salt RaaS](https://docs.vmware.com/en/VMware-Aria-Automation-Config/) to accept
Salt keys when VMs are created, run the bootstrap configuration chain, detect and apply
Day-2 role changes, and delete keys when VMs are destroyed.

This operator is purpose-built for VCF9 Supervisor clusters. It is not a generic
Kubernetes operator and does not support plain Kind or kubeadm clusters.

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

If no pending key appears within `acceptTimeout` (default `10m`) the VM is marked
`salt.vcf.io/key-status=Failed` and the operator stops retrying.

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

`test.ping` and `saltutil.refresh_pillar` are retried with a 60-second backoff for up
to 10 minutes from the acceptance timestamp. After that window, failure is terminal
and requires operator intervention. `state.highstate` is dispatched asynchronously;
the JID is stored in `salt.vcf.io/highstate-jid` and polled on subsequent reconciles.

`saltutil.refresh_pillar` is always run before `state.highstate` — this is a
load-bearing invariant. Pillar data is fetched from a Git-backed external pillar; if
highstate ran against a stale pillar the wrong states would be applied.

### Phase 3 — Ready and Day-2

Once highstate succeeds the operator sets `salt.vcf.io/ready=true` and enters the
Day-2 watch loop. On every reconcile it computes a SHA-256 hash of the
`salt.vcf.io/roles` annotation. If the hash differs from the stored
`salt.vcf.io/roles-hash` (written at the last highstate dispatch) a new
`refresh_pillar → highstate` cycle is triggered. This detects role changes pushed by
GitOps (e.g. ArgoCD updating the annotation) without polling — the annotation change
itself generates the watch event.

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
| `salt.vcf.io/key-status` | `Accepted` · `Failed` · `Deleted` · `DeleteFailed` | Key acceptance lifecycle state |
| `salt.vcf.io/minion-id` | string | Salt minion ID matched and accepted |
| `salt.vcf.io/match-method` | `name` · `ip` · `shifted-ip` · `prefix` | Matching strategy used |
| `salt.vcf.io/salt-status` | See phase table above | Post-accept bootstrap phase |
| `salt.vcf.io/ready` | `true` · `false` | Salt management fully complete and healthy |
| `salt.vcf.io/highstate-jid` | JID string | JID of the running or last completed highstate |
| `salt.vcf.io/highstate-status` | `InProgress` · `Success` · `Failed` | Last highstate outcome |
| `salt.vcf.io/highstate-time` | RFC3339 timestamp | Completion time of the last highstate |
| `salt.vcf.io/highstate-failed-count` | integer string | Number of failed states (`0` = all succeeded) |
| `salt.vcf.io/highstate-failed-states` | comma-separated state IDs | Failed state identifiers — contains only state map keys, never rendered values or secrets |
| `salt.vcf.io/roles-hash` | hex string | SHA-256 hash of `salt.vcf.io/roles` at last highstate dispatch |
| `salt.vcf.io/accepted-at` | RFC3339 timestamp | When the key was accepted; drives grace period and retry window |

`salt.vcf.io/highstate-failed-count` and `salt.vcf.io/highstate-failed-states` are
safe to expose to application teams. They contain Salt state IDs (e.g.
`pkg_|-install_agent_|-my-package_|-installed`) — static strings defined in the state
tree with no rendered pillar values, secrets, or command output.

### Set by GitOps / user (read by the operator)

| Annotation | Purpose |
|---|---|
| `salt.vcf.io/managed` | Set to `"true"` to opt a VM into Salt key management |
| `salt.vcf.io/roles` | Comma-separated role list watched for Day-2 changes (e.g. `web_server,monitoring_agent`) |

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

## SaltKeyConfig reference

One `SaltKeyConfig` per enrolled namespace connects the operator to a RaaS endpoint.

```yaml
apiVersion: salt.vcf.io/v1alpha1
kind: SaltKeyConfig
metadata:
  name: default
  namespace: <tenant-namespace>
spec:
  raasURL: https://<raas-host>          # RaaS API base URL
  masterID: <salt-master-id>            # Salt master ID registered in RaaS
  credentialsSecret: <secret-name>      # Secret with keys: username, password
  skipTLSVerify: false                  # Set true for self-signed RaaS certificates
  acceptTimeout: "10m"                  # How long to wait for a pending key before marking Failed
  requeueInterval: "30s"               # How often to re-check for pending keys
```

The `SaltKeyConfigReconciler` validates the referenced Secret and sets
`status.conditions[Ready=True]`. The VM reconciler only acts in namespaces where this
condition is met.

---

## Prerequisites

- **VCF9 Supervisor** with VM Service enabled (API group `vmoperator.vmware.com/v1alpha4`).
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

Set these variables once for your environment before running any command below:

```bash
export IMAGE_REPO=<image-repository>   # e.g. ghcr.io/stanimir-kozarev/vcf-salt-operator
export BUNDLE_REPO=<bundle-repository> # e.g. ghcr.io/stanimir-kozarev/vcf-salt-operator-bundle
export VERSION=<version>               # e.g. 0.1.0
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
  defaultInsecureSkipVerify: false   # set true if RaaS uses a self-signed certificate
```

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
  defaultInsecureSkipVerify: false
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
CRD `saltkeyconfigs.salt.vcf.io` present cluster-wide.

---

## Enrol a namespace

Each Supervisor namespace that should have its VMs managed needs:

1. A `Secret` with RaaS credentials.
2. A `SaltKeyConfig` pointing at the RaaS endpoint.
3. A namespace-scoped RBAC grant so the operator can read the Secret.

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
the next watch event.

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

The operator detects the hash change on the next reconcile and re-runs
`refresh_pillar → highstate`. The log will show:

```
Roles annotation changed, triggering Day-2 refresh_pillar + highstate
Day-2 highstate dispatched   minionID=<vm-name>  jid=<jid>
```

---

## Scavenger CronJob

The operator uses a Kubernetes finalizer to delete Salt keys when VMs are removed.
If the operator was offline when a VM was deleted the finalizer never fires and the
key remains in RaaS as an orphan.

The **Scavenger CronJob** runs nightly inside the operator namespace and reconciles
this: it lists all accepted keys in RaaS, checks whether a corresponding VM still
exists in any Supervisor namespace, and deletes keys whose VMs are gone.

The Scavenger is included in the Supervisor Service bundle and requires no separate
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
| Operator upgrade has no effect despite new image push | Same-tag push without bundle rebuild — Supervisor pulls old digest | Run the full build → bundle → service-yaml cycle and use **Upgrade** (not re-install) in vSphere Client |

---

## License

Copyright (c) 2025 Stan Kozarev. Licensed under the [MIT License](LICENSE).

## Disclaimer

This operator interfaces with VMware VCF Salt via its public REST API. VCF Salt and
its API are governed by VMware/Broadcom's licensing terms, not by this MIT-licensed
code. This project is not affiliated with or endorsed by VMware or Broadcom.
