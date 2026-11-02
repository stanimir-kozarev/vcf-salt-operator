/*
SPDX-License-Identifier: MIT-0
*/

package controller

import "testing"

func TestComputeIntentHash_Deterministic(t *testing.T) {
	build := func() map[string]string {
		return map[string]string{
			AnnotationRoles:           "web_server,db_server",
			"salt.vcf.io/cis-profile": "prod-strict",
			"salt.vcf.io/environment": "prod",
			"salt.vcf.io/tag/db_port": "5432",
			"salt.vcf.io/vault-path":  "secret/data/prod/web-01",
		}
	}

	first := computeIntentHash(build())
	for i := range 50 {
		if got := computeIntentHash(build()); got != first {
			t.Fatalf("computeIntentHash not deterministic across map instances: run %d got %q, want %q", i, got, first)
		}
	}
}

func TestComputeIntentHash_IgnoresOperatorStatusKeys(t *testing.T) {
	base := map[string]string{
		AnnotationRoles: "web_server",
	}
	withStatus := map[string]string{
		AnnotationRoles:                 "web_server",
		AnnotationHighstateTime:         "2026-08-22T00:00:00Z",
		AnnotationHighstateJID:          "20260822000000000000",
		AnnotationHighstateStatus:       "Success",
		AnnotationHighstateFailedCount:  "0",
		AnnotationHighstateFailedStates: "",
		AnnotationSaltStatus:            "Ready",
		AnnotationReady:                 "true",
		AnnotationKeyStatus:             "Accepted",
		AnnotationMinionID:              "web-01",
		AnnotationMatchMethod:           "name",
		AnnotationAcceptedAt:            "2026-08-22T00:00:00Z",
	}

	if got, want := computeIntentHash(withStatus), computeIntentHash(base); got != want {
		t.Fatalf("operator-written status keys affected the hash: got %q, want %q (same as base)", got, want)
	}
}

func TestComputeIntentHash_IgnoresNonSaltAnnotations(t *testing.T) {
	base := map[string]string{AnnotationRoles: "web_server"}
	withForeign := map[string]string{
		AnnotationRoles: "web_server",
		"kubectl.kubernetes.io/last-applied-configuration": `{"some":"blob"}`,
		"vsphere-tag-manager.vmware.com/last-sync":         "2026-08-22T00:00:00Z",
	}

	if got, want := computeIntentHash(withForeign), computeIntentHash(base); got != want {
		t.Fatalf("non-salt.vcf.io annotation affected the hash: got %q, want %q (same as base)", got, want)
	}
}

func TestComputeIntentHash_DetectsIntentChanges(t *testing.T) {
	cases := []struct {
		name   string
		before map[string]string
		after  map[string]string
	}{
		{
			name:   "roles",
			before: map[string]string{AnnotationRoles: "web_server"},
			after:  map[string]string{AnnotationRoles: "web_server,db_server"},
		},
		{
			name:   "cis-profile",
			before: map[string]string{AnnotationRoles: "web_server", "salt.vcf.io/cis-profile": "dev-standard"},
			after:  map[string]string{AnnotationRoles: "web_server", "salt.vcf.io/cis-profile": "prod-strict"},
		},
		{
			name:   "environment",
			before: map[string]string{AnnotationRoles: "web_server", "salt.vcf.io/environment": "dev"},
			after:  map[string]string{AnnotationRoles: "web_server", "salt.vcf.io/environment": "prod"},
		},
		{
			name:   "tag/db_port",
			before: map[string]string{AnnotationRoles: "db_server", "salt.vcf.io/tag/db_port": "5432"},
			after:  map[string]string{AnnotationRoles: "db_server", "salt.vcf.io/tag/db_port": "5433"},
		},
		{
			name:   "vault-path",
			before: map[string]string{AnnotationRoles: "web_server", "salt.vcf.io/vault-path": "secret/data/dev/web-01"},
			after:  map[string]string{AnnotationRoles: "web_server", "salt.vcf.io/vault-path": "secret/data/prod/web-01"},
		},
		{
			name:   "cis-exceptions/*",
			before: map[string]string{AnnotationRoles: "web_server"},
			after:  map[string]string{AnnotationRoles: "web_server", "salt.vcf.io/cis-exceptions/firewall-open-8080": "prod-baseline-deviation"},
		},
		{
			name:   "role-less VM, cis-profile only",
			before: map[string]string{"salt.vcf.io/cis-profile": "dev-standard"},
			after:  map[string]string{"salt.vcf.io/cis-profile": "prod-strict"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, unchanged := computeIntentHash(tc.after), computeIntentHash(tc.before); got == unchanged {
				t.Fatalf("%s: expected digest to change, got the same value %q for before/after", tc.name, got)
			}
		})
	}
}
