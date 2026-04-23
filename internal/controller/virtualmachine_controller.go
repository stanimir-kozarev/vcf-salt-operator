/*
Copyright (c) 2025 Stan Kozarev.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/go-logr/logr"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	saltv1alpha1 "github.com/stanimir-kozarev/vcf-salt-operator/api/v1alpha1"
	"github.com/stanimir-kozarev/vcf-salt-operator/pkg/salt"
)

// saltFinalizer is added to VMs once their key is accepted, ensuring reconcileDelete
// runs before the object is removed from the API server.
const saltFinalizer = "salt.vcf.io/cleanup"

// Salt key management annotation keys written by this operator onto VirtualMachine resources.
const (
	// AnnotationManaged opts a VirtualMachine into Salt key management.
	// Namespace owner sets this to "true" on the VM spec.
	AnnotationManaged = "salt.vcf.io/managed"

	// AnnotationKeyStatus is the lifecycle state written by the operator.
	// Values: Pending | Accepted | Failed | Deleted | DeleteFailed
	AnnotationKeyStatus = "salt.vcf.io/key-status"

	// AnnotationMinionID records which Salt minion ID was matched and accepted.
	AnnotationMinionID = "salt.vcf.io/minion-id"

	// AnnotationMatchMethod records which matching strategy was used.
	AnnotationMatchMethod = "salt.vcf.io/match-method"
)

// virtualMachineGVK is the GroupVersionKind for the VM Service VirtualMachine resource.
// Using unstructured avoids a Go dependency on the vmoperator types package.
var virtualMachineGVK = schema.GroupVersionKind{
	Group:   "vmoperator.vmware.com",
	Version: "v1alpha4",
	Kind:    "VirtualMachine",
}

// SaltClientFactory creates a salt.Client from connection parameters.
// Injected at startup (uses salt.NewRaaSClient); replaced with a mock in tests.
// log is forwarded to the client for V(1) debug output; pass logr.Discard() to suppress.
type SaltClientFactory func(raasURL, username, password, masterID string, skipTLS bool, log logr.Logger) salt.Client

// saltClientCacheTTL is the maximum lifetime of a cached logged-in client.
// Shorter than a typical JWT lifetime, but avoids a re-login on every reconcile.
const saltClientCacheTTL = 20 * time.Minute

// saltClientCache is a thread-safe cache of logged-in RaaS clients, keyed by SaltKeyConfig.
// Entries are invalidated when the config ResourceVersion changes or the TTL expires.
type saltClientCache struct {
	mu      sync.Mutex
	entries map[types.NamespacedName]saltCacheEntry
}

type saltCacheEntry struct {
	client    salt.Client
	configRV  string
	expiresAt time.Time
}

func (c *saltClientCache) get(key types.NamespacedName, configRV string) (salt.Client, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if e.configRV != configRV || time.Now().After(e.expiresAt) {
		delete(c.entries, key)
		return nil, false
	}
	return e.client, true
}

func (c *saltClientCache) set(key types.NamespacedName, configRV string, cl salt.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = saltCacheEntry{
		client:    cl,
		configRV:  configRV,
		expiresAt: time.Now().Add(saltClientCacheTTL),
	}
}

// VirtualMachineReconciler watches VirtualMachine resources cluster-wide and manages
// Salt minion key acceptance/deletion for VMs that opt in via annotation.
type VirtualMachineReconciler struct {
	client.Client
	SaltClientFactory       SaltClientFactory
	Recorder                record.EventRecorder
	MaxConcurrentReconciles int
	cacheOnce               sync.Once
	saltCache               *saltClientCache
}

// getCache returns the reconciler's shared client cache, initialising it on first call.
func (r *VirtualMachineReconciler) getCache() *saltClientCache {
	r.cacheOnce.Do(func() {
		r.saltCache = &saltClientCache{entries: make(map[types.NamespacedName]saltCacheEntry)}
	})
	return r.saltCache
}

// +kubebuilder:rbac:groups=vmoperator.vmware.com,resources=virtualmachines,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=vmoperator.vmware.com,resources=virtualmachines/status,verbs=get
// +kubebuilder:rbac:groups=salt.vcf.io,resources=saltkeyconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// Reconcile is called on every VirtualMachine create/update/delete event.
// It routes to the delete flow when DeletionTimestamp is set, and the create/accept
// flow otherwise.
func (r *VirtualMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(virtualMachineGVK)
	if err := r.Get(ctx, req.NamespacedName, vm); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Only manage VMs that opted in
	if vm.GetAnnotations()[AnnotationManaged] != "true" {
		return ctrl.Result{}, nil
	}

	// Only manage VMs in enrolled namespaces
	saltConfig, err := r.findSaltKeyConfig(ctx, req.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if saltConfig == nil {
		return ctrl.Result{}, nil
	}

	log.Info("Reconciling VirtualMachine",
		"name", vm.GetName(),
		"namespace", vm.GetNamespace(),
		"saltConfig", saltConfig.Name,
	)

	// Route: delete flow vs create/accept flow
	if !vm.GetDeletionTimestamp().IsZero() {
		return r.reconcileDelete(ctx, vm, saltConfig)
	}
	return r.reconcileCreate(ctx, vm, saltConfig)
}

// reconcileCreate implements Phase 5: accept the Salt minion key when the VM reaches Running.
// It is idempotent — already-accepted VMs are skipped immediately.
func (r *VirtualMachineReconciler) reconcileCreate(
	ctx context.Context,
	vm *unstructured.Unstructured,
	cfg *saltv1alpha1.SaltKeyConfig,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Idempotency: already accepted — ensure finalizer is still present.
	// The finalizer may be absent on VMs accepted before a prior operator version
	// that lacked finalizer support (upgrade safety).
	if vm.GetAnnotations()[AnnotationKeyStatus] == "Accepted" {
		if !controllerutil.ContainsFinalizer(vm, saltFinalizer) {
			fp := client.MergeFrom(vm.DeepCopy())
			controllerutil.AddFinalizer(vm, saltFinalizer)
			if err := r.Patch(ctx, vm, fp); err != nil {
				return ctrl.Result{}, fmt.Errorf("re-add finalizer to %s: %w", vm.GetName(), err)
			}
		}
		return ctrl.Result{}, nil
	}

	// Parse config intervals (defaults are set in CRD but parse defensively)
	acceptTimeout := parseDurationOrDefault(cfg.Spec.AcceptTimeout, 10*time.Minute)
	requeueAfter := parseDurationOrDefault(cfg.Spec.RequeueInterval, 30*time.Second)

	// Wait for VM to be running with an IP before checking for pending keys.
	// v1alpha1 VMs expose status.phase; v1alpha4 VMs expose status.powerState.
	// Either satisfies "running". We additionally require status.network.primaryIP4
	// so the minion has had a chance to boot and contact RaaS.
	phase, _, _ := unstructured.NestedString(vm.Object, "status", "phase")
	powerState, _, _ := unstructured.NestedString(vm.Object, "status", "powerState")
	primaryIP, _, _ := unstructured.NestedString(vm.Object, "status", "network", "primaryIP4")
	running := phase == "Running" || powerState == "PoweredOn"
	if !running || primaryIP == "" {
		log.Info("Waiting for VM to be running with an IP",
			"name", vm.GetName(),
			"phase", phase, "powerState", powerState, "primaryIP", primaryIP)
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	// Get a logged-in RaaS client (cached per SaltKeyConfig, reused across reconciles).
	saltClient, err := r.saltClientFromConfig(ctx, cfg)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get salt client for %s: %w", vm.GetName(), err)
	}

	pendingKeys, err := saltClient.ListPendingKeys(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("list pending keys for %s: %w", vm.GetName(), err)
	}

	// Match VM to a pending key using name, IP, shifted-IP, or prefix strategies
	match := salt.FindMinionID(vm.GetName(), primaryIP, pendingKeys)
	if match.Method != salt.MatchNone {
		log.Info("Matched pending Salt key",
			"name", vm.GetName(),
			"minionID", match.MinionID,
			"matchMethod", match.Method,
		)
		if err := saltClient.AcceptKey(ctx, match.MinionID); err != nil {
			return ctrl.Result{}, fmt.Errorf("accept key %q for %s: %w", match.MinionID, vm.GetName(), err)
		}
		if err := r.patchAnnotations(ctx, vm, map[string]string{
			AnnotationKeyStatus:   "Accepted",
			AnnotationMinionID:    match.MinionID,
			AnnotationMatchMethod: string(match.Method),
		}); err != nil {
			return ctrl.Result{}, err
		}
		// Add finalizer so reconcileDelete fires when the VM is removed
		if !controllerutil.ContainsFinalizer(vm, saltFinalizer) {
			finalizerPatch := client.MergeFrom(vm.DeepCopy())
			controllerutil.AddFinalizer(vm, saltFinalizer)
			if err := r.Patch(ctx, vm, finalizerPatch); err != nil {
				return ctrl.Result{}, err
			}
		}
		r.Recorder.Event(vm, corev1.EventTypeNormal, "SaltKeyAccepted",
			fmt.Sprintf("Salt key accepted for minion %q (match: %s)", match.MinionID, match.Method))
		return ctrl.Result{}, nil
	}

	// No matching pending key yet — check timeout
	elapsed := time.Since(vm.GetCreationTimestamp().Time)
	if elapsed < acceptTimeout {
		log.Info("No matching pending key yet, requeueing",
			"name", vm.GetName(),
			"elapsed", elapsed.Round(time.Second),
			"timeout", acceptTimeout,
		)
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	// Timeout exceeded — mark as Failed
	log.Info("Accept timeout exceeded, marking as Failed", "name", vm.GetName(), "elapsed", elapsed.Round(time.Second))
	if err := r.patchAnnotations(ctx, vm, map[string]string{
		AnnotationKeyStatus: "Failed",
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Event(vm, corev1.EventTypeWarning, "SaltKeyTimeout",
		fmt.Sprintf("No pending Salt key found for %q after %s", vm.GetName(), acceptTimeout))
	return ctrl.Result{}, nil
}

// reconcileDelete implements Phase 6: fire-and-forget key deletion on VM removal.
// Key deletion errors are logged but do NOT block VM deletion.
func (r *VirtualMachineReconciler) reconcileDelete(
	ctx context.Context,
	vm *unstructured.Unstructured,
	cfg *saltv1alpha1.SaltKeyConfig,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	minionID := vm.GetAnnotations()[AnnotationMinionID]
	if minionID == "" {
		// No key was ever accepted — nothing to delete
		return ctrl.Result{}, nil
	}

	saltClient, err := r.saltClientFromConfig(ctx, cfg)
	if err != nil {
		log.Error(err, "Could not get salt client for key deletion, skipping", "name", vm.GetName())
		return ctrl.Result{}, nil
	}

	deleteErr := saltClient.DeleteKey(ctx, minionID)
	status := "Deleted"
	eventMsg := fmt.Sprintf("Salt key deleted for minion %q", minionID)
	if deleteErr != nil {
		log.Error(deleteErr, "Could not delete Salt key (fire-and-forget)", "name", vm.GetName(), "minionID", minionID)
		status = "DeleteFailed"
		eventMsg = fmt.Sprintf("Salt key deletion failed for minion %q: %v", minionID, deleteErr)
	} else {
		log.Info("Deleted Salt key", "name", vm.GetName(), "minionID", minionID)
	}

	_ = r.patchAnnotations(ctx, vm, map[string]string{AnnotationKeyStatus: status})
	r.Recorder.Event(vm, corev1.EventTypeNormal, "SaltKeyDeleted", eventMsg)

	// Remove finalizer to unblock VM deletion from the API server.
	// Re-fetch so the patch uses a fresh ResourceVersion and the cache-backed Get
	// returns a well-typed *StatusError on NotFound (unlike the dynamic-client Patch
	// error which may not be reliably identified by apierrors.IsNotFound).
	// A concurrent reconcile may have already removed the finalizer and the VM may
	// have been garbage-collected — both outcomes are treated as success.
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(virtualMachineGVK)
	if err := r.Get(ctx, client.ObjectKeyFromObject(vm), latest); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if controllerutil.ContainsFinalizer(latest, saltFinalizer) {
		fp := client.MergeFrom(latest.DeepCopy())
		controllerutil.RemoveFinalizer(latest, saltFinalizer)
		if err := r.Patch(ctx, latest, fp); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	return ctrl.Result{}, nil
}

// findSaltKeyConfig lists SaltKeyConfig CRs in the given namespace and returns the first one
// whose Ready condition is True. Returns nil (no error) if none is ready — namespace is unenrolled.
func (r *VirtualMachineReconciler) findSaltKeyConfig(ctx context.Context, namespace string) (*saltv1alpha1.SaltKeyConfig, error) {
	configList := &saltv1alpha1.SaltKeyConfigList{}
	if err := r.List(ctx, configList, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for i := range configList.Items {
		cfg := &configList.Items[i]
		if cfg.IsReady() {
			return cfg, nil
		}
	}
	return nil, nil
}

// SetupWithManager registers the VirtualMachine controller with the operator Manager.
// In addition to watching VMs, it watches SaltKeyConfig so that when a config
// transitions to Ready=True, all VMs in that namespace are re-queued immediately.
func (r *VirtualMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(virtualMachineGVK)

	b := ctrl.NewControllerManagedBy(mgr).
		For(vm).
		Watches(
			&saltv1alpha1.SaltKeyConfig{},
			handler.EnqueueRequestsFromMapFunc(r.saltKeyConfigToVMs),
		).
		Named("virtualmachine")
	if r.MaxConcurrentReconciles > 0 {
		b = b.WithOptions(ctrlcontroller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles})
	}
	return b.Complete(r)
}

// saltKeyConfigToVMs maps a SaltKeyConfig change event to reconcile requests for
// all VirtualMachines in that namespace that have the managed annotation set.
func (r *VirtualMachineReconciler) saltKeyConfigToVMs(ctx context.Context, obj client.Object) []reconcile.Request {
	ns := obj.GetNamespace()

	vmList := &unstructured.UnstructuredList{}
	vmList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   virtualMachineGVK.Group,
		Version: virtualMachineGVK.Version,
		Kind:    virtualMachineGVK.Kind + "List",
	})
	if err := r.List(ctx, vmList, client.InNamespace(ns)); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, vm := range vmList.Items {
		if vm.GetAnnotations()[AnnotationManaged] != "true" {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      vm.GetName(),
				Namespace: vm.GetNamespace(),
			},
		})
	}
	return requests
}

// saltClientFromConfig returns a logged-in RaaS client for the given SaltKeyConfig.
// Clients are cached by config namespace/name and invalidated when the ResourceVersion
// changes (config or secret rotated) or after saltClientCacheTTL.
func (r *VirtualMachineReconciler) saltClientFromConfig(
	ctx context.Context,
	cfg *saltv1alpha1.SaltKeyConfig,
) (salt.Client, error) {
	key := types.NamespacedName{Name: cfg.Name, Namespace: cfg.Namespace}
	if cl, ok := r.getCache().get(key, cfg.ResourceVersion); ok {
		return cl, nil
	}

	// Cache miss — read credentials and create a new logged-in client.
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      cfg.Spec.CredentialsSecret,
		Namespace: cfg.Namespace,
	}, secret); err != nil {
		return nil, err
	}

	username := string(secret.Data["username"])
	password := string(secret.Data["password"])

	cl := r.SaltClientFactory(cfg.Spec.RaasURL, username, password, cfg.Spec.MasterID, cfg.Spec.SkipTLSVerify, logf.FromContext(ctx))

	loginCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := cl.Login(loginCtx); err != nil {
		return nil, fmt.Errorf("salt login for %s/%s: %w", cfg.Namespace, cfg.Name, err)
	}

	r.getCache().set(key, cfg.ResourceVersion, cl)
	return cl, nil
}

// patchAnnotations merges the given key/value pairs into the VM's annotations using a strategic merge patch.
func (r *VirtualMachineReconciler) patchAnnotations(
	ctx context.Context,
	vm *unstructured.Unstructured,
	additions map[string]string,
) error {
	patch := client.MergeFrom(vm.DeepCopy())
	annotations := vm.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	maps.Copy(annotations, additions)
	vm.SetAnnotations(annotations)
	return r.Patch(ctx, vm, patch)
}

// parseDurationOrDefault parses a Go duration string (e.g. "10m", "30s").
// Returns the fallback if the string is empty or unparseable.
func parseDurationOrDefault(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}
