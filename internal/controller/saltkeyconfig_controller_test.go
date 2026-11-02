/*
SPDX-License-Identifier: MIT-0
*/

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	saltv1alpha1 "github.com/stanimir-kozarev/vcf-salt-operator/api/v1alpha1"
	"github.com/stanimir-kozarev/vcf-salt-operator/internal/raasconn"
)

func saltCfgReconciler() *SaltKeyConfigReconciler {
	return &SaltKeyConfigReconciler{Client: k8sClient}
}

func reconcileCfg(ctx context.Context, ns string) *saltv1alpha1.SaltKeyConfig {
	GinkgoHelper()
	_, err := saltCfgReconciler().Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "salt-config", Namespace: ns},
	})
	Expect(err).NotTo(HaveOccurred())

	result := &saltv1alpha1.SaltKeyConfig{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "salt-config", Namespace: ns}, result)).To(Succeed())
	return result
}

func findReadyCondition(cfg *saltv1alpha1.SaltKeyConfig) *metav1.Condition {
	for i := range cfg.Status.Conditions {
		if cfg.Status.Conditions[i].Type == ConditionReady {
			return &cfg.Status.Conditions[i]
		}
	}
	return nil
}

var _ = Describe("SaltKeyConfig Controller", func() {
	var (
		ns    string
		cfgNS *corev1.Namespace
	)

	BeforeEach(func() {
		ns = "cfg-test-" + randomSuffix()
		cfgNS = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, cfgNS)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cfgNS) })
	})

	makeCfg := func(secretName, acceptTimeout, requeueInterval string) *saltv1alpha1.SaltKeyConfig {
		GinkgoHelper()
		cfg := &saltv1alpha1.SaltKeyConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "salt-config", Namespace: ns},
			Spec: saltv1alpha1.SaltKeyConfigSpec{
				RaasURL:           "https://aria-config.test:443",
				CredentialsSecret: secretName,
				MasterID:          "test-master",
				AcceptTimeout:     acceptTimeout,
				RequeueInterval:   requeueInterval,
			},
		}
		Expect(k8sClient.Create(ctx, cfg)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cfg) })
		return cfg
	}

	makeSecret := func(name string, data map[string][]byte) {
		GinkgoHelper()
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Data:       data,
		}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, s) })
	}

	Context("Ready=True", func() {
		It("should set Ready=True when Secret has valid username and password", func() {
			makeSecret("creds", map[string][]byte{
				"username": []byte("admin"),
				"password": []byte("secret"),
			})
			makeCfg("creds", "10m", "30s")

			result := reconcileCfg(ctx, ns)
			cond := findReadyCondition(result)
			Expect(cond).NotTo(BeNil())
			Expect(string(cond.Status)).To(Equal("True"))
			Expect(cond.Reason).To(Equal(ReasonReady))
		})
	})

	Context("Default connection", func() {
		var opNS string

		BeforeEach(func() {
			opNS = "op-test-" + randomSuffix()
			opNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: opNS}}
			Expect(k8sClient.Create(ctx, opNamespace)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, opNamespace) })
			conn := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "raas-connection", Namespace: opNS},
				Data: map[string][]byte{
					"url": []byte("https://raas.platform"), "masterID": []byte("master-01"),
					"username": []byte("u"), "password": []byte("p"),
				},
			}
			Expect(k8sClient.Create(ctx, conn)).To(Succeed())
		})

		reconcileWithDefault := func(spec saltv1alpha1.SaltKeyConfigSpec) *metav1.Condition {
			GinkgoHelper()
			cfg := &saltv1alpha1.SaltKeyConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "salt-config", Namespace: ns},
				Spec:       spec,
			}
			Expect(k8sClient.Create(ctx, cfg)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cfg) })
			r := &SaltKeyConfigReconciler{
				Client:            k8sClient,
				DefaultConnection: raasconn.Default{Namespace: opNS, SecretName: "raas-connection"},
			}
			_, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "salt-config", Namespace: ns},
			})
			Expect(err).NotTo(HaveOccurred())
			result := &saltv1alpha1.SaltKeyConfig{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "salt-config", Namespace: ns}, result)).To(Succeed())
			return findReadyCondition(result)
		}

		It("should accept a SaltKeyConfig with no connection fields and set Ready=True", func() {
			cond := reconcileWithDefault(saltv1alpha1.SaltKeyConfigSpec{})
			Expect(cond).NotTo(BeNil())
			Expect(string(cond.Status)).To(Equal("True"))
			Expect(cond.Message).To(ContainSubstring("default RaaS connection"))
			Expect(cond.Message).To(ContainSubstring("TLS verification on"))
		})

		It("should not store a skipTLSVerify the user never set", func() {
			cfg := &saltv1alpha1.SaltKeyConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "no-tls-field", Namespace: ns},
			}
			Expect(k8sClient.Create(ctx, cfg)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cfg) })
			stored := &unstructured.Unstructured{}
			stored.SetGroupVersionKind(saltv1alpha1.GroupVersion.WithKind("SaltKeyConfig"))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "no-tls-field", Namespace: ns}, stored)).To(Succeed())
			_, found, err := unstructured.NestedFieldNoCopy(stored.Object, "spec", "skipTLSVerify")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeFalse())
		})

		It("should set Ready=False with ConnectionMismatch for another endpoint", func() {
			cond := reconcileWithDefault(saltv1alpha1.SaltKeyConfigSpec{RaasURL: "https://attacker.example"})
			Expect(cond).NotTo(BeNil())
			Expect(string(cond.Status)).To(Equal("False"))
			Expect(cond.Reason).To(Equal(ReasonConnectionMismatch))
		})
	})

	Context("Ready=False: Secret problems", func() {
		It("should set Ready=False with SecretNotFound when Secret is missing", func() {
			makeCfg("nonexistent-secret", "", "")

			result := reconcileCfg(ctx, ns)
			cond := findReadyCondition(result)
			Expect(cond).NotTo(BeNil())
			Expect(string(cond.Status)).To(Equal("False"))
			Expect(cond.Reason).To(Equal(ReasonSecretNotFound))
		})

		It("should set Ready=False with SecretInvalid when username key is missing", func() {
			makeSecret("creds", map[string][]byte{
				"password": []byte("secret"),
			})
			makeCfg("creds", "", "")

			result := reconcileCfg(ctx, ns)
			cond := findReadyCondition(result)
			Expect(cond).NotTo(BeNil())
			Expect(string(cond.Status)).To(Equal("False"))
			Expect(cond.Reason).To(Equal(ReasonSecretInvalid))
		})

		It("should set Ready=False with SecretInvalid when password key is missing", func() {
			makeSecret("creds", map[string][]byte{
				"username": []byte("admin"),
			})
			makeCfg("creds", "", "")

			result := reconcileCfg(ctx, ns)
			cond := findReadyCondition(result)
			Expect(cond).NotTo(BeNil())
			Expect(string(cond.Status)).To(Equal("False"))
			Expect(cond.Reason).To(Equal(ReasonSecretInvalid))
		})
	})

	Context("Retry on Secret problems", func() {
		reconcileResult := func(r *SaltKeyConfigReconciler) ctrl.Result {
			GinkgoHelper()
			res, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "salt-config", Namespace: ns},
			})
			Expect(err).NotTo(HaveOccurred())
			return res
		}

		It("should retry a missing namespace Secret and turn Ready once it exists", func() {
			makeCfg("late-creds", "", "")
			Expect(reconcileResult(saltCfgReconciler()).RequeueAfter).To(Equal(secretRetryInterval))

			makeSecret("late-creds", map[string][]byte{"username": []byte("a"), "password": []byte("b")})
			Expect(reconcileResult(saltCfgReconciler()).RequeueAfter).To(BeZero())
			Expect(findReadyCondition(reconcileCfg(ctx, ns)).Reason).To(Equal(ReasonReady))
		})

		It("should retry a missing default connection Secret", func() {
			cfg := &saltv1alpha1.SaltKeyConfig{ObjectMeta: metav1.ObjectMeta{Name: "salt-config", Namespace: ns}}
			Expect(k8sClient.Create(ctx, cfg)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cfg) })
			r := &SaltKeyConfigReconciler{
				Client:            k8sClient,
				DefaultConnection: raasconn.Default{Namespace: ns, SecretName: "raas-connection"},
			}
			Expect(reconcileResult(r).RequeueAfter).To(Equal(secretRetryInterval))
		})

		It("should not retry a spec error", func() {
			makeSecret("creds", map[string][]byte{"username": []byte("a"), "password": []byte("b")})
			makeCfg("creds", "not-a-duration", "")
			Expect(reconcileResult(saltCfgReconciler()).RequeueAfter).To(BeZero())
		})
	})

	Context("Ready=False: invalid vmSelector", func() {
		It("should set Ready=False with InvalidSelector for an unknown operator", func() {
			makeSecret("creds", map[string][]byte{"username": []byte("a"), "password": []byte("b")})
			cfg := makeCfg("creds", "10m", "30s")
			patch := client.MergeFrom(cfg.DeepCopy())
			cfg.Spec.VMSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "salt", Operator: "Bogus", Values: []string{"yes"}},
			}}
			Expect(k8sClient.Patch(ctx, cfg, patch)).To(Succeed())

			cond := findReadyCondition(reconcileCfg(ctx, ns))
			Expect(cond).NotTo(BeNil())
			Expect(string(cond.Status)).To(Equal("False"))
			Expect(cond.Reason).To(Equal(ReasonInvalidSelector))
		})
	})

	Context("Ready=False: invalid duration fields", func() {
		It("should set Ready=False with InvalidDuration for bad acceptTimeout", func() {
			makeSecret("creds", map[string][]byte{
				"username": []byte("admin"),
				"password": []byte("secret"),
			})
			makeCfg("creds", "not-a-duration", "30s")

			result := reconcileCfg(ctx, ns)
			cond := findReadyCondition(result)
			Expect(cond).NotTo(BeNil())
			Expect(string(cond.Status)).To(Equal("False"))
			Expect(cond.Reason).To(Equal(ReasonInvalidDuration))
		})

		It("should set Ready=False with InvalidDuration for bad requeueInterval", func() {
			makeSecret("creds", map[string][]byte{
				"username": []byte("admin"),
				"password": []byte("secret"),
			})
			makeCfg("creds", "10m", "bad-value")

			result := reconcileCfg(ctx, ns)
			cond := findReadyCondition(result)
			Expect(cond).NotTo(BeNil())
			Expect(string(cond.Status)).To(Equal("False"))
			Expect(cond.Reason).To(Equal(ReasonInvalidDuration))
		})
	})

	Context("Idempotency", func() {
		It("should be idempotent: reconciling twice yields the same Ready=True condition", func() {
			makeSecret("creds", map[string][]byte{
				"username": []byte("admin"),
				"password": []byte("secret"),
			})
			makeCfg("creds", "5m", "20s")

			first := reconcileCfg(ctx, ns)
			second := reconcileCfg(ctx, ns)

			c1 := findReadyCondition(first)
			c2 := findReadyCondition(second)
			Expect(c1).NotTo(BeNil())
			Expect(c2).NotTo(BeNil())
			Expect(c1.Status).To(Equal(c2.Status))
			Expect(c1.Reason).To(Equal(c2.Reason))

			Expect(second.Status.ObservedGeneration).To(Equal(second.Generation))
		})
	})
})
