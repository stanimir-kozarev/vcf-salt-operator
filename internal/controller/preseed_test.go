/*
SPDX-License-Identifier: MIT-0
*/

package controller

import (
	"context"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	saltv1alpha1 "github.com/stanimir-kozarev/vcf-salt-operator/api/v1alpha1"
)

func newNamespace(ctx context.Context, prefix string) string {
	GinkgoHelper()
	name := prefix + randomSuffix()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(ctx, ns) })
	return name
}

func setKeyAcceptance(ctx context.Context, namespace, mode string) {
	GinkgoHelper()
	cfg := &saltv1alpha1.SaltKeyConfig{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "salt-config", Namespace: namespace}, cfg)).To(Succeed())
	patch := client.MergeFrom(cfg.DeepCopy())
	cfg.Spec.KeyAcceptance = mode
	Expect(k8sClient.Patch(ctx, cfg, patch)).To(Succeed())
}

func setAcceptTimeout(ctx context.Context, namespace, timeout string) {
	GinkgoHelper()
	cfg := &saltv1alpha1.SaltKeyConfig{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "salt-config", Namespace: namespace}, cfg)).To(Succeed())
	patch := client.MergeFrom(cfg.DeepCopy())
	cfg.Spec.AcceptTimeout = timeout
	Expect(k8sClient.Patch(ctx, cfg, patch)).To(Succeed())
}

func setGuestProbe(ctx context.Context, key types.NamespacedName, guestInfoKey string) {
	GinkgoHelper()
	vm := getVM(ctx, key)
	patch := client.MergeFrom(vm.DeepCopy())
	Expect(unstructured.SetNestedSlice(vm.Object, []any{map[string]any{"key": guestInfoKey}},
		"spec", "readinessProbe", "guestInfo")).To(Succeed())
	Expect(k8sClient.Patch(ctx, vm, patch)).To(Succeed())
}

func setReadyCondition(ctx context.Context, key types.NamespacedName, status string) {
	GinkgoHelper()
	setCondition(ctx, key, map[string]any{"type": "Ready", "status": status})
}

func setCreatedCondition(ctx context.Context, key types.NamespacedName) {
	GinkgoHelper()
	setCondition(ctx, key, createdCondition())
}

func setCondition(ctx context.Context, key types.NamespacedName, cond map[string]any) {
	GinkgoHelper()
	vm := getVM(ctx, key)
	conds, _, _ := unstructured.NestedSlice(vm.Object, "status", "conditions")
	conds = slices.DeleteFunc(conds, func(c any) bool {
		m, ok := c.(map[string]any)
		return ok && m["type"] == cond["type"]
	})
	Expect(unstructured.SetNestedSlice(vm.Object, append(conds, cond), "status", "conditions")).To(Succeed())
	Expect(k8sClient.Status().Update(ctx, vm)).To(Succeed())
}

func createManagedVM(ctx context.Context, name, namespace string, annotations map[string]string) *unstructured.Unstructured {
	GinkgoHelper()
	vm := makeVM(name, namespace, mergeAnnotations(map[string]string{AnnotationManaged: "true"}, annotations))
	Expect(k8sClient.Create(ctx, vm)).To(Succeed())
	DeferCleanup(func() {
		latest := &unstructured.Unstructured{}
		latest.SetGroupVersionKind(virtualMachineGVK)
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(vm), latest); err == nil {
			p := client.MergeFrom(latest.DeepCopy())
			latest.SetFinalizers(nil)
			_ = k8sClient.Patch(ctx, latest, p)
			_ = k8sClient.Delete(ctx, latest)
		}
	})
	return vm
}

func getVM(ctx context.Context, key types.NamespacedName) *unstructured.Unstructured {
	GinkgoHelper()
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(virtualMachineGVK)
	Expect(k8sClient.Get(ctx, key, vm)).To(Succeed())
	return vm
}

func drainEvents(r *VirtualMachineReconciler) []string {
	var out []string
	events := r.Recorder.(*record.FakeRecorder).Events
	for {
		select {
		case e := <-events:
			out = append(out, e)
		default:
			return out
		}
	}
}

var _ = Describe("Preseed key acceptance", func() {
	var (
		ns         string
		vmName     string
		mock       *mockSaltClient
		reconciler *VirtualMachineReconciler
		req        reconcile.Request
		secretKey  types.NamespacedName
	)

	BeforeEach(func() {
		ns = newNamespace(ctx, "preseed-")
		makeSaltKeyConfig(ctx, ns)
		setKeyAcceptance(ctx, ns, saltv1alpha1.KeyAcceptancePreseed)
		setAcceptTimeout(ctx, ns, "10m")
		vmName = "pre-vm-" + randomSuffix()
		mock = &mockSaltClient{}
		reconciler = newTestReconciler(mock)
		req = reconcile.Request{NamespacedName: types.NamespacedName{Name: vmName, Namespace: ns}}
		secretKey = types.NamespacedName{Name: vmName + bootstrapSecretSuffix, Namespace: ns}
	})

	It("should generate the key and write the bootstrap Secret before the VM runs", func() {
		vm := createManagedVM(ctx, vmName, ns, nil)

		result, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(mock.GenForce).To(Equal([]bool{false}))
		Expect(mock.AcceptedKeys).To(BeEmpty())
		Expect(mock.Pings).To(BeZero())

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
		Expect(secret.Data).To(Equal(map[string][]byte{
			"minion.pem":        []byte("priv-" + vmName),
			"minion.pub":        []byte("pub-" + vmName),
			"minion_master.pub": []byte("master-pub"),
			"minion_id":         []byte(vmName),
		}))
		Expect(secret.OwnerReferences).To(HaveLen(1))
		Expect(secret.OwnerReferences[0].UID).To(Equal(vm.GetUID()))
		Expect(secret.OwnerReferences[0].Kind).To(Equal("VirtualMachine"))

		updated := getVM(ctx, req.NamespacedName)
		Expect(updated.GetAnnotations()[AnnotationKeyStatus]).To(Equal(keyStatusPreseeded))
		Expect(updated.GetAnnotations()[AnnotationMinionID]).To(Equal(vmName))
		Expect(updated.GetAnnotations()[AnnotationMatchMethod]).To(Equal(matchMethodPreseed))
		Expect(updated.GetAnnotations()).NotTo(HaveKey(AnnotationAcceptedAt))
		Expect(updated.GetFinalizers()).To(ContainElement(saltFinalizer))
	})

	It("should wait for the preseeded VM to run without generating another key", func() {
		createManagedVM(ctx, vmName, ns, map[string]string{
			AnnotationKeyStatus: keyStatusPreseeded,
			AnnotationMinionID:  vmName,
		})

		result, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(mock.GenForce).To(BeEmpty())
		Expect(getVM(ctx, req.NamespacedName).GetAnnotations()[AnnotationKeyStatus]).To(Equal(keyStatusPreseeded))
	})

	It("should mark the key Accepted once the preseeded VM runs, starting the bootstrap clock then", func() {
		vm := createManagedVM(ctx, vmName, ns, map[string]string{
			AnnotationKeyStatus: keyStatusPreseeded,
			AnnotationMinionID:  vmName,
		})
		setVMStatus(ctx, vm, "Running", "10.0.0.9")

		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(mock.GenForce).To(BeEmpty())
		Expect(mock.AcceptedKeys).To(BeEmpty())

		updated := getVM(ctx, req.NamespacedName)
		Expect(updated.GetAnnotations()[AnnotationKeyStatus]).To(Equal("Accepted"))
		Expect(updated.GetAnnotations()[AnnotationAcceptedAt]).NotTo(BeEmpty())
	})

	It("should adopt a VM whose key discovery already accepted, without a new key or Secret", func() {
		vm := createManagedVM(ctx, vmName, ns, map[string]string{
			AnnotationKeyStatus:   "Accepted",
			AnnotationMinionID:    vmName,
			AnnotationMatchMethod: "name",
		})
		setVMStatus(ctx, vm, "Running", "10.0.0.9")

		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(mock.GenForce).To(BeEmpty())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, secretKey, &corev1.Secret{}))).To(BeTrue())
		Expect(getVM(ctx, req.NamespacedName).GetAnnotations()[AnnotationMatchMethod]).To(Equal("name"))
	})

	It("should force over a pending key and report it as foreign", func() {
		mock.KeyStateList = []string{"pending"}
		createManagedVM(ctx, vmName, ns, nil)

		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(mock.GenForce).To(Equal([]bool{true}))
		Expect(drainEvents(reconciler)).To(ContainElement(ContainSubstring("SaltForeignKeyReplaced")))
	})

	It("should force over an accepted key no other VM claims, the recreated VM case", func() {
		mock.KeyStateList = []string{"accepted"}
		createManagedVM(ctx, vmName, ns, nil)

		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(mock.GenForce).To(Equal([]bool{true}))
		Expect(drainEvents(reconciler)).NotTo(ContainElement(ContainSubstring("SaltForeignKeyReplaced")))
	})

	It("should not force when only a denied key exists", func() {
		mock.KeyStateList = []string{"denied"}
		createManagedVM(ctx, vmName, ns, nil)

		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(mock.GenForce).To(Equal([]bool{false}))
	})

	It("should never generate a key for a minion ID another VM holds", func() {
		mock.KeyStateList = []string{"accepted"}
		otherNS := newNamespace(ctx, "other-")
		createManagedVM(ctx, vmName, otherNS, map[string]string{
			AnnotationKeyStatus: "Accepted",
			AnnotationMinionID:  vmName,
		})
		createManagedVM(ctx, vmName, ns, nil)

		result, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero())
		Expect(mock.GenForce).To(BeEmpty())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, secretKey, &corev1.Secret{}))).To(BeTrue())
		Expect(getVM(ctx, req.NamespacedName).GetAnnotations()[AnnotationKeyStatus]).To(Equal("Failed"))
		Expect(drainEvents(reconciler)).To(ContainElement(ContainSubstring("SaltMinionIDInUse")))
	})

	It("should wait while the VM holding the minion ID is being deleted", func() {
		otherNS := newNamespace(ctx, "other-")
		holder := makeVM(vmName, otherNS, map[string]string{
			AnnotationManaged:   "true",
			AnnotationKeyStatus: "Accepted",
			AnnotationMinionID:  vmName,
		})
		holder.SetFinalizers([]string{"test.vcf.io/hold"})
		Expect(k8sClient.Create(ctx, holder)).To(Succeed())
		Expect(k8sClient.Delete(ctx, holder)).To(Succeed())
		DeferCleanup(func() {
			latest := getVM(ctx, client.ObjectKeyFromObject(holder))
			p := client.MergeFrom(latest.DeepCopy())
			latest.SetFinalizers(nil)
			_ = k8sClient.Patch(ctx, latest, p)
		})
		createManagedVM(ctx, vmName, ns, nil)

		result, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(mock.GenForce).To(BeEmpty())
		Expect(getVM(ctx, req.NamespacedName).GetAnnotations()).NotTo(HaveKey(AnnotationKeyStatus))
	})

	It("should fail without a Secret when the master already has a key after the state check", func() {
		mock.GenNoPair = true
		createManagedVM(ctx, vmName, ns, nil)

		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, secretKey, &corev1.Secret{}))).To(BeTrue())
		Expect(getVM(ctx, req.NamespacedName).GetAnnotations()).NotTo(HaveKey(AnnotationKeyStatus))
	})

	It("should overwrite a stale bootstrap Secret left by an earlier VM of the same name", func() {
		stale := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretKey.Name,
				Namespace: ns,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "vmoperator.vmware.com/v1alpha4", Kind: "VirtualMachine",
					Name: vmName, UID: "00000000-0000-0000-0000-000000000001",
				}},
			},
			Data: map[string][]byte{"minion.pem": []byte("old")},
		}
		Expect(k8sClient.Create(ctx, stale)).To(Succeed())
		mock.KeyStateList = []string{"accepted"}
		vm := createManagedVM(ctx, vmName, ns, nil)

		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
		Expect(string(secret.Data["minion.pem"])).To(Equal("priv-" + vmName))
		Expect(secret.OwnerReferences[0].UID).To(Equal(vm.GetUID()))
	})

	pingReadyVM := func(annotations map[string]string) {
		GinkgoHelper()
		vm := createManagedVM(ctx, vmName, ns, mergeAnnotations(map[string]string{
			AnnotationKeyStatus:  "Accepted",
			AnnotationMinionID:   vmName,
			AnnotationAcceptedAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		}, annotations))
		setVMStatus(ctx, vm, "Running", "10.0.0.9")
	}

	bootstrapData := func() map[string][]byte {
		return map[string][]byte{
			"minion.pem":        []byte("priv-" + vmName),
			"minion.pub":        []byte("pub-" + vmName),
			"minion_master.pub": []byte("master-written-at-preseed"),
			"minion_id":         []byte(vmName),
		}
	}
	createBootstrapSecret := func() {
		GinkgoHelper()
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretKey.Name, Namespace: secretKey.Namespace},
			Data:       bootstrapData(),
		})).To(Succeed())
	}

	It("should replace only the key pair in the bootstrap Secret after the first ping", func() {
		createBootstrapSecret()
		pingReadyVM(map[string]string{AnnotationMatchMethod: matchMethodPreseed})

		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(mock.Pings).To(Equal(1))

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
		Expect(secret.Data).To(Equal(map[string][]byte{
			"minion.pem":        []byte(bootstrapKeyCleared),
			"minion.pub":        []byte(bootstrapKeyCleared),
			"minion_master.pub": []byte("master-written-at-preseed"),
			"minion_id":         []byte(vmName),
		}))
		updated := getVM(ctx, req.NamespacedName)
		Expect(updated.GetAnnotations()[AnnotationSaltStatus]).To(Equal("Pinged"))
		Expect(updated.GetAnnotations()[AnnotationKeyCleared]).To(Equal(annotationTrue))
		Expect(drainEvents(reconciler)).To(ContainElement(And(
			ContainSubstring("SaltKeyCleared"), ContainSubstring("after the first ping"))))
	})

	It("should not write the Secret again on a later ping", func() {
		pingReadyVM(map[string]string{
			AnnotationMatchMethod: matchMethodPreseed,
			AnnotationKeyCleared:  annotationTrue,
		})

		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(mock.Pings).To(Equal(1))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, secretKey, &corev1.Secret{}))).To(BeTrue())
		Expect(drainEvents(reconciler)).NotTo(ContainElement(ContainSubstring("SaltKeyCleared")))
	})

	It("should mark a VM whose bootstrap Secret is gone as cleared without creating one", func() {
		pingReadyVM(map[string]string{AnnotationMatchMethod: matchMethodPreseed})

		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, secretKey, &corev1.Secret{}))).To(BeTrue())
		Expect(getVM(ctx, req.NamespacedName).GetAnnotations()[AnnotationKeyCleared]).To(Equal(annotationTrue))
	})

	It("should keep the private key while the first ping is retried", func() {
		createBootstrapSecret()
		mock.PingFails = true
		pingReadyVM(map[string]string{
			AnnotationMatchMethod: matchMethodPreseed,
			AnnotationAcceptedAt:  time.Now().UTC().Format(time.RFC3339),
		})

		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
		Expect(secret.Data).To(Equal(bootstrapData()))
		Expect(getVM(ctx, req.NamespacedName).GetAnnotations()).NotTo(HaveKey(AnnotationKeyCleared))
	})

	It("should clear the private key when the ping fails for good", func() {
		createBootstrapSecret()
		mock.PingFails = true
		pingReadyVM(map[string]string{AnnotationMatchMethod: matchMethodPreseed})

		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
		Expect(string(secret.Data["minion.pem"])).To(Equal(bootstrapKeyCleared))
		updated := getVM(ctx, req.NamespacedName)
		Expect(updated.GetAnnotations()[AnnotationSaltStatus]).To(Equal("Failed/Ping"))
		Expect(updated.GetAnnotations()[AnnotationKeyCleared]).To(Equal(annotationTrue))
		Expect(drainEvents(reconciler)).To(ContainElement(ContainSubstring("after the failed ping")))
	})

	preseededVM := func() {
		GinkgoHelper()
		createManagedVM(ctx, vmName, ns, map[string]string{
			AnnotationKeyStatus:   keyStatusPreseeded,
			AnnotationMinionID:    vmName,
			AnnotationMatchMethod: matchMethodPreseed,
		})
		createBootstrapSecret()
	}

	It("should clear the private key on the guest signal before the VM has an IP", func() {
		preseededVM()
		setGuestProbe(ctx, req.NamespacedName, guestSignalKey)
		setReadyCondition(ctx, req.NamespacedName, "True")

		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(mock.Pings).To(BeZero())
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
		Expect(string(secret.Data["minion.pem"])).To(Equal(bootstrapKeyCleared))
		Expect(string(secret.Data["minion.pub"])).To(Equal(bootstrapKeyCleared))
		updated := getVM(ctx, req.NamespacedName)
		Expect(updated.GetAnnotations()[AnnotationKeyCleared]).To(Equal(annotationTrue))
		Expect(updated.GetAnnotations()[AnnotationKeyStatus]).To(Equal(keyStatusPreseeded))
		Expect(drainEvents(reconciler)).To(ContainElement(ContainSubstring("after the guest signal")))
	})

	It("should ignore a Ready condition from a probe that does not name the signal key", func() {
		preseededVM()
		setGuestProbe(ctx, req.NamespacedName, "some-other-key")
		setReadyCondition(ctx, req.NamespacedName, "True")

		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
		Expect(secret.Data).To(Equal(bootstrapData()))
		Expect(getVM(ctx, req.NamespacedName).GetAnnotations()).NotTo(HaveKey(AnnotationKeyCleared))
	})

	It("should keep the private key while the signal probe is not Ready", func() {
		preseededVM()
		setGuestProbe(ctx, req.NamespacedName, guestSignalKey)
		setReadyCondition(ctx, req.NamespacedName, "False")

		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
		Expect(secret.Data).To(Equal(bootstrapData()))
	})

	It("should not touch the Secret again once cleared, whatever the guest signal does", func() {
		preseededVM()
		setGuestProbe(ctx, req.NamespacedName, guestSignalKey)
		setReadyCondition(ctx, req.NamespacedName, "True")
		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		drainEvents(reconciler)

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
		patch := client.MergeFrom(secret.DeepCopy())
		secret.Data["minion.pem"] = []byte("rewritten")
		Expect(k8sClient.Patch(ctx, secret, patch)).To(Succeed())

		for _, status := range []string{"False", "True"} {
			setReadyCondition(ctx, req.NamespacedName, status)
			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
		Expect(string(secret.Data["minion.pem"])).To(Equal("rewritten"))
		Expect(drainEvents(reconciler)).NotTo(ContainElement(ContainSubstring("SaltKeyCleared")))
	})

	It("should keep the private key of a preseeded VM not yet created in vSphere past acceptTimeout", func() {
		setAcceptTimeout(ctx, ns, "1ms")
		preseededVM()
		time.Sleep(10 * time.Millisecond)

		result, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
		Expect(string(secret.Data["minion.pem"])).To(Equal("priv-" + vmName))
		updated := getVM(ctx, req.NamespacedName)
		Expect(updated.GetAnnotations()).NotTo(HaveKey(AnnotationKeyCleared))
		Expect(updated.GetAnnotations()[AnnotationKeyStatus]).To(Equal(keyStatusPreseeded))
		Expect(drainEvents(reconciler)).To(BeEmpty())
	})

	It("should clear the private key and fail a preseeded VM with no IP after acceptTimeout", func() {
		setAcceptTimeout(ctx, ns, "1ms")
		preseededVM()
		setCreatedCondition(ctx, req.NamespacedName)

		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
		Expect(string(secret.Data["minion.pem"])).To(Equal(bootstrapKeyCleared))
		updated := getVM(ctx, req.NamespacedName)
		Expect(updated.GetAnnotations()[AnnotationKeyCleared]).To(Equal(annotationTrue))
		Expect(updated.GetAnnotations()[AnnotationKeyStatus]).To(Equal(keyStatusFailed))
		events := drainEvents(reconciler)
		Expect(events).To(ContainElement(ContainSubstring("after acceptTimeout 1ms")))
		Expect(events).To(ContainElement(ContainSubstring("SaltKeyTimeout")))

		result, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))
		Expect(mock.GenForce).To(BeEmpty())
		Expect(getVM(ctx, req.NamespacedName).GetAnnotations()[AnnotationKeyStatus]).To(Equal(keyStatusFailed))
	})

	It("should clear the private key after acceptTimeout while the first ping is retried", func() {
		setAcceptTimeout(ctx, ns, "1ms")
		createBootstrapSecret()
		mock.PingFails = true
		pingReadyVM(map[string]string{
			AnnotationMatchMethod: matchMethodPreseed,
			AnnotationAcceptedAt:  time.Now().UTC().Format(time.RFC3339),
		})

		result, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
		Expect(string(secret.Data["minion.pem"])).To(Equal(bootstrapKeyCleared))
		Expect(getVM(ctx, req.NamespacedName).GetAnnotations()[AnnotationKeyStatus]).To(Equal("Accepted"))
	})

	It("should leave a VM whose key discovery accepted without a bootstrap Secret after its ping", func() {
		pingReadyVM(map[string]string{AnnotationMatchMethod: "name"})

		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(mock.Pings).To(Equal(1))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, secretKey, &corev1.Secret{}))).To(BeTrue())
		Expect(getVM(ctx, req.NamespacedName).GetAnnotations()).NotTo(HaveKey(AnnotationKeyCleared))
	})

	It("should keep discovery when the namespace does not choose preseed", func() {
		setKeyAcceptance(ctx, ns, saltv1alpha1.KeyAcceptanceDiscovery)
		mock.PendingKeys = []string{vmName}
		vm := createManagedVM(ctx, vmName, ns, nil)
		setVMStatus(ctx, vm, "Running", "10.0.0.9")

		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(mock.GenForce).To(BeEmpty())
		Expect(mock.AcceptedKeys).To(ContainElement(vmName))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, secretKey, &corev1.Secret{}))).To(BeTrue())
	})
})
