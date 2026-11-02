/*
SPDX-License-Identifier: MIT-0
*/

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	saltv1alpha1 "github.com/stanimir-kozarev/vcf-salt-operator/api/v1alpha1"
	"github.com/stanimir-kozarev/vcf-salt-operator/internal/raasconn"
	"github.com/stanimir-kozarev/vcf-salt-operator/pkg/salt"
)

type fakeSaltClient struct {
	accepted  []string
	deleted   []string
	deleteErr error
}

func (f *fakeSaltClient) Login(_ context.Context) error { return nil }

func (f *fakeSaltClient) ListPendingKeys(_ context.Context) ([]string, error) { return nil, nil }

func (f *fakeSaltClient) ListAcceptedKeys(_ context.Context) ([]string, error) {
	return f.accepted, nil
}

func (f *fakeSaltClient) AcceptKey(_ context.Context, _ string) error { return nil }

func (f *fakeSaltClient) DeleteKey(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return f.deleteErr
}

func (f *fakeSaltClient) TestPing(_ context.Context, _ string) (bool, error) { return true, nil }

func (f *fakeSaltClient) RefreshPillar(_ context.Context, _ string) (bool, error) { return true, nil }

func (f *fakeSaltClient) DispatchHighstate(_ context.Context, _ string) (string, error) {
	return "test-jid", nil
}

func (f *fakeSaltClient) PollJID(_ context.Context, minionID, jid string) (*salt.JIDResult, bool, error) {
	return &salt.JIDResult{MinionID: minionID, JID: jid, Success: true}, true, nil
}

func (f *fakeSaltClient) MasterLastSeen(_ context.Context) (string, error) {
	return "", nil
}

func (f *fakeSaltClient) KeyStates(_ context.Context, _ string) ([]string, error) { return nil, nil }

func (f *fakeSaltClient) GenAcceptKey(_ context.Context, _ string, _ bool) (*salt.KeyPair, error) {
	return nil, nil
}

func (f *fakeSaltClient) MasterPublicKey(_ context.Context) (string, error) { return "", nil }

func mustResolve(t *testing.T, k8sClient client.Reader, cfg *saltv1alpha1.SaltKeyConfig) *raasconn.Connection {
	t.Helper()
	conn, err := raasconn.Resolve(context.Background(), k8sClient, cfg, raasconn.Default{})
	if err != nil {
		t.Fatalf("resolve connection: %v", err)
	}
	return conn
}

func newFakeFactory(fake *fakeSaltClient) saltClientFactory {
	return func(_, _, _, _ string, _ bool, _ logr.Logger) salt.Client {
		return fake
	}
}

func makeVM(name, namespace, id string) *unstructured.Unstructured {
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   vmGVK.Group,
		Version: vmGVK.Version,
		Kind:    vmGVK.Kind,
	})
	vm.SetName(name)
	vm.SetNamespace(namespace)
	if id != "" {
		vm.SetAnnotations(map[string]string{annotationMinionID: id})
	}
	return vm
}

func TestDryRunDefaultsToTrue(t *testing.T) {
	if !defaultDryRun {
		t.Fatal("defaultDryRun must stay true, the scavenger's only correlation signal can " +
			"produce false positives on a recreated VM CR")
	}

	fake := &fakeSaltClient{accepted: []string{"orphan-01"}}

	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := saltv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add saltv1alpha1 scheme: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "team-alpha"},
		Data:       map[string][]byte{"username": []byte("u"), "password": []byte("p")},
	}
	k8sClient := fakeclient.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()

	cfg := &saltv1alpha1.SaltKeyConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: "team-alpha"},
		Spec: saltv1alpha1.SaltKeyConfigSpec{
			RaasURL:           "https://raas.example",
			CredentialsSecret: "creds",
			MasterID:          "master-01",
		},
	}
	g := &raasGroup{conn: mustResolve(t, k8sClient, cfg), namespaces: []string{"team-alpha"}}

	if err := scavengeRaasGroup(context.Background(), k8sClient, g, true, nil, newFakeFactory(fake)); err != nil {
		t.Fatalf("scavengeRaasGroup: %v", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("dry-run must never delete, deleted %v", fake.deleted)
	}
}

func TestScavengeRaasGroup_OnlyDeletesUncorrelatedKeys(t *testing.T) {
	fake := &fakeSaltClient{accepted: []string{"live-01", "orphan-01"}}

	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := saltv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add saltv1alpha1 scheme: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "team-alpha"},
		Data:       map[string][]byte{"username": []byte("u"), "password": []byte("p")},
	}
	vm := makeVM("web-01", "team-alpha", "live-01")
	k8sClient := fakeclient.NewClientBuilder().WithScheme(s).WithObjects(secret).WithRuntimeObjects(vm).Build()

	cfg := &saltv1alpha1.SaltKeyConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: "team-alpha"},
		Spec: saltv1alpha1.SaltKeyConfigSpec{
			RaasURL:           "https://raas.example",
			CredentialsSecret: "creds",
			MasterID:          "master-01",
		},
	}
	g := &raasGroup{conn: mustResolve(t, k8sClient, cfg), namespaces: []string{"team-alpha"}}

	if err := scavengeRaasGroup(context.Background(), k8sClient, g, false, nil, newFakeFactory(fake)); err != nil {
		t.Fatalf("scavengeRaasGroup: %v", err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != "orphan-01" {
		t.Fatalf("expected only orphan-01 deleted, got %v", fake.deleted)
	}
}

func TestScavengeRaasGroup_ExcludedMinionsAreNeverDeleted(t *testing.T) {
	fake := &fakeSaltClient{accepted: []string{"live-01", "saltmaster", "infra-dns", "orphan-01"}}

	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := saltv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add saltv1alpha1 scheme: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "team-alpha"},
		Data:       map[string][]byte{"username": []byte("u"), "password": []byte("p")},
	}
	vm := makeVM("web-01", "team-alpha", "live-01")
	k8sClient := fakeclient.NewClientBuilder().WithScheme(s).WithObjects(secret).WithRuntimeObjects(vm).Build()

	cfg := &saltv1alpha1.SaltKeyConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: "team-alpha"},
		Spec: saltv1alpha1.SaltKeyConfigSpec{
			RaasURL:           "https://raas.example",
			CredentialsSecret: "creds",
			MasterID:          "master-01",
		},
	}
	g := &raasGroup{conn: mustResolve(t, k8sClient, cfg), namespaces: []string{"team-alpha"}}

	exclude := []string{"saltmaster", "infra-*"}
	if err := scavengeRaasGroup(context.Background(), k8sClient, g, false, exclude, newFakeFactory(fake)); err != nil {
		t.Fatalf("scavengeRaasGroup: %v", err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != "orphan-01" {
		t.Fatalf("expected only orphan-01 deleted, got %v", fake.deleted)
	}
}

func TestScavengeRaasGroup_RecreatedCRWithoutAnnotationIsTreatedAsOrphan(t *testing.T) {
	fake := &fakeSaltClient{accepted: []string{"recreated-01"}}

	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := saltv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add saltv1alpha1 scheme: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "team-alpha"},
		Data:       map[string][]byte{"username": []byte("u"), "password": []byte("p")},
	}
	vm := makeVM("web-01", "team-alpha", "")
	k8sClient := fakeclient.NewClientBuilder().WithScheme(s).WithObjects(secret).WithRuntimeObjects(vm).Build()

	cfg := &saltv1alpha1.SaltKeyConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: "team-alpha"},
		Spec: saltv1alpha1.SaltKeyConfigSpec{
			RaasURL:           "https://raas.example",
			CredentialsSecret: "creds",
			MasterID:          "master-01",
		},
	}
	g := &raasGroup{conn: mustResolve(t, k8sClient, cfg), namespaces: []string{"team-alpha"}}

	if err := scavengeRaasGroup(context.Background(), k8sClient, g, true, nil, newFakeFactory(fake)); err != nil {
		t.Fatalf("scavengeRaasGroup: %v", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("dry-run must never delete even the recreated-CR false positive, deleted %v", fake.deleted)
	}
}

func TestScavengeRaasGroup_DeleteFailureIsNotCountedAsDeleted(t *testing.T) {
	fake := &fakeSaltClient{accepted: []string{"orphan-01"}, deleteErr: errors.New("raas unavailable")}

	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := saltv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add saltv1alpha1 scheme: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "team-alpha"},
		Data:       map[string][]byte{"username": []byte("u"), "password": []byte("p")},
	}
	k8sClient := fakeclient.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()

	cfg := &saltv1alpha1.SaltKeyConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: "team-alpha"},
		Spec: saltv1alpha1.SaltKeyConfigSpec{
			RaasURL:           "https://raas.example",
			CredentialsSecret: "creds",
			MasterID:          "master-01",
		},
	}
	g := &raasGroup{conn: mustResolve(t, k8sClient, cfg), namespaces: []string{"team-alpha"}}

	if err := scavengeRaasGroup(context.Background(), k8sClient, g, false, nil, newFakeFactory(fake)); err != nil {
		t.Fatalf("scavengeRaasGroup: %v", err)
	}
	if len(fake.deleted) != 1 {
		t.Fatalf("DeleteKey should still be attempted once, got %v", fake.deleted)
	}
}

func TestScavenge_DefaultConnectionEndpointOnly(t *testing.T) {
	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := saltv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add saltv1alpha1 scheme: %v", err)
	}
	ready := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready"}}
	defaultSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "raas-connection", Namespace: "svc-salt"},
		Data: map[string][]byte{
			"url": []byte("https://raas.platform"), "masterID": []byte("master-01"),
			"username": []byte("u"), "password": []byte("p"),
		},
	}
	usesDefault := &saltv1alpha1.SaltKeyConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "team-alpha"},
		Status:     saltv1alpha1.SaltKeyConfigStatus{Conditions: ready},
	}
	redirects := &saltv1alpha1.SaltKeyConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "team-rogue"},
		Spec:       saltv1alpha1.SaltKeyConfigSpec{RaasURL: "https://attacker.example"},
		Status:     saltv1alpha1.SaltKeyConfigStatus{Conditions: ready},
	}
	k8sClient := fakeclient.NewClientBuilder().WithScheme(s).
		WithObjects(defaultSecret, usesDefault, redirects).
		WithStatusSubresource(&saltv1alpha1.SaltKeyConfig{}).Build()
	for _, c := range []*saltv1alpha1.SaltKeyConfig{usesDefault, redirects} {
		c.Status.Conditions = ready
		if err := k8sClient.Status().Update(context.Background(), c); err != nil {
			t.Fatalf("set status: %v", err)
		}
	}

	var urls []string
	factory := func(url, user, _, _ string, _ bool, _ logr.Logger) salt.Client {
		urls = append(urls, url+" as "+user)
		return &fakeSaltClient{}
	}
	def := raasconn.Default{Namespace: "svc-salt", SecretName: "raas-connection"}
	if err := scavenge(context.Background(), k8sClient, true, nil, def, factory); err != nil {
		t.Fatalf("scavenge: %v", err)
	}
	if len(urls) != 1 || urls[0] != "https://raas.platform as u" {
		t.Fatalf("expected one client for the default endpoint only, got %v", urls)
	}
}
