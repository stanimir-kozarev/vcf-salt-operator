#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

PAYLOAD=config/supervisor-service/bundle/config
OUT=$(mktemp -d)
trap 'rm -rf "$OUT"' EXIT

ytt -f "$PAYLOAD" > "$OUT/mode1.yaml"
ytt -f "$PAYLOAD" --data-value-yaml rbac.tenantCredentialSecrets=false \
    -v raas.connection.url=https://raas.example -v raas.connection.masterID=m \
    -v raas.connection.username=u -v raas.connection.password=p > "$OUT/mode2.yaml"
ytt -f "$PAYLOAD" --data-value-yaml rbac.tenantCredentialSecrets=false --data-value-yaml rbac.preseedKeys=true \
    -v raas.connection.url=https://raas.example -v raas.connection.masterID=m \
    -v raas.connection.username=u -v raas.connection.password=p > "$OUT/preseed.yaml"
ytt -f config/vcf-service/config --data-values-file config/vcf-service/.values/render.yml \
    -f dist/deps.yml > "$OUT/vcfservice.yaml"
python3 - "$OUT/vcfservice.yaml" > "$OUT/mode3-values.yaml" <<'PY'
import sys, yaml
for d in yaml.safe_load_all(open(sys.argv[1])):
    if d and d.get("kind") == "Secret":
        print(d["stringData"]["values"]); break
PY
ytt -f "$PAYLOAD" --data-values-file "$OUT/mode3-values.yaml" > "$OUT/mode3.yaml"
sed 's/preseedKeys: false/preseedKeys: true/' config/vcf-service/.values/render.yml > "$OUT/render-preseed.yml"
ytt -f config/vcf-service/config --data-values-file "$OUT/render-preseed.yml" -f dist/deps.yml \
  | python3 -c 'import sys, yaml
for d in yaml.safe_load_all(sys.stdin):
    if d and d.get("kind") == "Secret": print(d["stringData"]["values"]); break' > "$OUT/mode3-preseed-values.yaml"
ytt -f "$PAYLOAD" --data-values-file "$OUT/mode3-preseed-values.yaml" > "$OUT/mode3-preseed.yaml"
sed 's/enrolKeyAcceptance: discovery/enrolKeyAcceptance: preseed/' config/vcf-service/.values/render.yml > "$OUT/render-enrol-preseed.yml"
ytt -f config/vcf-service/config --data-values-file "$OUT/render-enrol-preseed.yml" -f dist/deps.yml \
  | python3 -c 'import sys, yaml
for d in yaml.safe_load_all(sys.stdin):
    if d and d.get("kind") == "Secret": print(d["stringData"]["values"]); break' > "$OUT/mode5-values.yaml"
ytt -f "$PAYLOAD" --data-values-file "$OUT/mode5-values.yaml" > "$OUT/mode5.yaml"
ytt -f "$PAYLOAD" -v enrolment.keyAcceptance=preseed > "$OUT/preseed-not-enrolled.yaml"
TRANSPILER=config/vcf-service/.values/transpiler.yml
printf 'regions:\n- name: r1\n  supervisors:\n  - name: s1\n' > "$OUT/inv-bare.yml"
printf 'inventory:\n  regions:\n  - name: r1\n    supervisors:\n    - name: s1\n' > "$OUT/inv-nested.yml"
ytt -f "$TRANSPILER" --data-values-file "$OUT/inv-bare.yml" > "$OUT/form-bare.yaml"
ytt -f "$TRANSPILER" --data-values-file "$OUT/inv-nested.yml" > "$OUT/form-nested.yaml"
ytt -f "$TRANSPILER" --data-values-file config/vcf-service/.values/render.yml > "$OUT/form-entered.yaml"

python3 - "$OUT" <<'PY'
import sys, yaml, os
out = sys.argv[1]
def load(name):
    return [d for d in yaml.safe_load_all(open(os.path.join(out, name))) if d]
def cluster_secret_rules(docs):
    return [r for d in docs if d["kind"] == "ClusterRole"
            for r in d["rules"] if "secrets" in r.get("resources", [])]
def find(docs, kind, name=None):
    return [d for d in docs if d["kind"] == kind and (name is None or d["metadata"]["name"] == name)]
def args(docs):
    return find(docs, "Deployment")[0]["spec"]["template"]["spec"]["containers"][0]["args"]
fails = []
def check(cond, msg):
    print(("ok   " if cond else "FAIL ") + msg)
    if not cond: fails.append(msg)

m1, m2, m3, vs = load("mode1.yaml"), load("mode2.yaml"), load("mode3.yaml"), load("vcfservice.yaml")
check(len(cluster_secret_rules(m1)) == 2, "mode 1: both ClusterRoles keep the tenant Secret read")
check(all(r["verbs"] == ["get"] for r in cluster_secret_rules(m1)), "mode 1: the tenant Secret read is get only")
check(not find(m1, "Secret", "raas-connection"), "mode 1: no connection Secret without install values")
for name, docs in (("mode 2", m2), ("mode 3", m3)):
    check(not cluster_secret_rules(docs), name + ": no ClusterRole grants any Secret access")
    check(bool(find(docs, "Secret", "raas-connection")), name + ": the connection Secret is rendered")
for name, docs in (("mode 1", m1), ("mode 2", m2), ("mode 3", m3)):
    role = find(docs, "Role", "vcf-salt-operator-connection-reader")
    check(bool(role) and role[0]["rules"][0].get("resourceNames") == ["raas-connection"],
          name + ": the own-namespace Role reads the connection Secret only")
    check(bool(find(docs, "ConfigMap", "vcf-salt-operator-ready")), name + ": the ready ConfigMap is rendered")
    check("--default-connection-secret=raas-connection" in args(docs), name + ": the manager gets the connection flag")
pre = load("preseed.yaml")
pre_rules = [(d["metadata"]["name"], r["verbs"]) for d in find(pre, "ClusterRole")
             for r in d["rules"] if "secrets" in r.get("resources", [])]
check(pre_rules == [("vcf-salt-operator-manager", ["create", "update", "patch"])],
      "preseed: only the manager may create, update and patch Secrets, and nothing reads them")
check(not any(set(r["verbs"]) & {"create", "update", "patch"}
              for docs in (m1, m2, m3) for r in cluster_secret_rules(docs)),
      "modes 1 to 3: no Secret write without preseedKeys")
m3p = [(d["metadata"]["name"], r["verbs"]) for d in find(load("mode3-preseed.yaml"), "ClusterRole")
       for r in d["rules"] if "secrets" in r.get("resources", [])]
check(m3p == [("vcf-salt-operator-manager", ["create", "update", "patch"])],
      "mode 3: preseedKeys from the install form reaches the manager's Secret write, still no read")
check("--enrol-namespaces=team-alpha" in args(m3), "mode 3: enrolment reaches the manager")
check("--enrol-key-acceptance=discovery" in args(m1) and "--enrol-key-acceptance=discovery" in args(m3),
      "modes 1 and 3: enrolled namespaces default to discovery")
m5 = load("mode5.yaml")
check("--enrol-key-acceptance=preseed" in args(m5), "mode 5: enrolKeyAcceptance from the install form reaches the manager")
m5r = [(d["metadata"]["name"], r["verbs"]) for d in find(m5, "ClusterRole")
       for r in d["rules"] if "secrets" in r.get("resources", [])]
check(m5r == [("vcf-salt-operator-manager", ["create", "update", "patch"])],
      "mode 5: preseed enrolment grants the manager's Secret write without preseedKeys, still no read")
check(not any(set(r["verbs"]) & {"create", "update", "patch"}
              for r in cluster_secret_rules(load("preseed-not-enrolled.yaml"))),
      "preseed key acceptance with no namespace enrolled grants no Secret write")
check(any("namespaces" in r.get("resources", []) for d in find(m3, "ClusterRole") for r in d["rules"]),
      "mode 3: enrolment adds the namespace read")
check(not any("namespaces" in r.get("resources", []) for d in find(m1, "ClusterRole") for r in d["rules"]),
      "mode 1: no namespace read without enrolment")
def scavenger_args(docs):
    return find(docs, "CronJob")[0]["spec"]["jobTemplate"]["spec"]["template"]["spec"]["containers"][0]["args"]
check(not any(a.startswith("--exclude-minions") for a in scavenger_args(m1)), "mode 1: no scavenger exclusion by default")
check("--exclude-minions=saltmaster" in scavenger_args(m3), "mode 3: the scavenger exclusion reaches the scavenger")
ss = find(vs, "SupervisorService")
check(len(ss) == 1 and ss[0]["spec"]["package"] is not None, "VCF Service: one SupervisorService with its Package")
check(ss and ss[0]["spec"]["readyConditions"][0]["supervisorSelector"]["name"] == "vcf-salt-operator-ready",
      "VCF Service: the ready condition reads the payload's ConfigMap")
pkg = [d for d in yaml.safe_load_all(open("config/vcf-service/package.yml")) if d and d["kind"] == "Package"][0]
fields = set(pkg["spec"]["valuesSchema"]["openAPIv3"]["properties"]["regions"]["items"]
             ["properties"]["configuration"]["properties"])
for shape in ("bare", "nested"):
    form = load("form-" + shape + ".yaml")[0]["regions"]
    check(len(form) == 1 and form[0]["supervisors"] == ["s1"],
          "install form, " + shape + " inventory: the region and its Supervisor carry over")
    check(bool(form) and set(form[0]["configuration"]) == fields,
          "install form, " + shape + " inventory: every schema field is offered")
entered = yaml.safe_load(open("config/vcf-service/.values/render.yml"))["regions"][0]["configuration"]
kept = load("form-entered.yaml")[0]["regions"][0]["configuration"]
check(kept == entered, "install form: the values the provider entered are kept")
sys.exit(1 if fails else 0)
PY
