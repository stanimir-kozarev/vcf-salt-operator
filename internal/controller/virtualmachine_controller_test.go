/*
Copyright (c) 2025 Stan Kozarev.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/go-logr/logr"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	saltv1alpha1 "github.com/stanimir-kozarev/vcf-salt-operator/api/v1alpha1"
	"github.com/stanimir-kozarev/vcf-salt-operator/pkg/salt"
)

// mockSaltClient is a test double for salt.Client.
// It records which methods were called and returns pre-configured values.
type mockSaltClient struct {
	LoginErr     error
	PendingKeys  []string
	ListErr      error
	AcceptedKeys []string
	AcceptErr    error
	DeletedKeys  []string
	DeleteErr    error
}

func (m *mockSaltClient) Login(_ context.Context) error { return m.LoginErr }

func (m *mockSaltClient) ListPendingKeys(_ context.Context) ([]string, error) {
	return m.PendingKeys, m.ListErr
}

func (m *mockSaltClient) ListAcceptedKeys(_ context.Context) ([]string, error) {
	return nil, nil
}

func (m *mockSaltClient) AcceptKey(_ context.Context, id string) error {
	m.AcceptedKeys = append(m.AcceptedKeys, id)
	return m.AcceptErr
}

func (m *mockSaltClient) DeleteKey(_ context.Context, id string) error {
	m.DeletedKeys = append(m.DeletedKeys, id)
	return m.DeleteErr
}

func (m *mockSaltClient) TestPing(_ context.Context, _ string) (bool, error) { return true, nil }

func (m *mockSaltClient) RefreshPillar(_ context.Context, _ string) (bool, error) { return true, nil }

func (m *mockSaltClient) DispatchHighstate(_ context.Context, _ string) (string, error) {
	return "mock-jid-00000000000000", nil
}

func (m *mockSaltClient) PollJID(_ context.Context, minionID, jid string) (*salt.JIDResult, bool, error) {
	return &salt.JIDResult{
		MinionID: minionID,
		JID:      jid,
		Fun:      "state.highstate",
		Return:   []byte(`{}`),
		Retcode:  0,
		Success:  true,
	}, true, nil
}

// newTestReconciler creates a VirtualMachineReconciler wired to the mock Salt client and envtest k8sClient.
func newTestReconciler(mock *mockSaltClient) *VirtualMachineReconciler {
	return &VirtualMachineReconciler{
		Client: k8sClient,
		SaltClientFactory: func(_, _, _, _ string, _ bool, _ logr.Logger) salt.Client {
			return mock
		},
		Recorder: record.NewFakeRecorder(32),
	}
}

// mergeAnnotations returns a new map with overlay's keys layered on top of base's,
// without mutating either input.
func mergeAnnotations(base, overlay map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(overlay))
	maps.Copy(out, base)
	maps.Copy(out, overlay)
	return out
}

// makeVM creates a minimal VirtualMachine as unstructured in the given namespace.
func makeVM(name, namespace string, annotations map[string]string) *unstructured.Unstructured {
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "vmoperator.vmware.com",
		Version: "v1alpha4",
		Kind:    "VirtualMachine",
	})
	vm.SetName(name)
	vm.SetNamespace(namespace)
	if annotations != nil {
		vm.SetAnnotations(annotations)
	}
	return vm
}

// setVMStatus updates the VM's status subresource with the given phase and IP.
// It also populates status.powerState (v1alpha4) so the controller recognizes
// "Running" in both legacy and current VM Operator API versions.
func setVMStatus(ctx context.Context, vm *unstructured.Unstructured, phase, ip string) {
	GinkgoHelper()
	powerState := "PoweredOff"
	if phase == "Running" {
		powerState = "PoweredOn"
	}
	statusPatch := map[string]any{
		"status": map[string]any{
			"phase":      phase,
			"powerState": powerState,
			"network": map[string]any{
				"primaryIP4": ip,
			},
		},
	}
	vmCopy := vm.DeepCopy()
	Expect(unstructured.SetNestedField(vmCopy.Object, statusPatch["status"], "status")).To(Succeed())
	Expect(k8sClient.Status().Update(ctx, vmCopy)).To(Succeed())
}

// setVMStatusV1Alpha4 updates the VM's status subresource with only the
// v1alpha4 shape (powerState + network), omitting legacy phase.
func setVMStatusV1Alpha4(ctx context.Context, vm *unstructured.Unstructured, powerState, ip string) {
	GinkgoHelper()
	statusPatch := map[string]any{
		"status": map[string]any{
			"powerState": powerState,
			"network": map[string]any{
				"primaryIP4": ip,
			},
		},
	}
	vmCopy := vm.DeepCopy()
	Expect(unstructured.SetNestedField(vmCopy.Object, statusPatch["status"], "status")).To(Succeed())
	Expect(k8sClient.Status().Update(ctx, vmCopy)).To(Succeed())
}

// makeSaltKeyConfig creates a SaltKeyConfig + its credentials Secret in the given namespace,
// then patches the status to Ready=True so findSaltKeyConfig returns it immediately.
func makeSaltKeyConfig(ctx context.Context, namespace string) {
	GinkgoHelper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "salt-raas-creds",
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"username": []byte("admin"),
			"password": []byte("secret"),
		},
	}
	Expect(k8sClient.Create(ctx, secret)).To(Succeed())

	cfg := &saltv1alpha1.SaltKeyConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "salt-config",
			Namespace: namespace,
		},
		Spec: saltv1alpha1.SaltKeyConfigSpec{
			RaasURL:           "https://aria-config.test:443",
			CredentialsSecret: "salt-raas-creds",
			MasterID:          "test-master",
			AcceptTimeout:     "2s",
			RequeueInterval:   "1s",
		},
	}
	Expect(k8sClient.Create(ctx, cfg)).To(Succeed())

	// Simulate the SaltKeyConfigReconciler having run: set Ready=True on the status.
	original := cfg.DeepCopy()
	cfg.Status.Conditions = []metav1.Condition{{
		Type:               ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonReady,
		Message:            "Credentials Secret found and valid",
		LastTransitionTime: metav1.Now(),
	}}
	Expect(k8sClient.Status().Patch(ctx, cfg, client.MergeFrom(original))).To(Succeed())
}

var _ = Describe("VirtualMachine Controller", func() {
	const testNamespace = "default"

	var (
		vmName     string
		mock       *mockSaltClient
		reconciler *VirtualMachineReconciler
		req        reconcile.Request
	)

	BeforeEach(func() {
		vmName = "test-vm-" + randomSuffix()
		mock = &mockSaltClient{}
		reconciler = newTestReconciler(mock)
		req = reconcile.Request{NamespacedName: types.NamespacedName{Name: vmName, Namespace: testNamespace}}
	})

	Context("Opt-in annotation filter", func() {
		It("should skip VMs without the managed annotation", func() {
			vm := makeVM(vmName, testNamespace, nil)
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(mock.AcceptedKeys).To(BeEmpty())
		})

		It("should skip VMs with managed=false", func() {
			vm := makeVM(vmName, testNamespace, map[string]string{AnnotationManaged: "false"})
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(mock.AcceptedKeys).To(BeEmpty())
		})
	})

	Context("Namespace enrollment check", func() {
		It("should skip VMs in unenrolled namespaces (no SaltKeyConfig)", func() {
			vm := makeVM(vmName, testNamespace, map[string]string{AnnotationManaged: "true"})
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(mock.AcceptedKeys).To(BeEmpty())
		})
	})

	Context("Create flow (Phase 5)", func() {
		var saltCfgNS string

		BeforeEach(func() {
			// Use a unique namespace per test to avoid 409 conflicts across specs
			saltCfgNS = "create-" + randomSuffix()
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: saltCfgNS}}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ns) })
			makeSaltKeyConfig(ctx, saltCfgNS)
			req.Namespace = saltCfgNS
		})

		It("should requeue when VM phase is not Running", func() {
			vm := makeVM(vmName, saltCfgNS, map[string]string{AnnotationManaged: "true"})
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

			setVMStatus(ctx, vm, "PoweredOff", "")

			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
			Expect(mock.AcceptedKeys).To(BeEmpty())
		})

		It("should requeue when VM is PoweredOn but has no primary IP yet (v1alpha4)", func() {
			vm := makeVM(vmName, saltCfgNS, map[string]string{AnnotationManaged: "true"})
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

			setVMStatusV1Alpha4(ctx, vm, "PoweredOn", "")

			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
			Expect(mock.AcceptedKeys).To(BeEmpty())
		})

		It("should accept key on v1alpha4 VM with powerState=PoweredOn and primaryIP4", func() {
			mock.PendingKeys = []string{vmName}

			vm := makeVM(vmName, saltCfgNS, map[string]string{AnnotationManaged: "true"})
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

			setVMStatusV1Alpha4(ctx, vm, "PoweredOn", "10.0.0.7")

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(mock.AcceptedKeys).To(ContainElement(vmName))
		})

		It("should accept key when VM is Running and pending key matches by name", func() {
			mock.PendingKeys = []string{vmName, "other-vm"}

			vm := makeVM(vmName, saltCfgNS, map[string]string{AnnotationManaged: "true"})
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

			setVMStatus(ctx, vm, "Running", "10.0.0.5")

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(mock.AcceptedKeys).To(ContainElement(vmName))

			// Verify annotations were patched onto the VM
			updated := &unstructured.Unstructured{}
			updated.SetGroupVersionKind(virtualMachineGVK)
			Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
			Expect(updated.GetAnnotations()[AnnotationKeyStatus]).To(Equal("Accepted"))
			Expect(updated.GetAnnotations()[AnnotationMinionID]).To(Equal(vmName))
			Expect(updated.GetAnnotations()[AnnotationMatchMethod]).To(Equal(string(salt.MatchByName)))
			Expect(updated.GetFinalizers()).To(ContainElement(saltFinalizer))
		})

		It("should accept key matching by IP when name does not match", func() {
			mock.PendingKeys = []string{"10.0.1.100"}

			vm := makeVM(vmName, saltCfgNS, map[string]string{AnnotationManaged: "true"})
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

			setVMStatus(ctx, vm, "Running", "10.0.1.100")

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(mock.AcceptedKeys).To(ContainElement("10.0.1.100"))

			updated := &unstructured.Unstructured{}
			updated.SetGroupVersionKind(virtualMachineGVK)
			Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
			Expect(updated.GetAnnotations()[AnnotationMatchMethod]).To(Equal(string(salt.MatchByIP)))
		})

		It("should be idempotent: skip when already Accepted and finalizer present", func() {
			vm := makeVM(vmName, saltCfgNS, map[string]string{
				AnnotationManaged:   "true",
				AnnotationKeyStatus: "Accepted",
				AnnotationMinionID:  vmName,
			})
			vm.SetFinalizers([]string{saltFinalizer})
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() {
				latest := &unstructured.Unstructured{}
				latest.SetGroupVersionKind(virtualMachineGVK)
				if err := k8sClient.Get(ctx, req.NamespacedName, latest); err == nil {
					p := client.MergeFrom(latest.DeepCopy())
					latest.SetFinalizers(nil)
					_ = k8sClient.Patch(ctx, latest, p)
					_ = k8sClient.Delete(ctx, latest)
				}
			})

			setVMStatus(ctx, vm, "Running", "10.0.0.5")

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(mock.AcceptedKeys).To(BeEmpty(), "should not call AcceptKey when already Accepted")
		})

		It("should re-add finalizer when VM is Accepted but finalizer is absent (upgrade safety)", func() {
			vm := makeVM(vmName, saltCfgNS, map[string]string{
				AnnotationManaged:   "true",
				AnnotationKeyStatus: "Accepted",
				AnnotationMinionID:  vmName,
			})
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() {
				latest := &unstructured.Unstructured{}
				latest.SetGroupVersionKind(virtualMachineGVK)
				if err := k8sClient.Get(ctx, req.NamespacedName, latest); err == nil {
					p := client.MergeFrom(latest.DeepCopy())
					latest.SetFinalizers(nil)
					_ = k8sClient.Patch(ctx, latest, p)
					_ = k8sClient.Delete(ctx, latest)
				}
			})

			setVMStatus(ctx, vm, "Running", "10.0.0.5")

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(mock.AcceptedKeys).To(BeEmpty(), "should not call AcceptKey when already Accepted")

			updated := &unstructured.Unstructured{}
			updated.SetGroupVersionKind(virtualMachineGVK)
			Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
			Expect(updated.GetFinalizers()).To(ContainElement(saltFinalizer), "finalizer should be re-added")
		})

		It("should requeue when no pending key matches yet (within timeout)", func() {
			mock.PendingKeys = []string{"completely-different-vm"}

			vm := makeVM(vmName, saltCfgNS, map[string]string{AnnotationManaged: "true"})
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

			setVMStatus(ctx, vm, "Running", "10.0.0.5")

			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
			Expect(mock.AcceptedKeys).To(BeEmpty())
		})

		It("should mark Failed after accept timeout expires", func() {
			mock.PendingKeys = []string{}

			// Create VM with a very old creation timestamp to simulate timeout elapsed.
			// We do this by creating the VM, waiting for envtest to assign a real timestamp,
			// then reconciling with a config that has already-expired AcceptTimeout.
			vm := makeVM(vmName, saltCfgNS, map[string]string{AnnotationManaged: "true"})
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

			setVMStatus(ctx, vm, "Running", "10.0.0.5")

			// SaltKeyConfig has AcceptTimeout=2s — wait for it to elapse
			time.Sleep(3 * time.Second)

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			updated := &unstructured.Unstructured{}
			updated.SetGroupVersionKind(virtualMachineGVK)
			Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
			Expect(updated.GetAnnotations()[AnnotationKeyStatus]).To(Equal("Failed"))
		})

		It("should return error on RaaS ListPendingKeys failure", func() {
			mock.ListErr = errors.New("raas connection refused")

			vm := makeVM(vmName, saltCfgNS, map[string]string{AnnotationManaged: "true"})
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

			setVMStatus(ctx, vm, "Running", "10.0.0.5")

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("raas connection refused"))
		})
	})

	Context("Day-2 change detection (full intent-annotation hash)", func() {
		var saltCfgNS string

		BeforeEach(func() {
			saltCfgNS = "day2-" + randomSuffix()
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: saltCfgNS}}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ns) })
			makeSaltKeyConfig(ctx, saltCfgNS)
			req.Namespace = saltCfgNS
		})

		// makeReadyVM builds a VM already past bootstrap (salt-status=Ready), with
		// AnnotationRolesHash set to whatever computeIntentHash would have produced for
		// baseAnnotations, the state as of the last successful highstate dispatch, then
		// returns it with liveAnnotations layered on top, simulating a subsequent Argo CD
		// sync that changed (or didn't change) the tenant-declared annotations.
		makeReadyVM := func(baseAnnotations, liveAnnotations map[string]string) *unstructured.Unstructured {
			GinkgoHelper()
			// AnnotationManaged is itself part of the tenant-declared intent set (not in
			// nonIntentAnnotations, see its own doc comment) and is unconditionally
			// present on any VM that reaches checkReconvergence at all (Reconcile's opt-in gate
			// requires it). The baseline hash a real prior dispatch would have stored
			// therefore always included it too. Omitting it here would make every "no
			// change" fixture look changed, since the live object always carries it.
			previousHash := computeIntentHash(mergeAnnotations(map[string]string{AnnotationManaged: "true"}, baseAnnotations))

			annotations := mergeAnnotations(map[string]string{
				AnnotationManaged:    "true",
				AnnotationKeyStatus:  "Accepted",
				AnnotationMinionID:   vmName,
				AnnotationSaltStatus: "Ready",
				AnnotationReady:      "true",
			}, liveAnnotations)
			annotations[AnnotationRolesHash] = previousHash

			vm := makeVM(vmName, saltCfgNS, annotations)
			vm.SetFinalizers([]string{saltFinalizer})
			return vm
		}

		It("should not dispatch when no intent annotation changed", func() {
			state := map[string]string{AnnotationRoles: "web_server"}
			vm := makeReadyVM(state, state)
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			updated := &unstructured.Unstructured{}
			updated.SetGroupVersionKind(virtualMachineGVK)
			Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
			Expect(updated.GetAnnotations()[AnnotationSaltStatus]).To(Equal("Ready"),
				"unchanged intent set must not trigger a Day-2 dispatch")
		})

		It("should still dispatch on a roles change (pre-existing behavior, not regressed)", func() {
			old := map[string]string{AnnotationRoles: "web_server"}
			live := map[string]string{AnnotationRoles: "web_server,db_server"}
			vm := makeReadyVM(old, live)
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			updated := &unstructured.Unstructured{}
			updated.SetGroupVersionKind(virtualMachineGVK)
			Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
			Expect(updated.GetAnnotations()[AnnotationSaltStatus]).To(Equal("HighstateDispatched"))
			Expect(updated.GetAnnotations()[AnnotationHighstateJID]).To(Equal("mock-jid-00000000000000"))
		})

		It("should dispatch on a cis-profile change with roles held constant", func() {
			old := map[string]string{AnnotationRoles: "web_server", "salt.vcf.io/cis-profile": "dev-standard"}
			live := map[string]string{AnnotationRoles: "web_server", "salt.vcf.io/cis-profile": "prod-strict"}
			vm := makeReadyVM(old, live)
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			updated := &unstructured.Unstructured{}
			updated.SetGroupVersionKind(virtualMachineGVK)
			Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
			Expect(updated.GetAnnotations()[AnnotationSaltStatus]).To(Equal("HighstateDispatched"),
				"a cis-profile-only change was previously invisible to the roles-only hash")
		})

		// No through-envtest "tag/* change" case here deliberately: "salt.vcf.io/tag/db_port"
		// is not a valid Kubernetes annotation key. The API server's annotation key regex
		// allows exactly one '/', splitting an optional DNS prefix from the name, and
		// "tag/db_port" as the name segment contains a second '/', so real VM creation with
		// that key is rejected outright (422 FieldValueInvalid). computeIntentHash's own
		// behavior on an arbitrary map key, irrespective of Kubernetes validity, is still
		// covered directly by TestComputeIntentHash_DetectsIntentChanges/tag/db_port in
		// intent_hash_test.go.

		It("should dispatch on a role-less VM whose cis-profile changes", func() {
			old := map[string]string{"salt.vcf.io/cis-profile": "dev-standard"}
			live := map[string]string{"salt.vcf.io/cis-profile": "prod-strict"}
			vm := makeReadyVM(old, live)
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			updated := &unstructured.Unstructured{}
			updated.SetGroupVersionKind(virtualMachineGVK)
			Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
			Expect(updated.GetAnnotations()[AnnotationSaltStatus]).To(Equal("HighstateDispatched"),
				"a role-less VM still carries the unconditional CIS baseline, so it must not be "+
					"excluded from Day-2 drift detection just because AnnotationRoles is empty")
		})

		It("should not dispatch on a change to an operator-written status annotation", func() {
			old := map[string]string{AnnotationRoles: "web_server"}
			// highstate-time is operator-written. Changing it must never itself trigger a
			// dispatch, or every completed highstate would immediately re-trigger itself.
			live := map[string]string{AnnotationRoles: "web_server", AnnotationHighstateTime: "2026-08-22T00:00:00Z"}
			vm := makeReadyVM(old, live)
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			updated := &unstructured.Unstructured{}
			updated.SetGroupVersionKind(virtualMachineGVK)
			Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
			Expect(updated.GetAnnotations()[AnnotationSaltStatus]).To(Equal("Ready"))
		})
	})

	Context("Recovery from a terminal Failed state", func() {
		var saltCfgNS string

		BeforeEach(func() {
			saltCfgNS = "recover-" + randomSuffix()
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: saltCfgNS}}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ns) })
			makeSaltKeyConfig(ctx, saltCfgNS)
			req.Namespace = saltCfgNS
		})

		// makeFailedVM builds a VM stranded in a terminal Failed/<step> state, with its
		// stored intent hash matching baseAnnotations (the state as of the dispatch that
		// failed) and liveAnnotations layered on top.
		makeFailedVM := func(failedStatus string, baseAnnotations, liveAnnotations map[string]string) *unstructured.Unstructured {
			GinkgoHelper()
			previousHash := computeIntentHash(mergeAnnotations(map[string]string{AnnotationManaged: "true"}, baseAnnotations))
			annotations := mergeAnnotations(map[string]string{
				AnnotationManaged:         "true",
				AnnotationKeyStatus:       "Accepted",
				AnnotationMinionID:        vmName,
				AnnotationSaltStatus:      failedStatus,
				AnnotationReady:           "false",
				AnnotationHighstateStatus: "Failed",
			}, liveAnnotations)
			annotations[AnnotationRolesHash] = previousHash
			vm := makeVM(vmName, saltCfgNS, annotations)
			vm.SetFinalizers([]string{saltFinalizer})
			return vm
		}

		setBulkRetryToken := func(token string) {
			GinkgoHelper()
			cfg := &saltv1alpha1.SaltKeyConfig{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "salt-config", Namespace: saltCfgNS}, cfg)).To(Succeed())
			original := cfg.DeepCopy()
			cfg.Spec.RetryToken = token
			Expect(k8sClient.Patch(ctx, cfg, client.MergeFrom(original))).To(Succeed())
		}

		getVM := func() *unstructured.Unstructured {
			GinkgoHelper()
			updated := &unstructured.Unstructured{}
			updated.SetGroupVersionKind(virtualMachineGVK)
			Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
			return updated
		}

		It("should stay stranded when nothing has changed and no retry was requested", func() {
			state := map[string]string{AnnotationRoles: "web_server"}
			vm := makeFailedVM("Failed/Highstate", state, state)
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(getVM().GetAnnotations()[AnnotationSaltStatus]).To(Equal("Failed/Highstate"),
				"a genuinely broken VM must stay visibly broken, never silently auto-retry")
		})

		It("should reset when intent changed while the VM was stranded", func() {
			// The IDP case: a team's first request failed, and their second, entirely valid
			// request arrives through the same sanctioned path. This was previously accepted
			// by Git and then silently ignored by the operator.
			old := map[string]string{AnnotationRoles: "web_server"}
			live := map[string]string{AnnotationRoles: "web_server,db_server"}
			vm := makeFailedVM("Failed/Highstate", old, live)
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			annotations := getVM().GetAnnotations()
			Expect(annotations[AnnotationSaltStatus]).To(BeEmpty(), "should re-enter the chain at the start")
			Expect(annotations[AnnotationReady]).To(Equal("false"))
			Expect(annotations[AnnotationRolesHash]).NotTo(Equal(computeIntentHash(
				mergeAnnotations(map[string]string{AnnotationManaged: "true"}, old))),
				"the intent hash must be recorded at reset time, or a re-failing run loops forever")
		})

		DescribeTable("should recover from every terminal step via a retry request",
			func(failedStatus string) {
				state := map[string]string{AnnotationRoles: "web_server"}
				vm := makeFailedVM(failedStatus, state,
					mergeAnnotations(state, map[string]string{AnnotationRetryRequest: "CHG0041234"}))
				Expect(k8sClient.Create(ctx, vm)).To(Succeed())
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

				_, err := reconciler.Reconcile(ctx, req)
				Expect(err).NotTo(HaveOccurred())

				annotations := getVM().GetAnnotations()
				Expect(annotations[AnnotationSaltStatus]).To(BeEmpty())
				Expect(annotations[AnnotationRetryHandled]).To(Equal("CHG0041234"))
				Expect(annotations[AnnotationRetryRequest]).To(Equal("CHG0041234"),
					"the operator must never clear the request, or it fights Argo CD's selfHeal")
			},
			Entry("Failed/Ping", "Failed/Ping"),
			Entry("Failed/PillarRefresh", "Failed/PillarRefresh"),
			Entry("Failed/HighstateDispatch", "Failed/HighstateDispatch"),
			Entry("Failed/Highstate", "Failed/Highstate"),
		)

		It("should not act twice on a retry token it already handled", func() {
			// The loop guard: a retry that fails again lands back in Failed/<step>. If the
			// handled marker were written only on success, this VM would retry forever.
			state := map[string]string{AnnotationRoles: "web_server"}
			vm := makeFailedVM("Failed/Highstate", state, mergeAnnotations(state, map[string]string{
				AnnotationRetryRequest: "CHG0041234",
				AnnotationRetryHandled: "CHG0041234",
			}))
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(getVM().GetAnnotations()[AnnotationSaltStatus]).To(Equal("Failed/Highstate"))
		})

		It("should rescue a stranded VM when the namespace bulk retry token changes", func() {
			state := map[string]string{AnnotationRoles: "web_server"}
			vm := makeFailedVM("Failed/Highstate", state, state)
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })
			setBulkRetryToken("CHG0099999")

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			annotations := getVM().GetAnnotations()
			Expect(annotations[AnnotationSaltStatus]).To(BeEmpty())
			Expect(annotations[AnnotationBulkRetryHandled]).To(Equal("CHG0099999"))
		})

		It("should not re-run a healthy VM on a bulk retry, but should consume the token", func() {
			// Bulk retry rescues stranded VMs only, which is what makes "retry this
			// environment" safe rather than a fleet-wide convergence. Consuming the token
			// stops it re-arming against a VM that fails weeks later for another reason.
			state := map[string]string{AnnotationRoles: "web_server"}
			annotations := mergeAnnotations(map[string]string{
				AnnotationManaged:    "true",
				AnnotationKeyStatus:  "Accepted",
				AnnotationMinionID:   vmName,
				AnnotationSaltStatus: "Ready",
				AnnotationReady:      "true",
			}, state)
			annotations[AnnotationRolesHash] = computeIntentHash(
				mergeAnnotations(map[string]string{AnnotationManaged: "true"}, state))
			vm := makeVM(vmName, saltCfgNS, annotations)
			vm.SetFinalizers([]string{saltFinalizer})
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })
			setBulkRetryToken("CHG0099999")

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			updated := getVM().GetAnnotations()
			Expect(updated[AnnotationSaltStatus]).To(Equal("Ready"), "a healthy VM must not be disturbed")
			Expect(updated[AnnotationBulkRetryHandled]).To(Equal("CHG0099999"), "but must consume the token")
		})

		It("should force a re-run on a healthy VM when a retry is explicitly requested", func() {
			// The break-glass follow-up: Ops repaired the VM out of band, and the operator's
			// own record has to catch up with a reality it never observed.
			state := map[string]string{AnnotationRoles: "web_server"}
			annotations := mergeAnnotations(map[string]string{
				AnnotationManaged:      "true",
				AnnotationKeyStatus:    "Accepted",
				AnnotationMinionID:     vmName,
				AnnotationSaltStatus:   "Ready",
				AnnotationReady:        "true",
				AnnotationRetryRequest: "CHG0042000",
			}, state)
			annotations[AnnotationRolesHash] = computeIntentHash(
				mergeAnnotations(map[string]string{AnnotationManaged: "true"}, state))
			vm := makeVM(vmName, saltCfgNS, annotations)
			vm.SetFinalizers([]string{saltFinalizer})
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			updated := getVM().GetAnnotations()
			Expect(updated[AnnotationSaltStatus]).To(Equal("HighstateDispatched"))
			Expect(updated[AnnotationRetryHandled]).To(Equal("CHG0042000"))
		})

		// Scheduled enforcement (finding D). The interval corrects drift introduced outside
		// Git between events. The tests that matter most here are the ones asserting it does
		// NOT fire: enabled by accident, it would dispatch a highstate against every Ready VM
		// in every enrolled namespace.
		Describe("Scheduled enforcement", func() {
			setEnforcementInterval := func(interval string) {
				GinkgoHelper()
				cfg := &saltv1alpha1.SaltKeyConfig{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "salt-config", Namespace: saltCfgNS}, cfg)).To(Succeed())
				original := cfg.DeepCopy()
				cfg.Spec.EnforcementInterval = interval
				Expect(k8sClient.Patch(ctx, cfg, client.MergeFrom(original))).To(Succeed())
			}

			// makeReadyVM builds a settled, healthy VM whose last highstate completed
			// lastHighstateAge ago.
			makeReadyVM := func(lastHighstateAge time.Duration) *unstructured.Unstructured {
				GinkgoHelper()
				state := map[string]string{AnnotationRoles: "web_server"}
				annotations := mergeAnnotations(map[string]string{
					AnnotationManaged:         "true",
					AnnotationKeyStatus:       "Accepted",
					AnnotationMinionID:        vmName,
					AnnotationSaltStatus:      "Ready",
					AnnotationReady:           "true",
					AnnotationHighstateStatus: "Success",
					AnnotationHighstateTime: time.Now().UTC().
						Add(-lastHighstateAge).Format(time.RFC3339),
				}, state)
				annotations[AnnotationRolesHash] = computeIntentHash(
					mergeAnnotations(map[string]string{AnnotationManaged: "true"}, state))
				vm := makeVM(vmName, saltCfgNS, annotations)
				vm.SetFinalizers([]string{saltFinalizer})
				return vm
			}

			It("should never fire on a VM stranded in a terminal Failed state", func() {
				// The single most important property. checkReconvergence deliberately refuses
				// automatic retries so a broken VM stays visibly broken instead of looping
				// against a failure nobody has looked at. An interval that swept up Failed
				// VMs would silently undo that.
				setEnforcementInterval("1s")
				state := map[string]string{AnnotationRoles: "web_server"}
				vm := makeFailedVM("Failed/Highstate", state, mergeAnnotations(state, map[string]string{
					AnnotationHighstateTime: time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339),
				}))
				Expect(k8sClient.Create(ctx, vm)).To(Succeed())
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

				_, err := reconciler.Reconcile(ctx, req)
				Expect(err).NotTo(HaveOccurred())
				Expect(getVM().GetAnnotations()[AnnotationSaltStatus]).To(Equal("Failed/Highstate"),
					"scheduled enforcement must leave terminal failures alone, not auto-retry them")
			})

			It("should not fire when no interval is configured", func() {
				// Every SaltKeyConfig that predates this field lands here. Behaviour must be
				// identical to before the feature existed.
				vm := makeReadyVM(365 * 24 * time.Hour)
				Expect(k8sClient.Create(ctx, vm)).To(Succeed())
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

				result, err := reconciler.Reconcile(ctx, req)
				Expect(err).NotTo(HaveOccurred())
				Expect(getVM().GetAnnotations()[AnnotationSaltStatus]).To(Equal("Ready"),
					"a VM with no enforcementInterval must stay dormant regardless of how old its last highstate is")
				Expect(result.RequeueAfter).To(BeZero(),
					"and must not be requeued, or the operator would poll every Ready VM forever for nothing")
			})

			It("should not fire before the interval has elapsed, and should requeue", func() {
				setEnforcementInterval("24h")
				vm := makeReadyVM(time.Hour)
				Expect(k8sClient.Create(ctx, vm)).To(Succeed())
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

				result, err := reconciler.Reconcile(ctx, req)
				Expect(err).NotTo(HaveOccurred())
				Expect(getVM().GetAnnotations()[AnnotationSaltStatus]).To(Equal("Ready"))
				Expect(result.RequeueAfter).To(BeNumerically(">", 22*time.Hour),
					"a VM not yet due must come back on its own when it is")
			})

			It("should reject a malformed interval at the API rather than silently disabling it", func() {
				// enforcementDue treats an unparseable value as "disabled", which is the safe
				// reading but a silent one. The CRD pattern is what makes the mistake visible
				// to whoever wrote it, so a typo is a rejected apply rather than enforcement
				// that quietly never runs.
				cfg := &saltv1alpha1.SaltKeyConfig{
					ObjectMeta: metav1.ObjectMeta{Name: "bad-interval", Namespace: saltCfgNS},
					Spec: saltv1alpha1.SaltKeyConfigSpec{
						RaasURL:             "https://aria-config.test:443",
						CredentialsSecret:   "salt-raas-creds",
						MasterID:            "test-master",
						EnforcementInterval: "24 hours",
					},
				}
				err := k8sClient.Create(ctx, cfg)
				Expect(err).To(HaveOccurred(), "a malformed duration must not reach the controller at all")
				Expect(err.Error()).To(ContainSubstring("enforcementInterval"))
			})

			It("should re-run the chain once the interval has elapsed", func() {
				setEnforcementInterval("24h")
				vm := makeReadyVM(48 * time.Hour)
				Expect(k8sClient.Create(ctx, vm)).To(Succeed())
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

				_, err := reconciler.Reconcile(ctx, req)
				Expect(err).NotTo(HaveOccurred())

				updated := getVM().GetAnnotations()
				Expect(updated[AnnotationSaltStatus]).To(Equal("HighstateDispatched"),
					"an overdue Ready VM must re-run the full chain, not just be marked")
				Expect(updated[AnnotationHighstateStatus]).To(Equal("InProgress"))
			})
		})
	})

	Context("Intent hash format migration", func() {
		var saltCfgNS string

		BeforeEach(func() {
			saltCfgNS = "migrate-" + randomSuffix()
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: saltCfgNS}}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ns) })
			makeSaltKeyConfig(ctx, saltCfgNS)
			req.Namespace = saltCfgNS
		})

		makeReadyVMWithHash := func(roles, storedHash string) *unstructured.Unstructured {
			GinkgoHelper()
			vm := makeVM(vmName, saltCfgNS, map[string]string{
				AnnotationManaged:    "true",
				AnnotationKeyStatus:  "Accepted",
				AnnotationMinionID:   vmName,
				AnnotationSaltStatus: "Ready",
				AnnotationReady:      "true",
				AnnotationRoles:      roles,
				AnnotationRolesHash:  storedHash,
			})
			vm.SetFinalizers([]string{saltFinalizer})
			return vm
		}

		It("should adopt a pre-upgrade hash in place without dispatching", func() {
			// Without this, every VM in the fleet mismatches on first reconcile after the
			// operator upgrade and converges at once, which is an unannounced mass action.
			vm := makeReadyVMWithHash("web_server", computeLegacyRolesHash("web_server"))
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			updated := &unstructured.Unstructured{}
			updated.SetGroupVersionKind(virtualMachineGVK)
			Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
			Expect(updated.GetAnnotations()[AnnotationSaltStatus]).To(Equal("Ready"),
				"migration must not dispatch")
			Expect(updated.GetAnnotations()[AnnotationRolesHash]).To(HavePrefix(intentHashVersion))
		})

		It("should still dispatch when roles genuinely changed before the upgrade", func() {
			// Suppressing the difference blindly would swallow a real change made while the
			// old operator was running, so the old algorithm is recomputed to tell them apart.
			vm := makeReadyVMWithHash("web_server,db_server", computeLegacyRolesHash("web_server"))
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			updated := &unstructured.Unstructured{}
			updated.SetGroupVersionKind(virtualMachineGVK)
			Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
			Expect(updated.GetAnnotations()[AnnotationSaltStatus]).To(Equal("HighstateDispatched"))
		})
	})

	Context("Delete flow (Phase 6)", func() {
		var saltCfgNS string

		BeforeEach(func() {
			saltCfgNS = "delete-" + randomSuffix()
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: saltCfgNS}}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ns) })
			makeSaltKeyConfig(ctx, saltCfgNS)
			req.Namespace = saltCfgNS
		})

		It("should delete key and annotate Deleted when DeletionTimestamp is set", func() {
			vm := makeVM(vmName, saltCfgNS, map[string]string{
				AnnotationManaged:   "true",
				AnnotationKeyStatus: "Accepted",
				AnnotationMinionID:  vmName,
			})
			// Add finalizer so delete doesn't immediately remove the object
			vm.SetFinalizers([]string{"test.vcf.io/hold"})
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())

			// Trigger deletion
			Expect(k8sClient.Delete(ctx, vm)).To(Succeed())

			// Fetch with DeletionTimestamp set
			updated := &unstructured.Unstructured{}
			updated.SetGroupVersionKind(virtualMachineGVK)
			Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
			Expect(updated.GetDeletionTimestamp()).NotTo(BeNil())

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(mock.DeletedKeys).To(ContainElement(vmName))

			// Remove finalizer for cleanup
			patch := client.MergeFrom(updated.DeepCopy())
			updated.SetFinalizers(nil)
			_ = k8sClient.Patch(ctx, updated, patch)
		})

		It("should remove operator finalizer after successful key deletion", func() {
			vm := makeVM(vmName, saltCfgNS, map[string]string{
				AnnotationManaged:   "true",
				AnnotationKeyStatus: "Accepted",
				AnnotationMinionID:  vmName,
			})
			vm.SetFinalizers([]string{"test.vcf.io/hold", saltFinalizer})
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			Expect(k8sClient.Delete(ctx, vm)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(mock.DeletedKeys).To(ContainElement(vmName))

			updated := &unstructured.Unstructured{}
			updated.SetGroupVersionKind(virtualMachineGVK)
			Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
			Expect(updated.GetFinalizers()).NotTo(ContainElement(saltFinalizer))

			patch := client.MergeFrom(updated.DeepCopy())
			updated.SetFinalizers(nil)
			_ = k8sClient.Patch(ctx, updated, patch)
		})

		It("should skip key deletion when no MinionID annotation is present", func() {
			vm := makeVM(vmName, saltCfgNS, map[string]string{AnnotationManaged: "true"})
			vm.SetFinalizers([]string{"test.vcf.io/hold"})
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			Expect(k8sClient.Delete(ctx, vm)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(mock.DeletedKeys).To(BeEmpty(), "should not call DeleteKey if no MinionID annotation")

			updated := &unstructured.Unstructured{}
			updated.SetGroupVersionKind(virtualMachineGVK)
			Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
			patch := client.MergeFrom(updated.DeepCopy())
			updated.SetFinalizers(nil)
			_ = k8sClient.Patch(ctx, updated, patch)
		})

		It("should annotate DeleteFailed but not error when RaaS delete fails", func() {
			mock.DeleteErr = errors.New("key not found")

			vm := makeVM(vmName, saltCfgNS, map[string]string{
				AnnotationManaged:  "true",
				AnnotationMinionID: vmName,
			})
			vm.SetFinalizers([]string{"test.vcf.io/hold"})
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			Expect(k8sClient.Delete(ctx, vm)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred(), "fire-and-forget: delete errors must not propagate")

			updated := &unstructured.Unstructured{}
			updated.SetGroupVersionKind(virtualMachineGVK)
			Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
			Expect(updated.GetAnnotations()[AnnotationKeyStatus]).To(Equal("DeleteFailed"))

			patch := client.MergeFrom(updated.DeepCopy())
			updated.SetFinalizers(nil)
			_ = k8sClient.Patch(ctx, updated, patch)
		})
	})
})

var _ = Describe("VirtualMachine SaltKeyConfig watch mapper", func() {
	var (
		ns    string
		nsObj *corev1.Namespace
	)

	BeforeEach(func() {
		ns = "vm-mapper-" + randomSuffix()
		nsObj = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, nsObj) })
	})

	It("should enqueue managed VMs when their SaltKeyConfig changes", func() {
		vmName := "mapper-vm-" + randomSuffix()
		vm := makeVM(vmName, ns, map[string]string{AnnotationManaged: "true"})
		Expect(k8sClient.Create(ctx, vm)).To(Succeed())

		cfg := &saltv1alpha1.SaltKeyConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: ns},
			Spec: saltv1alpha1.SaltKeyConfigSpec{
				RaasURL:           "https://aria.test:443",
				CredentialsSecret: "creds",
				MasterID:          "test-master",
			},
		}
		Expect(k8sClient.Create(ctx, cfg)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cfg) })

		r := newTestReconciler(&mockSaltClient{})
		reqs := r.saltKeyConfigToVMs(ctx, cfg)
		Expect(reqs).To(HaveLen(1))
		Expect(reqs[0].NamespacedName).To(Equal(types.NamespacedName{Name: vmName, Namespace: ns}))
	})

	It("should return no requests when no managed VMs exist in the namespace", func() {
		vmName := "unmanaged-vm-" + randomSuffix()
		vm := makeVM(vmName, ns, map[string]string{AnnotationManaged: "false"})
		Expect(k8sClient.Create(ctx, vm)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

		cfg := &saltv1alpha1.SaltKeyConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "cfg2", Namespace: ns},
			Spec: saltv1alpha1.SaltKeyConfigSpec{
				RaasURL:           "https://aria.test:443",
				CredentialsSecret: "creds",
				MasterID:          "test-master",
			},
		}
		Expect(k8sClient.Create(ctx, cfg)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cfg) })

		r := newTestReconciler(&mockSaltClient{})
		reqs := r.saltKeyConfigToVMs(ctx, cfg)
		Expect(reqs).To(BeEmpty())
	})
})

var _ = Describe("parseDurationOrDefault", func() {
	It("returns fallback for empty string", func() {
		Expect(parseDurationOrDefault("", 5*time.Second)).To(Equal(5 * time.Second))
	})
	It("returns parsed duration for valid string", func() {
		Expect(parseDurationOrDefault("10m", 5*time.Second)).To(Equal(10 * time.Minute))
	})
	It("returns fallback for invalid duration string", func() {
		Expect(parseDurationOrDefault("not-a-duration", 5*time.Second)).To(Equal(5 * time.Second))
	})
})

// randomSuffix generates a guaranteed-unique suffix via an atomic counter.
var suffixCounter atomic.Int64

func randomSuffix() string {
	return fmt.Sprintf("%d", suffixCounter.Add(1))
}
