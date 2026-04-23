/*
Copyright (c) 2025 Stan Kozarev.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"errors"
	"fmt"
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
