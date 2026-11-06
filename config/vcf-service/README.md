# VCF Service packaging (VCF 9.1)

The VCF Service wraps the same Carvel payload the Supervisor Service uses, in
`config/supervisor-service/`. One release therefore installs three ways:

| Mode | Platform | Installed through | RaaS connection | `rbac.tenantCredentialSecrets` |
|---|---|---|---|---|
| Supervisor Service | VCF 9.0.x, expected to work, not tested | vCenter, Add New Service, the Service YAML | Install values, or a credentials Secret per namespace | `true` |
| Supervisor Service | VCF 9.1.x | the same | the same | `true` |
| VCF Service | VCF 9.1.x | VCF Automation, Service Management, Partner Services | Install values only | `false`, set by the bundle |

A VCF Service is unsigned, and VCF 9.1 refuses an unsigned service's cluster-wide Secret
reads, so this mode always uses the default connection.

Which versions each mode was tested on is in the main README, Platform support.

## Layout

```
config/vcf-service/
  package.yml               VCF Service PackageMetadata and Package with its values schema
  config/vcf-service.yml    one values Secret and one SupervisorService per region and Supervisor
  config/values.yml         defaults, and the slot the payload Package is injected into
  .values/transpiler.yml    the Service Manager inventory turned into this values schema
  .values/render.yml        example values for a local render, placeholders only
  .imgpkg/images.yml        an empty ImagesLock, the service bundle carries no images
```

## Build for a lab

No cloud and no emulation: the binaries are cross-compiled on the host and the image only
copies them. The build starts a throwaway registry container on port 5050 and removes it
at the end, so no registry needs to be set up first.

### Prerequisites

Install these on the build host first. The build does not install them.

| Tool | Used for |
|---|---|
| Go 1.25.3 or newer, from [go.dev/dl](https://go.dev/dl/) | Compiling the manager and scavenger |
| `make`, `git`, `curl` | The Makefile and its scripts |
| `podman` | Building the image and running the throwaway registry. `lab-build` needs podman, it pushes with `--tls-verify=false`, which Docker does not accept |
| `ytt`, `kbld`, `imgpkg` | Building the payload bundle and the VCF Service bundle |
| `python3` with PyYAML | `make render-check` only |

On Ubuntu, including WSL:

```bash
sudo apt-get update
sudo apt-get install -y podman make git curl python3-yaml
curl -L https://carvel.dev/install.sh | sudo bash
```

On RHEL or Rocky Linux:

```bash
sudo dnf install -y podman make git curl python3-pyyaml
curl -L https://carvel.dev/install.sh | sudo bash
```

On macOS or Windows, podman runs in a VM. Start it with `podman machine start` before the
build. On Linux there is no machine to start.

The build downloads the following. On a host without internet, point `GOPROXY` and the
podman registry mirrors at a local mirror, or bring them in from a connected host.

- Go modules, from `proxy.golang.org`.
- `controller-gen`, which the Makefile installs into `./bin`. `make lint` adds
  `golangci-lint` there, and `make test` adds `setup-envtest` with its Kubernetes binaries.
- The images `docker.io/library/registry:2` and `gcr.io/distroless/static:nonroot`.

### Build

```bash
make lab-build VERSION=0.4.0-rc.1 LAB_REGISTRY=registry.example.com/platform CONTAINER_TOOL=podman
make render-check
```

`LAB_REGISTRY` is the lab registry project the payload is imported into. It has no
default. Three files travel:

| File | What it is |
|---|---|
| `dist/vcf-salt-operator-payload-<version>.tar` | The payload bundle and the operator image, imported into the lab registry |
| `dist/vcf-salt-operator.salt.vcf.io-<version>.tar` | The VCF Service bundle, uploaded to VCF Automation |
| `dist/vcf-salt-operator-supervisorservice-<version>-lab.yaml` | The Service YAML for a standalone Supervisor Service install in vCenter |

Use a pre-release version such as `0.4.0-rc.1` for each test build, so every rebuild is
a distinct Carvel version the platform can upgrade to.

## On the lab

### 1. Import the payload

```bash
imgpkg copy --tar vcf-salt-operator-payload-<version>.tar \
  --to-repo registry.example.com/platform/vcf-salt-operator-bundle
```

The digest it reports must equal the one in the Service YAML and in the VCF Service's
Package. The image references inside the payload still say `localhost:5050`. That is
correct: `imgpkg copy` rewrites the bundle's ImagesLock, and `kbld` substitutes the
relocated digest at install.

### 2. Standalone Supervisor Service, upgrade in place

vCenter, Workload Management, Services, the VCF Salt Operator card, add the new version
from the `-lab.yaml`, then upgrade the Supervisor's install to it. Add to the YAML
Service Config:

```yaml
raas:
  connection:
    url: https://<raas-host>:443
    masterID: <salt-master-id>
    username: <user>
    password: <password>
    insecureSkipVerify: false
```

Check:

```bash
SVCNS=$(kubectl get ns -o name | grep svc-vcf-salt-operator | head -1 | cut -d/ -f2)
kubectl -n "$SVCNS" get deploy,secret,configmap,role,rolebinding
kubectl -n "$SVCNS" logs deploy/controller-manager | grep -E 'Default RaaS connection|Automatic enrolment'
kubectl get saltkeyconfig -A
```

Existing namespaces with their own credentials Secret stay `Ready`. A test namespace
enrolled with an empty `SaltKeyConfig` turns `Ready` with "Using the operator's default
RaaS connection".

### 3. VCF Service

The manually registered Supervisor Service uses the same service ID and must go first.
Uninstalling it deletes every `SaltKeyConfig` cluster-wide, so re-apply enrolment from
Git afterwards, or set `enrolNamespaces`.

1. vCenter: uninstall the Supervisor Service, then confirm `svc-vcf-salt-operator-*` and
   the `vcf-salt-operator-manager` and `vcf-salt-operator-scavenger` ClusterRoles and
   ClusterRoleBindings are gone. Delete the service itself as well, so the VCF Service can
   take its service ID.
2. VCF Automation, Provider Management, Service Management, Partner Services: upload
   `vcf-salt-operator.salt.vcf.io-<version>.tar`, accept the unsigned warning, activate.
3. Install on the region and Supervisor, and enter the install values in the form, in its
   fields or in its YAML view. They reach the service as its values, and `raas-connection`
   appears in the service namespace within a few minutes.
4. For CI/CD pipelines and other automation, set the same values through the Service
   Manager API as the user overwrite, the YAML of the form, base64-encoded:

   ```bash
   VCFA=<vcf-automation-fqdn>
   TOKEN=$(curl -sk -X POST "https://$VCFA/cloudapi/1.0.0/sessions/provider" \
     -H 'Accept: application/json;version=40.0' -u 'admin@System:<password>' -D - -o /dev/null \
     | awk -F': ' 'tolower($1)=="x-vmware-vcloud-access-token"{print $2}' | tr -d '\r')
   SID=urn:vcloud:entity:broadcom:vcfa-service:<uuid>
   OW=$(base64 -w0 < values.yaml)
   curl -sk -X PATCH "https://$VCFA/api/extension/broadcom/service-manager/v2/vcf-services/$SID" \
     -H "Authorization: Bearer $TOKEN" -H 'Accept: application/json;version=40.0' \
     -H 'Content-Type: application/json' -d "{\"values\":{\"userOverwrite\":\"$OW\"}}"
   ```

   `values.yaml` holds `regions`, each with its `supervisors` and a `configuration` block
   carrying `raasURL`, `raasMasterID`, `raasUsername`, `raasPassword`,
   `raasInsecureSkipVerify`, the enrolment fields including `enrolKeyAcceptance`, `preseedKeys`
   and `scavengerExcludeMinions`. The
   service ID is the full URN from `GET /v2/vcf-services`. The short UUID in the
   `service-id` label is refused with 403. The reply is 202 and its `effective` values are
   computed before the change applies, so check the Supervisor instead: `raas-connection`
   appears in the service namespace and the manager and scavenger get their flags within a
   minute. A `504 GATEWAY_TIMEOUT` reply means VCF Automation stopped waiting for its service
   manager, not that the change failed, so check for `raas-connection` before sending it
   again. Sending it again is harmless. On VCF Automation 9.1.0 a plain `GET` of the service
   returns `values` empty whatever is stored.
5. Wait for `Healthy`, then run the checks in step 2. SaltKeyConfigs restored from a
   Supervisor Service install drop `spec.credentialsSecret`, since this mode reads no
   Secret in tenant namespaces.

An upgrade is a new version uploaded the same way and moved to from the installed service.
The upgrade form opens with the installed values. Submit it unchanged, or change what you
need, and the new version gets the values: the operator keeps its connection and its
enrolled `SaltKeyConfig`s throughout. To change values later, use Edit. It opens with the
current values and applies only what you change. A CI/CD pipeline upgrades with `PATCH
/v2/vcf-services/{id}` and the new source. Each version is its own service entry with its
own URN and the user overwrite is stored on the entry, so the new entry starts with no
values. Send the step 4 `PATCH` to its URN as soon as the entry is listed. Until the
values arrive the operator runs with enrolment off and no connection: it removes the
`SaltKeyConfig`s it created, `raas-connection` is removed, the teams' own `SaltKeyConfig`s
turn `Ready=False`, no new VM in those namespaces gets a key, and a deleted VM waits in
`Terminating` until its key is deleted. The operator recreates the enrolled
`SaltKeyConfig`s within a minute of the values arriving, and the teams' own turn `Ready`
again within 30 seconds of `raas-connection` returning. Changing enrolment is a change of
values, through Edit or a new user overwrite. Turning it off removes the `SaltKeyConfig`s
the operator created and leaves the teams' own. VCF Automation leaves the values Secret of
an uninstalled version, `vcf-salt-operator.salt.vcf.io-<version>-config-secret-*` in
`vmware-system-supervisor-services` on the Supervisor, and it holds the RaaS password.

## Assumptions, as measured on VCF 9.1.0

| # | Assumption | Result |
|---|---|---|
| A1 | An unsigned VCF Service may `get` a named Secret in its own namespace | Holds, 2026-10-19. The operator reads `raas-connection` and every `SaltKeyConfig` turns `Ready` |
| A2 | An unsigned service may patch VirtualMachines, create Events in tenant namespaces and hold a Lease | Holds, 2026-10-19. A Day-2 retry ran to `Ready` with its annotations and Events written, under leader election |
| A3 | The platform runs the payload in its generated `svc-*` namespace and the Role and RoleBinding follow the Deployment there | Holds, 2026-10-19 |
| A4 | A `SupervisorService` without overlay labels installs directly | Holds, 2026-10-19. `Healthy` on the `vcf-salt-operator-ready` ConfigMap |
| A5 | The VCF Service may reuse the service ID `vcf-salt-operator.salt.vcf.io` once the Supervisor Service is deleted | Holds, 2026-10-19 |
| A6 | A `format: password` value is passed through the values Secret | Holds through the API user overwrite, 2026-10-19, and through the install form on VCF 9.1.1, 2026-11-06 |
| A7 | Namespace labels can be set on Supervisor namespaces for `enrolNamespaceSelector` | Not measured. Use `enrolNamespaces` by name |
| A8 | Listing namespaces cluster-wide is allowed for an unsigned service | Holds, 2026-09-01, by the `vcf-hello-service` probe |
