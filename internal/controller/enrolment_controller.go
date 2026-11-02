/*
SPDX-License-Identifier: MIT-0
*/

package controller

import (
	"context"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	saltv1alpha1 "github.com/stanimir-kozarev/vcf-salt-operator/api/v1alpha1"
)

const (
	EnrolledConfigName = "salt-config"

	LabelManagedBy = "salt.vcf.io/managed-by"

	ManagedByOperator = "vcf-salt-operator"
)

var DefaultEnrolmentExcludePrefixes = []string{"kube-", "vmware-system-", "svc-", "tkg-system", "kapp-controller"}

type Enrolment struct {
	Namespaces      []string
	All             bool
	Selector        labels.Selector
	ExcludePrefixes []string
	KeyAcceptance   string
}

func (e Enrolment) Enabled() bool {
	return len(e.Namespaces) > 0 || e.All || (e.Selector != nil && !e.Selector.Empty())
}

func (e Enrolment) matches(ns *corev1.Namespace) bool {
	for _, p := range e.ExcludePrefixes {
		if p != "" && strings.HasPrefix(ns.Name, p) {
			return false
		}
	}
	if slices.Contains(e.Namespaces, ns.Name) || e.All {
		return true
	}
	return e.Selector != nil && !e.Selector.Empty() && e.Selector.Matches(labels.Set(ns.Labels))
}

type EnrolmentReconciler struct {
	client.Client
	Enrolment Enrolment
}

// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

func (r *EnrolmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	ns := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: req.Name}, ns); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !ns.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	configs := &saltv1alpha1.SaltKeyConfigList{}
	if err := r.List(ctx, configs, client.InNamespace(ns.Name)); err != nil {
		return ctrl.Result{}, err
	}
	var own *saltv1alpha1.SaltKeyConfig
	for i := range configs.Items {
		if configs.Items[i].Labels[LabelManagedBy] == ManagedByOperator {
			own = &configs.Items[i]
		}
	}

	if !r.Enrolment.matches(ns) {
		if own != nil {
			log.Info("Namespace left enrolment, removing the SaltKeyConfig the operator created", "namespace", ns.Name)
			if err := r.Delete(ctx, own); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if own != nil && len(configs.Items) > 1 {
		log.Info("Namespace has its own SaltKeyConfig, removing the one the operator created", "namespace", ns.Name)
		if err := r.Delete(ctx, own); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if own != nil {
		if r.Enrolment.KeyAcceptance != "" && own.Spec.KeyAcceptance != r.Enrolment.KeyAcceptance {
			patch := client.MergeFrom(own.DeepCopy())
			own.Spec.KeyAcceptance = r.Enrolment.KeyAcceptance
			if err := r.Patch(ctx, own, patch); err != nil {
				return ctrl.Result{}, err
			}
			log.Info("Changed the key acceptance of the enrolled SaltKeyConfig",
				"namespace", ns.Name, "keyAcceptance", own.Spec.KeyAcceptance)
		}
		return ctrl.Result{}, nil
	}

	if len(configs.Items) > 0 {
		return ctrl.Result{}, nil
	}

	cfg := &saltv1alpha1.SaltKeyConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      EnrolledConfigName,
			Namespace: ns.Name,
			Labels:    map[string]string{LabelManagedBy: ManagedByOperator},
		},
		Spec: saltv1alpha1.SaltKeyConfigSpec{KeyAcceptance: r.Enrolment.KeyAcceptance},
	}
	if err := r.Create(ctx, cfg); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, err
	}
	log.Info("Enrolled namespace with the default RaaS connection",
		"namespace", ns.Name, "keyAcceptance", cfg.Spec.KeyAcceptance)
	return ctrl.Result{}, nil
}

func ReleaseEnrolled(ctx context.Context, c client.Client) (int, error) {
	configs := &saltv1alpha1.SaltKeyConfigList{}
	if err := c.List(ctx, configs, client.MatchingLabels{LabelManagedBy: ManagedByOperator}); err != nil {
		return 0, err
	}
	deleted := 0
	for i := range configs.Items {
		if err := c.Delete(ctx, &configs.Items[i]); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

func (r *EnrolmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Namespace{}).
		Watches(&saltv1alpha1.SaltKeyConfig{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, obj client.Object) []reconcile.Request {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: obj.GetNamespace()}}}
			})).
		Named("enrolment").
		Complete(r)
}
