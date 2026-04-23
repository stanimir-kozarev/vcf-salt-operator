# vcf-salt-operator

A Kubernetes operator that automates **Salt minion key acceptance and cleanup** for VMs managed by
[VM Service](https://developer.broadcom.com/apis/vsphere-supervisor-services-and-vm-service) on vSphere with Tanzu /
VCF. It integrates with [VCF Salt](https://docs.vmware.com/en/VMware-Aria-Automation-Config/)
(RaaS) to accept or delete Salt keys as VMs are created and destroyed.

## How it works

```
VirtualMachine (vmoperator.vmware.com/v1alpha4)
       │  salt.vcf.io/managed=true
       ▼
VirtualMachineReconciler ──► VCF Salt (RaaS) API ──► AcceptKey / DeleteKey
       │  gates on SaltKeyConfig.status.conditions[Ready]=True
       ▼
SaltKeyConfigReconciler ──► validates credentialsSecret + duration fields
```

- **`SaltKeyConfig`** (one per namespace) enrolls a namespace: points at a RaaS endpoint and a
  credentials `Secret` containing `username` and `password`.
- **`VirtualMachineReconciler`** watches VMs cluster-wide. For each VM annotated with
  `salt.vcf.io/managed=true` it finds the matching pending Salt key (by name, IP, shifted-IP, or
  prefix) and accepts it. On VM deletion it deletes the key.
- **Scavenger CronJob** (`cmd/scavenger`) runs nightly and removes accepted keys whose VMs no
  longer exist — handling the case where the operator was offline during a deletion.

### Annotations written by the operator

| Annotation | Values |
|---|---|
| `salt.vcf.io/key-status` | `Accepted` \| `Failed` \| `Deleted` \| `DeleteFailed` |
| `salt.vcf.io/minion-id` | matched Salt minion ID |
| `salt.vcf.io/match-method` | `name` \| `ip` \| `shifted-ip` \| `prefix` |

## Getting Started

### Prerequisites

- Go v1.24+
- kubectl v1.27+
- Access to a Kubernetes cluster with VM Service (vSphere with Tanzu / VCF)
- A VCF Salt (RaaS) instance reachable from the cluster

### Deploy

**1. Build and push the image:**

```sh
export IMG=ghcr.io/<your-org>/vcf-salt-operator:latest
make docker-build docker-push IMG=$IMG
```

**2. Install CRDs and deploy the operator:**

```sh
make install
make deploy IMG=$IMG
```

> If you encounter RBAC errors, ensure you have cluster-admin privileges.

**3. Create a credentials Secret and a `SaltKeyConfig` in each target namespace:**

```sh
kubectl apply -k config/samples/
```

The sample creates a `SaltKeyConfig` and a `salt-keys-credentials` Secret.
Edit `config/samples/` to set your actual RaaS URL and credentials.

**4. Opt a VM into management:**

```sh
kubectl annotate virtualmachine <vm-name> salt.vcf.io/managed=true
```

### Validate connectivity (optional)

Use the `raas-test` CLI to test credentials against a live RaaS instance before deploying:

```sh
go run ./cmd/raas-test \
  --url https://aria-config.corp:443 \
  --user admin --password secret \
  --action list-pending
```

Actions: `list-pending`, `list-accepted`, `accept --minion <id>`, `delete --minion <id>`.

### Uninstall

```sh
kubectl delete -k config/samples/
make undeploy
make uninstall
```

## Project Distribution

### Deploy as a VCF9 Supervisor Service (recommended)

A vSphere Namespace user cannot create cluster-scoped resources on a VCF9
Supervisor, so the operator ships as a **Supervisor Service** installed by the
vSphere admin. Sources and the full install/relocate guide live in
[`config/supervisor-service/`](config/supervisor-service/README.md).

```sh
# Build + push operator image and bundle, emit the upload YAML.
make supervisor-release VERSION=0.1.0 \
    IMG=ghcr.io/<your-org>/vcf-salt-operator:0.1.0

# Upload dist/vcf-salt-operator-supervisorservice-0.1.0.yaml via
# vSphere Client -> Workload Management -> Services -> Add New Service.
```

For a private registry (e.g. Nexus), use `make supervisor-relocate` — see the
[Supervisor Service README](config/supervisor-service/README.md).

### Development install (Kind / cluster-admin cluster)

For iteration on Kind or any cluster where you have cluster-admin:

```sh
make build-installer-vcf9 IMG=ghcr.io/<your-org>/vcf-salt-operator:<tag>
kubectl apply -f dist/install-vcf9.yaml
```

`kubectl apply` as a vSphere Namespace user against a real VCF9 Supervisor
will fail on cluster-scoped resources; use the Supervisor Service flow.

## Development

```sh
make test          # unit + controller tests (envtest)
make lint-fix      # auto-fix lint issues
make run           # run locally against current kubeconfig context
```

**NOTE:** Run `make help` for all available targets.

More information: [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

Copyright (c) 2025 Stan Kozarev. Licensed under the [MIT License](LICENSE).

## Disclaimer

This operator interfaces with VMware VCF Salt via its public REST API.
VCF Salt and its API are governed by VMware/Broadcom's licensing terms, not by this
MIT-licensed code. This project is not affiliated with or endorsed by VMware or Broadcom.
