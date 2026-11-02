/*
SPDX-License-Identifier: MIT-0
*/

package raasconn

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	saltv1alpha1 "github.com/stanimir-kozarev/vcf-salt-operator/api/v1alpha1"
)

const opNS = "svc-salt"

var def = Default{Namespace: opNS, SecretName: "raas-connection"}

func newReader(t *testing.T, objs ...*corev1.Secret) *fakeclient.ClientBuilder {
	t.Helper()
	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	b := fakeclient.NewClientBuilder().WithScheme(s)
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	return b
}

func secret(ns, name string, data map[string]string) *corev1.Secret {
	d := map[string][]byte{}
	for k, v := range data {
		d[k] = []byte(v)
	}
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Data: d}
}

func defaultSecret(skipTLS string) *corev1.Secret {
	return secret(opNS, "raas-connection", map[string]string{
		KeyURL: "https://raas.platform", KeyMasterID: "master-01",
		KeyUsername: "u", KeyPassword: "p", KeySkipTLSVerify: skipTLS,
	})
}

func cfg(spec saltv1alpha1.SaltKeyConfigSpec) *saltv1alpha1.SaltKeyConfig {
	return &saltv1alpha1.SaltKeyConfig{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-alpha", Name: "default"},
		Spec:       spec,
	}
}

func reason(t *testing.T, err error) string {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("expected *Error, got %v", err)
	}
	return e.Reason
}

func TestResolve(t *testing.T) {
	tenantCreds := secret("team-alpha", "creds", map[string]string{KeyUsername: "tu", KeyPassword: "tp"})
	cases := []struct {
		name    string
		secrets []*corev1.Secret
		def     Default
		spec    saltv1alpha1.SaltKeyConfigSpec
		reason  string
		wantURL string
		source  string
	}{
		{"default connection", []*corev1.Secret{defaultSecret("")}, def,
			saltv1alpha1.SaltKeyConfigSpec{}, "", "https://raas.platform", SourceDefaultConnection},
		{"default restated exactly", []*corev1.Secret{defaultSecret("")}, def,
			saltv1alpha1.SaltKeyConfigSpec{RaasURL: "https://raas.platform", MasterID: "master-01"},
			"", "https://raas.platform", SourceDefaultConnection},
		{"default with another endpoint", []*corev1.Secret{defaultSecret("")}, def,
			saltv1alpha1.SaltKeyConfigSpec{RaasURL: "https://attacker.example"}, ReasonConnectionMismatch, "", ""},
		{"default with another master", []*corev1.Secret{defaultSecret("")}, def,
			saltv1alpha1.SaltKeyConfigSpec{MasterID: "other"}, ReasonConnectionMismatch, "", ""},
		{"default cannot be downgraded to skip TLS", []*corev1.Secret{defaultSecret("false")}, def,
			saltv1alpha1.SaltKeyConfigSpec{SkipTLSVerify: true}, ReasonConnectionMismatch, "", ""},
		{"default that allows skip TLS", []*corev1.Secret{defaultSecret("true")}, def,
			saltv1alpha1.SaltKeyConfigSpec{SkipTLSVerify: true}, "", "https://raas.platform", SourceDefaultConnection},
		{"no default configured", nil, Default{},
			saltv1alpha1.SaltKeyConfigSpec{}, ReasonDefaultNotSet, "", ""},
		{"default Secret missing", nil, def,
			saltv1alpha1.SaltKeyConfigSpec{}, ReasonSecretNotFound, "", ""},
		{"default Secret incomplete", []*corev1.Secret{secret(opNS, "raas-connection", map[string]string{
			KeyUsername: "u", KeyPassword: "p"})}, def,
			saltv1alpha1.SaltKeyConfigSpec{}, ReasonSecretInvalid, "", ""},
		{"namespace Secret", []*corev1.Secret{tenantCreds, defaultSecret("")}, def,
			saltv1alpha1.SaltKeyConfigSpec{RaasURL: "https://raas.team", MasterID: "m", CredentialsSecret: "creds"},
			"", "https://raas.team", SourceNamespaceSecret},
		{"namespace Secret without endpoint", []*corev1.Secret{tenantCreds}, Default{},
			saltv1alpha1.SaltKeyConfigSpec{CredentialsSecret: "creds"}, ReasonIncompleteSpec, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newReader(t, tc.secrets...).Build()
			conn, err := Resolve(context.Background(), r, cfg(tc.spec), tc.def)
			if tc.reason != "" {
				if got := reason(t, err); got != tc.reason {
					t.Fatalf("reason %q, want %q", got, tc.reason)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if conn.URL != tc.wantURL || conn.Source != tc.source {
				t.Fatalf("got %s from %s, want %s from %s", conn.URL, conn.Source, tc.wantURL, tc.source)
			}
		})
	}
}

func TestDefaultLoad(t *testing.T) {
	ctx := context.Background()

	conn, err := def.Load(ctx, newReader(t, defaultSecret("true")).Build())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if conn.URL != "https://raas.platform" || conn.MasterID != "master-01" || !conn.SkipTLSVerify {
		t.Fatalf("unexpected connection %+v", conn)
	}

	if _, err := def.Load(ctx, newReader(t).Build()); reason(t, err) != ReasonSecretNotFound {
		t.Fatalf("missing Secret: want %s, got %v", ReasonSecretNotFound, err)
	}

	partial := secret(opNS, "raas-connection", map[string]string{KeyURL: "https://raas.platform", KeyUsername: "u"})
	if _, err := def.Load(ctx, newReader(t, partial).Build()); reason(t, err) != ReasonSecretInvalid {
		t.Fatalf("incomplete Secret: want %s, got %v", ReasonSecretInvalid, err)
	}
}
