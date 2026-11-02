#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER=vso-e2e
CTX=kind-$CLUSTER
REGISTRY=vso-e2e-registry
REGISTRY_HOST_PORT=5051
KAPP_CONTROLLER_VERSION=v0.60.9
MINION_IMAGE=localhost/vso-e2e-minion:3008.3
PKG_NS=vmware-system-supervisor-services
WORK=dist/local-e2e
export KIND_EXPERIMENTAL_PROVIDER=podman

k() { kubectl --context "$CTX" "$@"; }
log() { printf '\n== %s\n' "$*"; }
FAILS=0
pass() { printf 'ok   %s\n' "$*"; }
fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS + 1)); }

wait_for() {
  local timeout=$1 desc=$2; shift 2
  local start=$SECONDS
  until "$@" >/dev/null 2>&1; do
    if ((SECONDS - start >= timeout)); then return 1; fi
    sleep 5
  done
  echo "   $desc after $((SECONDS - start))s"
}

up() {
  mkdir -p "$WORK/certs"
  if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    cat > "$WORK/kind.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: $CLUSTER
containerdConfigPatches:
- |-
  [plugins."io.containerd.grpc.v1.cri".registry]
    config_path = "/etc/containerd/certs.d"
EOF
    kind create cluster --config "$WORK/kind.yaml"
  fi

  if [ ! -f "$WORK/certs/tls.crt" ]; then
    openssl req -x509 -newkey rsa:2048 -nodes -days 30 -keyout "$WORK/certs/tls.key" \
      -out "$WORK/certs/tls.crt" -subj "/CN=$REGISTRY" \
      -addext "subjectAltName=DNS:$REGISTRY,DNS:localhost,IP:127.0.0.1" 2>/dev/null
  fi
  podman rm -f "$REGISTRY" >/dev/null 2>&1 || true
  podman create --name "$REGISTRY" --network kind -p "$REGISTRY_HOST_PORT:5000" \
    -e REGISTRY_HTTP_TLS_CERTIFICATE=/certs/tls.crt \
    -e REGISTRY_HTTP_TLS_KEY=/certs/tls.key docker.io/library/registry:2 >/dev/null
  podman cp "$WORK/certs" "$REGISTRY:/certs"
  podman start "$REGISTRY" >/dev/null
  podman exec "$CLUSTER-control-plane" sh -c "mkdir -p /etc/containerd/certs.d/$REGISTRY:5000 && \
    printf '[host.\"https://$REGISTRY:5000\"]\n  skip_verify = true\n' > /etc/containerd/certs.d/$REGISTRY:5000/hosts.toml"

  k apply -f "https://github.com/carvel-dev/kapp-controller/releases/download/$KAPP_CONTROLLER_VERSION/release.yml" >/dev/null
  k -n kapp-controller create secret generic kapp-controller-config \
    --from-literal=dangerousSkipTLSVerify="$REGISTRY:5000" --dry-run=client -o yaml | k apply -f - >/dev/null
  k -n kapp-controller rollout restart deploy/kapp-controller >/dev/null
  k -n kapp-controller rollout status deploy/kapp-controller --timeout=300s
  k apply -f config/test/virtualmachine-crd.yaml >/dev/null
  k create namespace "$PKG_NS" --dry-run=client -o yaml | k apply -f - >/dev/null
  cat <<EOF | k apply -f - >/dev/null
apiVersion: v1
kind: ServiceAccount
metadata: {name: vso-e2e-installer, namespace: $PKG_NS}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: vso-e2e-installer}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: cluster-admin}
subjects: [{kind: ServiceAccount, name: vso-e2e-installer, namespace: $PKG_NS}]
EOF

  podman build -q -t "$MINION_IMAGE" -f hack/local-e2e/Containerfile.minion hack/local-e2e >/dev/null
  echo "Local e2e rig is up: cluster $CLUSTER, registry localhost:$REGISTRY_HOST_PORT"
}

down() {
  podman ps -a --format '{{.Names}}' | grep '^vso-e2e-vm-' | xargs -r podman rm -f >/dev/null
  podman rm -f "$REGISTRY" >/dev/null 2>&1 || true
  kind delete cluster --name "$CLUSTER"
}

load_package() {
  local tar="dist/vcf-salt-operator-payload-$VERSION.tar"
  [ -f "$tar" ] || { echo "missing $tar, run make lab-build VERSION=$VERSION first" >&2; exit 1; }
  make -s vcf-service-deps VERSION="$VERSION" >/dev/null
  imgpkg copy --tar "$tar" --to-repo "localhost:$REGISTRY_HOST_PORT/vcf-salt-operator-bundle" \
    --registry-verify-certs=false >/dev/null
  local digest
  digest=$(awk '/image:/ {print $2}' dist/supervisor-bundle.lock.yml | sed 's/.*@//')
  sed -e "s#image: .*vcf-salt-operator-bundle[@:].*#image: $REGISTRY:5000/vcf-salt-operator-bundle@$digest#" \
    dist/ss-package/package.yaml | k apply -f - >/dev/null
  k apply -f dist/ss-package/package-metadata.yaml >/dev/null
}

install_payload() {
  local values_file=$1 values_secret
  values_secret="vso-e2e-values-$(shasum -a 256 "$values_file" | cut -c1-10)"
  k -n "$PKG_NS" create secret generic "$values_secret" --from-file=values.yaml="$values_file" \
    --dry-run=client -o yaml | k apply -f - >/dev/null
  cat <<EOF | k apply -f - >/dev/null
apiVersion: packaging.carvel.dev/v1alpha1
kind: PackageInstall
metadata: {name: vcf-salt-operator, namespace: $PKG_NS}
spec:
  serviceAccountName: vso-e2e-installer
  packageRef:
    refName: vcf-salt-operator.salt.vcf.io
    versionSelection: {constraints: "$VERSION", prereleases: {}}
  values:
  - secretRef: {name: $values_secret}
EOF
  if k -n "$PKG_NS" wait pkgi/vcf-salt-operator --for=condition=ReconcileSucceeded --timeout=300s >/dev/null; then
    pass "payload installs through kapp-controller"
  else
    fail "payload installs through kapp-controller"
    k -n "$PKG_NS" get app vcf-salt-operator -o jsonpath='{.status.usefulErrorMessage}' | tail -20
    return 1
  fi
}

uninstall_payload() {
  k -n "$PKG_NS" delete pkgi vcf-salt-operator --wait=true --timeout=300s >/dev/null
  if k get crd saltkeyconfigs.salt.vcf.io >/dev/null 2>&1; then
    fail "uninstall removes the SaltKeyConfig CRD"
  else
    pass "uninstall removes the SaltKeyConfig CRD and with it every SaltKeyConfig"
  fi
}

skc_reason() {
  k -n "$1" get saltkeyconfig salt-config -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}' 2>/dev/null
}

reason_is() { [ "$(skc_reason "$1")" = "$2" ]; }
skc_gone() { ! k -n "$1" get saltkeyconfig "$2" >/dev/null 2>&1; }

expect_reason() {
  local ns=$1 want=$2 desc=$3
  if wait_for 90 "$ns $want" reason_is "$ns" "$want"; then pass "$desc"
  else fail "$desc (reason $(skc_reason "$ns"))"; fi
}

legacy_namespace() {
  local ns=$1
  k create namespace "$ns" --dry-run=client -o yaml | k apply -f - >/dev/null
  cat <<EOF | k apply -f - >/dev/null
apiVersion: v1
kind: Secret
metadata: {name: raas-credentials, namespace: $ns}
stringData: {username: "$RAAS_USERNAME", password: "$RAAS_PASSWORD"}
---
apiVersion: salt.vcf.io/v1alpha1
kind: SaltKeyConfig
metadata: {name: salt-config, namespace: $ns}
spec:
  raasURL: "$RAAS_URL"
  masterID: "$RAAS_MASTER_ID"
  credentialsSecret: raas-credentials
  skipTLSVerify: $RAAS_INSECURE
EOF
}

vm_annotation() {
  k -n "$1" get virtualmachines.vmoperator.vmware.com "$VM_NAME" \
    -o jsonpath="{.metadata.annotations.salt\.vcf\.io/$2}" 2>/dev/null
}

vm_is() { [ "$(vm_annotation "$1" "$2")" = "$3" ]; }
vm_chain_done() { vm_annotation "$1" salt-status | grep -qE '^(Ready|Failed/Highstate)$'; }

vm_flow() {
  local ns=$1 container="vso-e2e-vm-$1"
  podman rm -f "$container" >/dev/null 2>&1 || true
  podman run -d --name "$container" --hostname "$VM_NAME" --privileged --systemd=always \
    "$MINION_IMAGE" >/dev/null
  podman exec "$container" sh -c "printf 'master: $SALT_MASTER\nid: $VM_NAME\n' > /etc/salt/minion.d/e2e.conf && systemctl restart salt-minion"
  local ip
  ip=$(podman inspect "$container" --format '{{.NetworkSettings.IPAddress}}')
  cat <<EOF | k apply -f - >/dev/null
apiVersion: vmoperator.vmware.com/v1alpha4
kind: VirtualMachine
metadata:
  name: $VM_NAME
  namespace: $ns
  annotations:
    salt.vcf.io/managed: "true"
spec: {className: best-effort-small, imageName: vmi-e2e, powerState: PoweredOn}
EOF
  k -n "$ns" patch virtualmachines.vmoperator.vmware.com "$VM_NAME" --subresource=status --type merge \
    -p "{\"status\":{\"phase\":\"Created\",\"powerState\":\"PoweredOn\",\"network\":{\"primaryIP4\":\"$ip\"}}}" >/dev/null

  if wait_for 300 "key accepted" vm_is "$ns" key-status Accepted; then
    pass "$ns: the minion key is accepted through RaaS"
  else
    fail "$ns: the minion key is accepted through RaaS"
  fi
  if wait_for 900 "chain finished" vm_chain_done "$ns"; then
    pass "$ns: ping, refresh_pillar and highstate run, status $(vm_annotation "$ns" salt-status)"
    if [ "$(vm_annotation "$ns" salt-status)" != Ready ]; then
      echo "   highstate failed states: $(vm_annotation "$ns" highstate-failed-states | cut -c1-300)"
    fi
  else
    fail "$ns: ping, refresh_pillar and highstate run, stopped at $(vm_annotation "$ns" salt-status)"
  fi

  k -n "$ns" delete virtualmachines.vmoperator.vmware.com "$VM_NAME" --timeout=180s >/dev/null
  if k -n "$ns" get events --field-selector reason=SaltKeyDeleted -o name | grep -q .; then
    pass "$ns: the key is deleted when the VM is deleted"
  else
    fail "$ns: the key is deleted when the VM is deleted"
  fi
  podman rm -f "$container" >/dev/null
}

vm_flow_preseed() {
  local ns=$1 container="vso-e2e-vm-$1" secret="$VM_NAME-salt-bootstrap"
  local trigger="the first ping" probe=""
  if [ "${2:-}" = signal ]; then
    trigger="the guest signal" probe=", readinessProbe: {guestInfo: [{key: salt-key-written}]}"
  fi
  podman rm -f "$container" >/dev/null 2>&1 || true
  podman run -d --name "$container" --hostname "$VM_NAME" --privileged --systemd=always \
    "$MINION_IMAGE" >/dev/null
  wait_for 60 "container booted" podman exec "$container" systemctl is-active salt-minion || true
  podman exec "$container" sh -c "systemctl stop salt-minion && rm -rf /etc/salt/pki /var/cache/salt/minion /etc/salt/minion_id"
  cat <<EOF | k apply -f - >/dev/null
apiVersion: vmoperator.vmware.com/v1alpha4
kind: VirtualMachine
metadata:
  name: $VM_NAME
  namespace: $ns
  annotations:
    salt.vcf.io/managed: "true"
spec: {className: best-effort-small, imageName: vmi-e2e, powerState: PoweredOn$probe}
EOF

  if wait_for 180 "key preseeded" vm_is "$ns" key-status Preseeded; then
    pass "$ns: the key is generated and accepted before the VM runs"
  else
    fail "$ns: the key is generated and accepted before the VM runs (key-status $(vm_annotation "$ns" key-status))"
    return 0
  fi
  local entry file mode dest preseeded_pub master_pub
  preseeded_pub=$(k -n "$ns" get secret "$secret" -o jsonpath='{.data.minion\.pub}' | base64 -d)
  master_pub=$(k -n "$ns" get secret "$secret" -o jsonpath='{.data.minion_master\.pub}')
  for entry in minion.pem:0400 minion.pub:0644 minion_master.pub:0644 minion_id:0644; do
    file=${entry%%:*} mode=${entry#*:} dest=/etc/salt/pki/minion/${entry%%:*}
    [ "$file" = minion_id ] && dest=/etc/salt/minion_id
    k -n "$ns" get secret "$secret" -o jsonpath="{.data.${file//./\\.}}" | base64 -d \
      | podman exec -i "$container" sh -c "mkdir -p /etc/salt/pki/minion && cat > $dest && chmod $mode $dest"
  done
  if [ -n "$probe" ]; then
    k -n "$ns" patch virtualmachines.vmoperator.vmware.com "$VM_NAME" --subresource=status --type merge \
      -p '{"status":{"conditions":[{"type":"Ready","status":"True"}]}}' >/dev/null
    if wait_for 60 "key cleared" vm_is "$ns" key-cleared true && vm_is "$ns" key-status Preseeded; then
      pass "$ns: the guest signal clears the private key before the VM has an IP"
    else
      fail "$ns: the guest signal clears the private key before the VM has an IP (key-cleared $(vm_annotation "$ns" key-cleared))"
    fi
  fi
  podman exec "$container" sh -c "printf 'master: $SALT_MASTER\n' > /etc/salt/minion.d/e2e.conf && systemctl start salt-minion"
  local ip
  ip=$(podman inspect "$container" --format '{{.NetworkSettings.IPAddress}}')
  k -n "$ns" patch virtualmachines.vmoperator.vmware.com "$VM_NAME" --subresource=status --type merge \
    -p "{\"status\":{\"phase\":\"Created\",\"powerState\":\"PoweredOn\",\"network\":{\"primaryIP4\":\"$ip\"}}}" >/dev/null

  if wait_for 900 "chain finished" vm_chain_done "$ns"; then
    pass "$ns: ping, refresh_pillar and highstate run on the preseeded key, status $(vm_annotation "$ns" salt-status)"
  else
    fail "$ns: ping, refresh_pillar and highstate run on the preseeded key, stopped at $(vm_annotation "$ns" salt-status)"
  fi
  if [ "$preseeded_pub" = "$(podman exec "$container" cat /etc/salt/pki/minion/minion.pub)" ]; then
    pass "$ns: the minion kept the operator's key"
  else
    fail "$ns: the minion kept the operator's key"
  fi
  if [ "$(k -n "$ns" get secret "$secret" -o jsonpath='{.data.minion\.pem}' | base64 -d)" = cleared ] \
     && [ "$(k -n "$ns" get secret "$secret" -o jsonpath='{.data.minion\.pub}' | base64 -d)" = cleared ] \
     && vm_is "$ns" key-cleared true \
     && [ "$(k -n "$ns" get events --field-selector reason=SaltKeyCleared \
          -o jsonpath='{range .items[*]}{.count} {.message}{"\n"}{end}' | grep -c "^1 .*after $trigger")" = 1 ] \
     && [ "$(k -n "$ns" get events --field-selector reason=SaltKeyCleared -o name | wc -l | tr -d ' ')" = 1 ]; then
    pass "$ns: the private key is cleared from the Secret once, after $trigger"
  else
    fail "$ns: the private key is cleared from the Secret once, after $trigger (key-cleared $(vm_annotation "$ns" key-cleared))"
  fi
  if [ -n "$master_pub" ] && [ "$(k -n "$ns" get secret "$secret" -o jsonpath='{.data.minion_master\.pub}')" = "$master_pub" ]; then
    pass "$ns: the clear keeps the master public key in the Secret"
  else
    fail "$ns: the clear keeps the master public key in the Secret"
  fi
  if vm_is "$ns" match-method preseed; then
    pass "$ns: the key was never matched from the pending list"
  else
    fail "$ns: the key was never matched from the pending list (match-method $(vm_annotation "$ns" match-method))"
  fi

  k -n "$ns" delete virtualmachines.vmoperator.vmware.com "$VM_NAME" --timeout=180s >/dev/null
  if k -n "$ns" get events --field-selector reason=SaltKeyDeleted -o name | grep -q .; then
    pass "$ns: the preseeded key is deleted when the VM is deleted"
  else
    fail "$ns: the preseeded key is deleted when the VM is deleted"
  fi
  podman rm -f "$container" >/dev/null
}

test_supervisor_service() {
  log "Supervisor Service mode"
  local opns=svc-vcf-salt-operator-e2e
  cat > "$WORK/values-ss.yaml" <<EOF
namespace: $opns
raas:
  connection:
    url: "$RAAS_URL"
    masterID: "$RAAS_MASTER_ID"
    username: "$RAAS_USERNAME"
    password: "$RAAS_PASSWORD"
    insecureSkipVerify: $RAAS_INSECURE
enrolment:
  namespaces: [e2e-alpha, e2e-beta]
scavenger:
  excludeMinions: "$SCAVENGER_EXCLUDE"
rbac:
  preseedKeys: true
EOF
  install_payload "$WORK/values-ss.yaml" || return 0
  k create namespace e2e-alpha --dry-run=client -o yaml | k apply -f - >/dev/null
  expect_reason e2e-alpha Ready "automatic enrolment on the default connection"
  k create namespace e2e-beta --dry-run=client -o yaml | k apply -f - >/dev/null
  expect_reason e2e-beta Ready "a second namespace is enrolled"
  legacy_namespace e2e-legacy-ss
  expect_reason e2e-legacy-ss Ready "a namespace with its own credentials Secret"
  k create namespace e2e-redirect --dry-run=client -o yaml | k apply -f - >/dev/null
  printf 'apiVersion: salt.vcf.io/v1alpha1\nkind: SaltKeyConfig\nmetadata: {name: salt-config, namespace: e2e-redirect}\nspec: {raasURL: "https://redirect.invalid"}\n' | k apply -f - >/dev/null
  expect_reason e2e-redirect ConnectionMismatch "a namespace pointing at another RaaS is refused"
  vm_flow e2e-alpha
  k create namespace e2e-preseed --dry-run=client -o yaml | k apply -f - >/dev/null
  printf 'apiVersion: salt.vcf.io/v1alpha1\nkind: SaltKeyConfig\nmetadata: {name: salt-config, namespace: e2e-preseed}\nspec: {keyAcceptance: preseed}\n' \
    | k apply -f - >/dev/null
  expect_reason e2e-preseed Ready "a namespace with keyAcceptance preseed"
  vm_flow_preseed e2e-preseed

  printf 'apiVersion: salt.vcf.io/v1alpha1\nkind: SaltKeyConfig\nmetadata: {name: team-own, namespace: e2e-beta}\nspec: {}\n' \
    | k apply -f - >/dev/null
  if wait_for 90 "enrolled config removed" skc_gone e2e-beta salt-config; then
    pass "a team's own config in an enrolled namespace replaces the operator's"
  else
    fail "a team's own config in an enrolled namespace replaces the operator's"
  fi

  grep -v -e '^enrolment:' -e '^  namespaces: \[e2e-alpha, e2e-beta\]' "$WORK/values-ss.yaml" > "$WORK/values-ss-off.yaml"
  install_payload "$WORK/values-ss-off.yaml" || return 0
  if wait_for 180 "enrolled config released" skc_gone e2e-alpha salt-config; then
    pass "turning enrolment off removes the configs it created"
  else
    fail "turning enrolment off removes the configs it created"
  fi
  if skc_gone e2e-legacy-ss salt-config || skc_gone e2e-beta team-own; then
    fail "turning enrolment off leaves the teams' own configs"
  else
    pass "turning enrolment off leaves the teams' own configs"
  fi
  uninstall_payload
}

test_vcf_service() {
  log "VCF Service mode"
  cat > "$WORK/vcf-service-input.yaml" <<EOF
regions:
- name: e2e-region
  supervisors: [e2e-supervisor]
  configuration:
    raasURL: "$RAAS_URL"
    raasMasterID: "$RAAS_MASTER_ID"
    raasUsername: "$RAAS_USERNAME"
    raasPassword: "$RAAS_PASSWORD"
    raasInsecureSkipVerify: $RAAS_INSECURE
    enrolNamespaces: [e2e-gamma]
    enrolKeyAcceptance: preseed
    scavengerExcludeMinions: "$SCAVENGER_EXCLUDE"
EOF
  ytt -f config/vcf-service/config --data-values-file "$WORK/vcf-service-input.yaml" -f dist/deps.yml \
    > "$WORK/vcf-service-rendered.yaml"
  python3 - "$WORK/vcf-service-rendered.yaml" > "$WORK/values-vs.yaml" <<'PY'
import sys, yaml
for d in yaml.safe_load_all(open(sys.argv[1])):
    if d and d.get("kind") == "Secret":
        print(d["stringData"]["values"]); break
PY
  install_payload "$WORK/values-vs.yaml" || return 0
  local opns
  opns=$(k get deploy -A --field-selector metadata.name=controller-manager -o jsonpath='{.items[0].metadata.namespace}')
  k create namespace e2e-gamma --dry-run=client -o yaml | k apply -f - >/dev/null
  expect_reason e2e-gamma Ready "automatic enrolment on the default connection"
  if [ "$(k -n e2e-gamma get saltkeyconfig salt-config -o jsonpath='{.spec.keyAcceptance}')" = preseed ] \
     && [ "$(k auth can-i create secrets -n e2e-gamma --as="system:serviceaccount:$opns:controller-manager")" = yes ] \
     && [ "$(k auth can-i patch secrets -n e2e-gamma --as="system:serviceaccount:$opns:controller-manager")" = yes ] \
     && [ "$(k auth can-i get secrets -n e2e-gamma --as="system:serviceaccount:$opns:controller-manager")" = no ]; then
    pass "the enrolled namespace uses preseed and the operator may write Secrets"
  else
    fail "the enrolled namespace uses preseed and the operator may write Secrets"
  fi
  legacy_namespace e2e-legacy-vs
  expect_reason e2e-legacy-vs GetSecretFailed "a namespace with its own credentials Secret is refused"
  if [ "$(k auth can-i get secrets -n e2e-legacy-vs --as="system:serviceaccount:$opns:controller-manager")" = no ]; then
    pass "the operator cannot read Secrets in tenant namespaces"
  else
    fail "the operator cannot read Secrets in tenant namespaces"
  fi
  if [ "$(k get cm -n "$opns" vcf-salt-operator-ready -o jsonpath='{.data.ready}')" = true ]; then
    pass "the ready ConfigMap the VCF Service waits on is present"
  else
    fail "the ready ConfigMap the VCF Service waits on is present"
  fi

  k -n "$opns" create job vso-e2e-scavenger --from=cronjob/vcf-salt-scavenger >/dev/null
  if k -n "$opns" wait job/vso-e2e-scavenger --for=condition=complete --timeout=300s >/dev/null; then
    pass "the scavenger completes a dry run on the default connection"
    local excluded_hit=""
    for pattern in ${SCAVENGER_EXCLUDE//,/ }; do
      if k -n "$opns" logs job/vso-e2e-scavenger | grep 'no correlated VM' | grep -q "\"minionID\": \"$pattern\""; then
        excluded_hit=$pattern
      fi
    done
    if [ -z "$excluded_hit" ]; then pass "the scavenger leaves excluded minions alone"
    else fail "the scavenger leaves excluded minions alone ($excluded_hit was reported)"; fi
  else
    fail "the scavenger completes a dry run on the default connection"
  fi

  vm_flow_preseed e2e-gamma signal
  uninstall_payload
}

run_test() {
  : "${VERSION:?set VERSION to the lab build version}"
  : "${RAAS_URL:?}" "${RAAS_MASTER_ID:?}" "${RAAS_USERNAME:?}" "${RAAS_PASSWORD:?}"
  RAAS_INSECURE=${RAAS_INSECURE:-true}
  VM_NAME=${VM_NAME:-e2e-vm-01}
  SCAVENGER_EXCLUDE=${SCAVENGER_EXCLUDE:-}
  SALT_MASTER=${SALT_MASTER:-$(echo "$RAAS_URL" | sed -E 's#^[a-z]+://##; s#[:/].*$##')}
  mkdir -p "$WORK"
  k apply -f config/test/virtualmachine-crd.yaml >/dev/null
  load_package
  test_supervisor_service
  for ns in e2e-alpha e2e-beta e2e-legacy-ss e2e-redirect e2e-preseed; do k delete namespace "$ns" --wait=false >/dev/null 2>&1 || true; done
  test_vcf_service
  for ns in e2e-gamma e2e-legacy-vs; do k delete namespace "$ns" --wait=false >/dev/null 2>&1 || true; done
  log "Result"
  if ((FAILS > 0)); then echo "$FAILS check(s) failed"; exit 1; fi
  echo "All checks passed"
}

case "${1:-}" in
  up) up ;;
  test) run_test ;;
  down) down ;;
  *) echo "usage: $0 up|test|down" >&2; exit 2 ;;
esac
