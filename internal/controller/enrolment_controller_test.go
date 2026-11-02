/*
SPDX-License-Identifier: MIT-0
*/

package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	saltv1alpha1 "github.com/stanimir-kozarev/vcf-salt-operator/api/v1alpha1"
)

var _ = Describe("Enrolment Controller", func() {
	makeNS := func(prefix string, nsLabels map[string]string) string {
		GinkgoHelper()
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: prefix + randomSuffix(), Labels: nsLabels}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ns) })
		return ns.Name
	}

	reconcileNS := func(e Enrolment, name string) {
		GinkgoHelper()
		r := &EnrolmentReconciler{Client: k8sClient, Enrolment: e}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
		Expect(err).NotTo(HaveOccurred())
	}

	configsIn := func(name string) []saltv1alpha1.SaltKeyConfig {
		GinkgoHelper()
		list := &saltv1alpha1.SaltKeyConfigList{}
		Expect(k8sClient.List(ctx, list, client.InNamespace(name))).To(Succeed())
		return list.Items
	}

	It("should create a default-connection SaltKeyConfig in a listed namespace", func() {
		ns := makeNS("enrol-", nil)
		reconcileNS(Enrolment{Namespaces: []string{ns}}, ns)

		items := configsIn(ns)
		Expect(items).To(HaveLen(1))
		Expect(items[0].Name).To(Equal(EnrolledConfigName))
		Expect(items[0].Labels[LabelManagedBy]).To(Equal(ManagedByOperator))
		Expect(items[0].Spec.CredentialsSecret).To(BeEmpty())
		Expect(items[0].Spec.RaasURL).To(BeEmpty())
	})

	It("should create the enrolled SaltKeyConfig with the install's key acceptance", func() {
		ns := makeNS("enrol-", nil)
		reconcileNS(Enrolment{Namespaces: []string{ns}, KeyAcceptance: saltv1alpha1.KeyAcceptancePreseed}, ns)

		items := configsIn(ns)
		Expect(items).To(HaveLen(1))
		Expect(items[0].Spec.KeyAcceptance).To(Equal(saltv1alpha1.KeyAcceptancePreseed))
	})

	It("should change its own SaltKeyConfig when the install's key acceptance changes", func() {
		ns := makeNS("enrol-", nil)
		reconcileNS(Enrolment{Namespaces: []string{ns}, KeyAcceptance: saltv1alpha1.KeyAcceptanceDiscovery}, ns)
		Expect(configsIn(ns)[0].Spec.KeyAcceptance).To(Equal(saltv1alpha1.KeyAcceptanceDiscovery))

		reconcileNS(Enrolment{Namespaces: []string{ns}, KeyAcceptance: saltv1alpha1.KeyAcceptancePreseed}, ns)
		items := configsIn(ns)
		Expect(items).To(HaveLen(1))
		Expect(items[0].Name).To(Equal(EnrolledConfigName))
		Expect(items[0].Spec.KeyAcceptance).To(Equal(saltv1alpha1.KeyAcceptancePreseed))
	})

	It("should leave the key acceptance of a team's own SaltKeyConfig alone", func() {
		ns := makeNS("enrol-", nil)
		team := &saltv1alpha1.SaltKeyConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: ns},
			Spec:       saltv1alpha1.SaltKeyConfigSpec{KeyAcceptance: saltv1alpha1.KeyAcceptanceDiscovery},
		}
		Expect(k8sClient.Create(ctx, team)).To(Succeed())

		reconcileNS(Enrolment{Namespaces: []string{ns}, KeyAcceptance: saltv1alpha1.KeyAcceptancePreseed}, ns)
		items := configsIn(ns)
		Expect(items).To(HaveLen(1))
		Expect(items[0].Name).To(Equal("team"))
		Expect(items[0].Spec.KeyAcceptance).To(Equal(saltv1alpha1.KeyAcceptanceDiscovery))
	})

	It("should enrol a namespace matching the selector", func() {
		ns := makeNS("enrol-", map[string]string{"salt": "on"})
		sel, err := labels.Parse("salt=on")
		Expect(err).NotTo(HaveOccurred())
		reconcileNS(Enrolment{Selector: sel}, ns)
		Expect(configsIn(ns)).To(HaveLen(1))
	})

	It("should leave a namespace that already has its own SaltKeyConfig untouched", func() {
		ns := makeNS("enrol-", nil)
		team := &saltv1alpha1.SaltKeyConfig{ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: ns}}
		Expect(k8sClient.Create(ctx, team)).To(Succeed())

		reconcileNS(Enrolment{Namespaces: []string{ns}}, ns)
		items := configsIn(ns)
		Expect(items).To(HaveLen(1))
		Expect(items[0].Name).To(Equal("team"))
	})

	It("should never enrol an excluded namespace, even when listed or all are enrolled", func() {
		ns := makeNS("svc-enrol-", nil)
		reconcileNS(Enrolment{Namespaces: []string{ns}, All: true, ExcludePrefixes: []string{"svc-"}}, ns)
		Expect(configsIn(ns)).To(BeEmpty())
	})

	It("should remove its own SaltKeyConfig when an enrolled namespace gets one of its own", func() {
		ns := makeNS("enrol-", nil)
		e := Enrolment{Namespaces: []string{ns}}
		reconcileNS(e, ns)
		team := &saltv1alpha1.SaltKeyConfig{ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: ns}}
		Expect(k8sClient.Create(ctx, team)).To(Succeed())

		reconcileNS(e, ns)
		items := configsIn(ns)
		Expect(items).To(HaveLen(1))
		Expect(items[0].Name).To(Equal("team"))

		reconcileNS(e, ns)
		Expect(configsIn(ns)).To(HaveLen(1))
	})

	It("should release only operator-created SaltKeyConfigs when enrolment is off", func() {
		enrolled := makeNS("enrol-", nil)
		reconcileNS(Enrolment{Namespaces: []string{enrolled}}, enrolled)
		teamNS := makeNS("enrol-", nil)
		team := &saltv1alpha1.SaltKeyConfig{ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: teamNS}}
		Expect(k8sClient.Create(ctx, team)).To(Succeed())

		n, err := ReleaseEnrolled(ctx, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeNumerically(">=", 1))
		Expect(configsIn(enrolled)).To(BeEmpty())
		Expect(configsIn(teamNS)).To(HaveLen(1))
	})

	It("should give a VM the team's config over the enrolled one while both exist", func() {
		ns := makeNS("enrol-", nil)
		makeReady := func(name string, lbls map[string]string) {
			GinkgoHelper()
			cfg := &saltv1alpha1.SaltKeyConfig{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: lbls}}
			Expect(k8sClient.Create(ctx, cfg)).To(Succeed())
			original := cfg.DeepCopy()
			cfg.Status.Conditions = []metav1.Condition{{
				Type: ConditionReady, Status: metav1.ConditionTrue, Reason: ReasonReady,
				Message: "ready", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Patch(ctx, cfg, client.MergeFrom(original))).To(Succeed())
		}
		makeReady("a-enrolled", map[string]string{LabelManagedBy: ManagedByOperator})
		makeReady("z-team", nil)

		r := &VirtualMachineReconciler{Client: k8sClient}
		cfg, err := r.findSaltKeyConfig(ctx, ns)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg).NotTo(BeNil())
		Expect(cfg.Name).To(Equal("z-team"))
	})

	It("should delete only its own SaltKeyConfig when a namespace leaves enrolment", func() {
		ns := makeNS("enrol-", nil)
		reconcileNS(Enrolment{Namespaces: []string{ns}}, ns)
		team := &saltv1alpha1.SaltKeyConfig{ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: ns}}
		Expect(k8sClient.Create(ctx, team)).To(Succeed())

		reconcileNS(Enrolment{}, ns)
		items := configsIn(ns)
		Expect(items).To(HaveLen(1))
		Expect(items[0].Name).To(Equal("team"))
	})
})
