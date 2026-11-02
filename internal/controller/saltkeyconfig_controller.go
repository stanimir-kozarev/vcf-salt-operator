/*
SPDX-License-Identifier: MIT-0
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	saltv1alpha1 "github.com/stanimir-kozarev/vcf-salt-operator/api/v1alpha1"
	"github.com/stanimir-kozarev/vcf-salt-operator/internal/raasconn"
)

const (
	ConditionReady = "Ready"

	ReasonReady              = "Ready"
	ReasonSecretNotFound     = raasconn.ReasonSecretNotFound
	ReasonSecretInvalid      = raasconn.ReasonSecretInvalid
	ReasonGetSecretFailed    = raasconn.ReasonGetSecretFailed
	ReasonIncompleteSpec     = raasconn.ReasonIncompleteSpec
	ReasonDefaultNotSet      = raasconn.ReasonDefaultNotSet
	ReasonConnectionMismatch = raasconn.ReasonConnectionMismatch
	ReasonInvalidDuration    = "InvalidDuration"
	ReasonInvalidSelector    = "InvalidSelector"
)

const secretRetryInterval = 30 * time.Second

var secretReasons = map[string]bool{
	ReasonSecretNotFound:     true,
	ReasonSecretInvalid:      true,
	ReasonGetSecretFailed:    true,
	ReasonConnectionMismatch: true,
}

type SaltKeyConfigReconciler struct {
	client.Client
	DefaultConnection raasconn.Default
}

// +kubebuilder:rbac:groups=salt.vcf.io,resources=saltkeyconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=salt.vcf.io,resources=saltkeyconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=salt.vcf.io,resources=saltkeyconfigs/finalizers,verbs=update
// +kubebuilder:rbac:groups="",namespace=system,resources=secrets,verbs=get

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
		return ctrl.Result{}, nil
	}
	if secretReasons[cond.Reason] {
		log.Info("SaltKeyConfig is not ready", "reason", cond.Reason, "message", cond.Message,
			"retryAfter", secretRetryInterval)
		return ctrl.Result{RequeueAfter: secretRetryInterval}, nil
	}
	log.Info("SaltKeyConfig is not ready", "reason", cond.Reason, "message", cond.Message)
	return ctrl.Result{}, nil
}

func (r *SaltKeyConfigReconciler) validate(ctx context.Context, cfg *saltv1alpha1.SaltKeyConfig) metav1.Condition {
	base := metav1.Condition{
		Type:               ConditionReady,
		ObservedGeneration: cfg.Generation,
	}

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

	if cfg.Spec.VMSelector != nil {
		if _, err := metav1.LabelSelectorAsSelector(cfg.Spec.VMSelector); err != nil {
			base.Status = metav1.ConditionFalse
			base.Reason = ReasonInvalidSelector
			base.Message = fmt.Sprintf("spec.vmSelector is not a valid label selector: %v", err)
			return base
		}
	}

	conn, err := raasconn.Resolve(ctx, r.Client, cfg, r.DefaultConnection)
	if err != nil {
		base.Status = metav1.ConditionFalse
		var resolveErr *raasconn.Error
		if errors.As(err, &resolveErr) {
			base.Reason = resolveErr.Reason
			base.Message = resolveErr.Message
		} else {
			base.Reason = ReasonGetSecretFailed
			base.Message = err.Error()
		}
		return base
	}

	base.Status = metav1.ConditionTrue
	base.Reason = ReasonReady
	if conn.Source == raasconn.SourceDefaultConnection {
		base.Message = "Using the operator's default RaaS connection, TLS verification on"
		if conn.SkipTLSVerify {
			base.Message = "Using the operator's default RaaS connection, TLS verification skipped"
		}
	} else {
		base.Message = "Credentials Secret found and valid"
	}
	return base
}

func (r *SaltKeyConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&saltv1alpha1.SaltKeyConfig{}).
		Named("saltkeyconfig").
		Complete(r)
}
