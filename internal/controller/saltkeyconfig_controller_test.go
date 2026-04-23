/*
Copyright (c) 2025 Stan Kozarev.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	saltv1alpha1 "github.com/stanimir-kozarev/vcf-salt-operator/api/v1alpha1"
)

// saltCfgReconciler returns a configured SaltKeyConfigReconciler for tests.
func saltCfgReconciler() *SaltKeyConfigReconciler {
	return &SaltKeyConfigReconciler{Client: k8sClient}
}

// reconcileCfg runs the reconciler against the SaltKeyConfig named "salt-config" and returns the fetched result.
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

// findReadyCondition returns the Ready condition from the SaltKeyConfig status, or nil.
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
