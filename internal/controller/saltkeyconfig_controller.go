/*
Copyright (c) 2025 Stan Kozarev.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	saltv1alpha1 "github.com/stanimir-kozarev/vcf-salt-operator/api/v1alpha1"
)

// Ready condition type and reason constants.
const (
	ConditionReady = "Ready"

	ReasonReady           = "Ready"
	ReasonSecretNotFound  = "SecretNotFound"
	ReasonSecretInvalid   = "SecretInvalid"
	ReasonGetSecretFailed = "GetSecretFailed"
	ReasonInvalidDuration = "InvalidDuration"
)

// SaltKeyConfigReconciler reconciles a SaltKeyConfig object.
// It validates the referenced credentials Secret and duration fields,
// then surfaces the result as a Ready status condition.
type SaltKeyConfigReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=salt.vcf.io,resources=saltkeyconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=salt.vcf.io,resources=saltkeyconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=salt.vcf.io,resources=saltkeyconfigs/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile validates the SaltKeyConfig and reports the result as a Ready condition.
// The VirtualMachineReconciler only processes VMs in namespaces whose SaltKeyConfig is Ready.
func (r *SaltKeyConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	cfg := &saltv1alpha1.SaltKeyConfig{}
	if err := r.Get(ctx, req.NamespacedName, cfg); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	original := cfg.DeepCopy()

	cond := r.validate(ctx, cfg)
	apimeta.SetStatusCondition(&cfg.Status.Conditions, cond)
	cfg.Status.ObservedGeneration = cfg.Generation

	if err := r.Status().Patch(ctx, cfg, client.MergeFrom(original)); err != nil {
		return ctrl.Result{}, err
	}

	if cond.Status == metav1.ConditionTrue {
		log.Info("SaltKeyConfig is ready")
	} else {
		log.Info("SaltKeyConfig is not ready", "reason", cond.Reason, "message", cond.Message)
	}
	return ctrl.Result{}, nil
}

// validate checks the SaltKeyConfig spec and returns the appropriate Ready condition.
func (r *SaltKeyConfigReconciler) validate(ctx context.Context, cfg *saltv1alpha1.SaltKeyConfig) metav1.Condition {
	base := metav1.Condition{
		Type:               ConditionReady,
		ObservedGeneration: cfg.Generation,
	}

	// Validate duration fields in a deterministic order.
	for _, fd := range []struct{ name, val string }{
		{"acceptTimeout", cfg.Spec.AcceptTimeout},
		{"requeueInterval", cfg.Spec.RequeueInterval},
	} {
		if fd.val != "" {
			if _, err := time.ParseDuration(fd.val); err != nil {
				base.Status = metav1.ConditionFalse
				base.Reason = ReasonInvalidDuration
				base.Message = fmt.Sprintf("spec.%s %q is not a valid duration: %v", fd.name, fd.val, err)
				return base
			}
		}
	}

	// Validate credentials Secret
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      cfg.Spec.CredentialsSecret,
		Namespace: cfg.Namespace,
	}, secret)
	if err != nil {
		base.Status = metav1.ConditionFalse
		if apierrors.IsNotFound(err) {
			base.Reason = ReasonSecretNotFound
			base.Message = fmt.Sprintf("Secret %q not found in namespace %q", cfg.Spec.CredentialsSecret, cfg.Namespace)
		} else {
			base.Reason = ReasonGetSecretFailed
			base.Message = fmt.Sprintf("Could not read Secret %q: %v", cfg.Spec.CredentialsSecret, err)
		}
		return base
	}

	if len(secret.Data["username"]) == 0 || len(secret.Data["password"]) == 0 {
		base.Status = metav1.ConditionFalse
		base.Reason = ReasonSecretInvalid
		base.Message = fmt.Sprintf("Secret %q must contain non-empty keys \"username\" and \"password\"", cfg.Spec.CredentialsSecret)
		return base
	}

	base.Status = metav1.ConditionTrue
	base.Reason = ReasonReady
	base.Message = "Credentials Secret found and valid"
	return base
}

// SetupWithManager sets up the controller with the Manager.
// Secret reads bypass the cache (configured in main.go) so no cluster-wide
// Secret informer is needed — per-namespace RBAC via namespace-enroll.yaml suffices.
func (r *SaltKeyConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&saltv1alpha1.SaltKeyConfig{}).
		Named("saltkeyconfig").
		Complete(r)
}
