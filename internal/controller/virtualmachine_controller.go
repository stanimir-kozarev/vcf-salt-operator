/*
SPDX-License-Identifier: MIT-0
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	saltv1alpha1 "github.com/stanimir-kozarev/vcf-salt-operator/api/v1alpha1"
	"github.com/stanimir-kozarev/vcf-salt-operator/internal/raasconn"
	"github.com/stanimir-kozarev/vcf-salt-operator/pkg/salt"
)

const saltFinalizer = "salt.vcf.io/cleanup"

const postAcceptStepTimeout = 2 * time.Minute

var minMasterSilence = time.Minute

const eventMasterSilent = "SaltMasterSilent"

const postAcceptDelay = 30 * time.Second

const maxBootstrapDuration = 10 * time.Minute

const intentHashVersion = "v2:"

const failedStatusPrefix = "Failed/"

const saltStatusReady = "Ready"

const saltStatusHighstateDispatched = "HighstateDispatched"

const (
	keyStatusAccepted     = "Accepted"
	keyStatusFailed       = "Failed"
	keyStatusDeleted      = "Deleted"
	keyStatusPreseeded    = "Preseeded"
	matchMethodPreseed    = "preseed"
	bootstrapSecretSuffix = "-salt-bootstrap"
	bootstrapKeyCleared   = "cleared"
	guestSignalKey        = "salt-key-written"
)

const (
	annotationTrue  = "true"
	annotationFalse = "false"
)

const (
	AnnotationManaged = "salt.vcf.io/managed"

	AnnotationKeyStatus = "salt.vcf.io/key-status"

	AnnotationMinionID = "salt.vcf.io/minion-id"

	AnnotationMatchMethod = "salt.vcf.io/match-method"

	AnnotationSaltStatus = "salt.vcf.io/salt-status"

	AnnotationReady = "salt.vcf.io/ready"

	AnnotationHighstateJID = "salt.vcf.io/highstate-jid"

	AnnotationHighstateStatus = "salt.vcf.io/highstate-status"

	AnnotationHighstateTime = "salt.vcf.io/highstate-time"

	AnnotationRoles = "salt.vcf.io/roles"

	AnnotationRolesHash = "salt.vcf.io/roles-hash"

	AnnotationAcceptedAt = "salt.vcf.io/accepted-at"

	AnnotationKeyCleared = "salt.vcf.io/key-cleared"

	AnnotationHighstateFailedCount = "salt.vcf.io/highstate-failed-count"

	AnnotationHighstateFailedStates = "salt.vcf.io/highstate-failed-states"

	AnnotationRetryRequest = "salt.vcf.io/retry-request"

	AnnotationRetryHandled = "salt.vcf.io/retry-handled"

	AnnotationBulkRetryHandled = "salt.vcf.io/bulk-retry-handled"
)

var nonIntentAnnotations = map[string]bool{
	AnnotationKeyStatus:             true,
	AnnotationMinionID:              true,
	AnnotationMatchMethod:           true,
	AnnotationSaltStatus:            true,
	AnnotationReady:                 true,
	AnnotationHighstateJID:          true,
	AnnotationHighstateStatus:       true,
	AnnotationHighstateTime:         true,
	AnnotationRolesHash:             true,
	AnnotationAcceptedAt:            true,
	AnnotationKeyCleared:            true,
	AnnotationHighstateFailedCount:  true,
	AnnotationHighstateFailedStates: true,
	AnnotationRetryHandled:          true,
	AnnotationBulkRetryHandled:      true,

	AnnotationRetryRequest: true,
}

var virtualMachineGVK = schema.GroupVersionKind{
	Group:   "vmoperator.vmware.com",
	Version: "v1alpha4",
	Kind:    "VirtualMachine",
}

type SaltClientFactory func(raasURL, username, password, masterID string, skipTLS bool, log logr.Logger) salt.Client

const saltClientCacheTTL = 20 * time.Minute

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

type VirtualMachineReconciler struct {
	client.Client
	SaltClientFactory       SaltClientFactory
	Recorder                record.EventRecorder
	DefaultConnection       raasconn.Default
	MaxConcurrentReconciles int
	cacheOnce               sync.Once
	saltCache               *saltClientCache
}

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
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=create;update;patch

func (r *VirtualMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(virtualMachineGVK)
	if err := r.Get(ctx, req.NamespacedName, vm); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if vm.GetAnnotations()[AnnotationManaged] != annotationTrue {
		return ctrl.Result{}, nil
	}

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

	if !vm.GetDeletionTimestamp().IsZero() {
		return r.reconcileDelete(ctx, vm, saltConfig)
	}
	if !matchesVMSelector(saltConfig, vm) {
		log.V(1).Info("VirtualMachine does not match the SaltKeyConfig vmSelector, skipping")
		return ctrl.Result{}, nil
	}
	return r.reconcileCreate(ctx, vm, saltConfig)
}

func (r *VirtualMachineReconciler) reconcileCreate(
	ctx context.Context,
	vm *unstructured.Unstructured,
	cfg *saltv1alpha1.SaltKeyConfig,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	requeueAfter := parseDurationOrDefault(cfg.Spec.RequeueInterval, 30*time.Second)
	acceptTimeout := parseDurationOrDefault(cfg.Spec.AcceptTimeout, 10*time.Minute)
	elapsed, created := sinceVSphereCreate(vm)
	timedOut := created && elapsed >= acceptTimeout

	switch {
	case guestSignalled(vm):
		if err := r.clearPreseedKey(ctx, vm, "the guest signal"); err != nil {
			return ctrl.Result{}, err
		}
	case timedOut:
		if err := r.clearPreseedKey(ctx, vm, "acceptTimeout "+acceptTimeout.String()); err != nil {
			return ctrl.Result{}, err
		}
	}

	if vm.GetAnnotations()[AnnotationKeyStatus] != "Accepted" {
		preseeded := vm.GetAnnotations()[AnnotationKeyStatus] == keyStatusPreseeded
		if vm.GetAnnotations()[AnnotationKeyStatus] == keyStatusFailed &&
			vm.GetAnnotations()[AnnotationMatchMethod] == matchMethodPreseed {
			return ctrl.Result{}, nil
		}
		if cfg.Spec.KeyAcceptance == saltv1alpha1.KeyAcceptancePreseed && !preseeded {
			saltClient, err := r.saltClientFromConfig(ctx, cfg)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("get salt client for %s: %w", vm.GetName(), err)
			}
			return r.runPreseedKey(ctx, vm, saltClient, requeueAfter)
		}
		phase, _, _ := unstructured.NestedString(vm.Object, "status", "phase")
		powerState, _, _ := unstructured.NestedString(vm.Object, "status", "powerState")
		primaryIP, _, _ := unstructured.NestedString(vm.Object, "status", "network", "primaryIP4")
		running := phase == "Running" || powerState == "PoweredOn"
		if (!running || primaryIP == "") && preseeded && timedOut {
			if err := r.patchAnnotations(ctx, vm, map[string]string{AnnotationKeyStatus: keyStatusFailed}); err != nil {
				return ctrl.Result{}, err
			}
			r.Recorder.Event(vm, corev1.EventTypeWarning, "SaltKeyTimeout",
				fmt.Sprintf("VM not running with an IP %s after creation in vSphere, recreate it for a new key", acceptTimeout))
			return ctrl.Result{}, nil
		}
		if !running || primaryIP == "" {
			log.Info("Waiting for VM to be running with an IP",
				"name", vm.GetName(),
				"phase", phase, "powerState", powerState, "primaryIP", primaryIP)
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
		}
		if preseeded {
			return r.markPreseedAccepted(ctx, vm, requeueAfter)
		}
		saltClient, err := r.saltClientFromConfig(ctx, cfg)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("get salt client for %s: %w", vm.GetName(), err)
		}
		return r.runAcceptKey(ctx, vm, saltClient, primaryIP, acceptTimeout, requeueAfter)
	}

	if !controllerutil.ContainsFinalizer(vm, saltFinalizer) {
		fp := client.MergeFrom(vm.DeepCopy())
		controllerutil.AddFinalizer(vm, saltFinalizer)
		if err := r.Patch(ctx, vm, fp); err != nil {
			return ctrl.Result{}, fmt.Errorf("re-add finalizer to %s: %w", vm.GetName(), err)
		}
	}

	saltClient, err := r.saltClientFromConfig(ctx, cfg)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get salt client for %s: %w", vm.GetName(), err)
	}
	minionID := vm.GetAnnotations()[AnnotationMinionID]
	return r.reconcilePostAccept(ctx, vm, saltClient, cfg, minionID, requeueAfter)
}

func (r *VirtualMachineReconciler) runAcceptKey(
	ctx context.Context,
	vm *unstructured.Unstructured,
	saltClient salt.Client,
	primaryIP string,
	acceptTimeout, requeueAfter time.Duration,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	pendingKeys, err := saltClient.ListPendingKeys(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("list pending keys for %s: %w", vm.GetName(), err)
	}

	match := salt.FindMinionID(vm.GetName(), primaryIP, pendingKeys)
	if match.Method != salt.MatchNone {
		log.Info("Matched pending Salt key",
			"name", vm.GetName(), "minionID", match.MinionID, "matchMethod", match.Method)
		if err := saltClient.AcceptKey(ctx, match.MinionID); err != nil {
			return ctrl.Result{}, fmt.Errorf("accept key %q for %s: %w", match.MinionID, vm.GetName(), err)
		}
		if err := r.patchAnnotations(ctx, vm, map[string]string{
			AnnotationKeyStatus:   "Accepted",
			AnnotationMinionID:    match.MinionID,
			AnnotationMatchMethod: string(match.Method),
			AnnotationAcceptedAt:  time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			return ctrl.Result{}, err
		}
		if !controllerutil.ContainsFinalizer(vm, saltFinalizer) {
			fp := client.MergeFrom(vm.DeepCopy())
			controllerutil.AddFinalizer(vm, saltFinalizer)
			if err := r.Patch(ctx, vm, fp); err != nil {
				return ctrl.Result{}, err
			}
		}
		r.Recorder.Event(vm, corev1.EventTypeNormal, "SaltKeyAccepted",
			fmt.Sprintf("Salt key accepted for minion %q (match: %s)", match.MinionID, match.Method))
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	elapsed, created := sinceVSphereCreate(vm)
	if !created || elapsed < acceptTimeout {
		log.Info("No matching pending key yet, requeueing",
			"name", vm.GetName(), "elapsed", elapsed.Round(time.Second), "timeout", acceptTimeout)
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	if vm.GetAnnotations()[AnnotationKeyStatus] == keyStatusFailed {
		return ctrl.Result{}, nil
	}
	log.Info("Accept timeout exceeded, marking as Failed",
		"name", vm.GetName(), "elapsed", elapsed.Round(time.Second))
	if err := r.patchAnnotations(ctx, vm, map[string]string{AnnotationKeyStatus: "Failed"}); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Event(vm, corev1.EventTypeWarning, "SaltKeyTimeout",
		fmt.Sprintf("No pending Salt key found for %q after %s", vm.GetName(), acceptTimeout))
	return ctrl.Result{}, nil
}

func (r *VirtualMachineReconciler) runPreseedKey(
	ctx context.Context,
	vm *unstructured.Unstructured,
	saltClient salt.Client,
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	minionID := vm.GetName()

	owner, err := r.minionIDOwner(ctx, vm, minionID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if owner != nil {
		ownerName := owner.GetNamespace() + "/" + owner.GetName()
		if !owner.GetDeletionTimestamp().IsZero() {
			log.Info("Minion ID still held by a VM being deleted, requeueing",
				"name", vm.GetName(), "minionID", minionID, "owner", ownerName)
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
		}
		if err := r.patchAnnotations(ctx, vm, map[string]string{AnnotationKeyStatus: keyStatusFailed}); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Event(vm, corev1.EventTypeWarning, "SaltMinionIDInUse",
			fmt.Sprintf("Minion ID %q belongs to VirtualMachine %s, no key generated", minionID, ownerName))
		return ctrl.Result{}, nil
	}

	states, err := saltClient.KeyStates(ctx, minionID)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("key state for %s: %w", minionID, err)
	}
	force := slices.ContainsFunc(states, func(s string) bool { return s != "denied" })
	if slices.Contains(states, "pending") || slices.Contains(states, "rejected") {
		r.Recorder.Event(vm, corev1.EventTypeWarning, "SaltForeignKeyReplaced",
			fmt.Sprintf("Key for minion %q in state %s was not generated by the operator and is replaced",
				minionID, strings.Join(states, ",")))
	}

	genCtx, cancel := context.WithTimeout(ctx, postAcceptStepTimeout)
	defer cancel()
	pair, err := saltClient.GenAcceptKey(genCtx, minionID, force)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("generate key for %s: %w", minionID, err)
	}
	if pair == nil {
		return ctrl.Result{}, fmt.Errorf("generate key for %s: a key appeared on the master after the state check", minionID)
	}
	masterPub, err := saltClient.MasterPublicKey(genCtx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("master public key for %s: %w", minionID, err)
	}
	if err := r.writeBootstrapSecret(ctx, vm, minionID, pair, masterPub); err != nil {
		return ctrl.Result{}, err
	}

	if !controllerutil.ContainsFinalizer(vm, saltFinalizer) {
		fp := client.MergeFrom(vm.DeepCopy())
		controllerutil.AddFinalizer(vm, saltFinalizer)
		if err := r.Patch(ctx, vm, fp); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.patchAnnotations(ctx, vm, map[string]string{
		AnnotationKeyStatus:   keyStatusPreseeded,
		AnnotationMinionID:    minionID,
		AnnotationMatchMethod: matchMethodPreseed,
	}); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("Preseeded Salt key", "name", vm.GetName(), "minionID", minionID, "forced", force)
	r.Recorder.Event(vm, corev1.EventTypeNormal, "SaltKeyPreseeded",
		fmt.Sprintf("Salt key generated and accepted for minion %q, written to Secret %s",
			minionID, vm.GetName()+bootstrapSecretSuffix))
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *VirtualMachineReconciler) minionIDOwner(
	ctx context.Context,
	vm *unstructured.Unstructured,
	minionID string,
) (*unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(virtualMachineGVK.GroupVersion().WithKind("VirtualMachineList"))
	if err := r.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list VirtualMachines: %w", err)
	}
	for i := range list.Items {
		other := &list.Items[i]
		if other.GetUID() != vm.GetUID() && other.GetAnnotations()[AnnotationMinionID] == minionID {
			return other, nil
		}
	}
	return nil, nil
}

func (r *VirtualMachineReconciler) writeBootstrapSecret(
	ctx context.Context,
	vm *unstructured.Unstructured,
	minionID string,
	pair *salt.KeyPair,
	masterPub string,
) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vm.GetName() + bootstrapSecretSuffix,
			Namespace: vm.GetNamespace(),
			Labels:    map[string]string{LabelManagedBy: ManagedByOperator},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: vm.GetAPIVersion(),
				Kind:       vm.GetKind(),
				Name:       vm.GetName(),
				UID:        vm.GetUID(),
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"minion.pem":        []byte(pair.Priv),
			"minion.pub":        []byte(pair.Pub),
			"minion_master.pub": []byte(masterPub),
			"minion_id":         []byte(minionID),
		},
	}
	err := r.Create(ctx, secret)
	if apierrors.IsAlreadyExists(err) {
		err = r.Update(ctx, secret)
	}
	if err != nil {
		return fmt.Errorf("write Secret %s/%s: %w", secret.Namespace, secret.Name, err)
	}
	return nil
}

func (r *VirtualMachineReconciler) markPreseedAccepted(
	ctx context.Context,
	vm *unstructured.Unstructured,
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	minionID := vm.GetAnnotations()[AnnotationMinionID]
	if err := r.patchAnnotations(ctx, vm, map[string]string{
		AnnotationKeyStatus:  keyStatusAccepted,
		AnnotationAcceptedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Event(vm, corev1.EventTypeNormal, "SaltKeyAccepted",
		fmt.Sprintf("VM running with the preseeded key for minion %q", minionID))
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *VirtualMachineReconciler) reconcilePostAccept(
	ctx context.Context,
	vm *unstructured.Unstructured,
	saltClient salt.Client,
	cfg *saltv1alpha1.SaltKeyConfig,
	minionID string,
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	if minionID == "" {
		logf.FromContext(ctx).Error(nil, "Accepted VM has no minion-id annotation — skipping post-accept", "name", vm.GetName())
		return ctrl.Result{}, nil
	}
	switch vm.GetAnnotations()[AnnotationSaltStatus] {
	case "":
		if acceptedAt, err := time.Parse(time.RFC3339, vm.GetAnnotations()[AnnotationAcceptedAt]); err == nil {
			if remaining := postAcceptDelay - time.Since(acceptedAt); remaining > 0 {
				logf.FromContext(ctx).Info("Waiting for minion to connect after key acceptance",
					"name", vm.GetName(), "minionID", minionID, "remaining", remaining.Round(time.Second))
				return ctrl.Result{RequeueAfter: remaining}, nil
			}
		}
		return r.runPing(ctx, vm, saltClient, minionID, requeueAfter)
	case "Pinged":
		return r.runRefreshPillar(ctx, vm, saltClient, minionID, requeueAfter)
	case "PillarRefreshed":
		return r.runDispatchHighstate(ctx, vm, saltClient, minionID, requeueAfter)
	case saltStatusHighstateDispatched:
		return r.runPollHighstate(ctx, vm, saltClient, minionID,
			vm.GetAnnotations()[AnnotationHighstateJID], requeueAfter)
	case "Ready":
		return r.checkReconvergence(ctx, vm, saltClient, cfg, minionID, requeueAfter)
	default:
		return r.checkReconvergence(ctx, vm, saltClient, cfg, minionID, requeueAfter)
	}
}

func (r *VirtualMachineReconciler) runPing(
	ctx context.Context,
	vm *unstructured.Unstructured,
	saltClient salt.Client,
	minionID string,
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Running test.ping", "name", vm.GetName(), "minionID", minionID)

	masterBefore, started := masterLastSeen(ctx, saltClient), time.Now()
	pingCtx, cancel := context.WithTimeout(ctx, postAcceptStepTimeout)
	defer cancel()

	ok, err := saltClient.TestPing(pingCtx, minionID)
	if err != nil || !ok {
		msg := "test.ping returned false"
		if err != nil {
			msg = fmt.Sprintf("test.ping: %v", err)
		}
		retryReason, failReason := "SaltPingRetry", "SaltPingFailed"
		if silent := masterSilence(ctx, saltClient, masterBefore, started); silent != "" {
			msg = silent + ", " + msg
			retryReason, failReason = eventMasterSilent, eventMasterSilent
		}
		log.Error(err, "test.ping failed", "name", vm.GetName(), "minionID", minionID, "detail", msg)

		if acceptedAt, parseErr := time.Parse(time.RFC3339, vm.GetAnnotations()[AnnotationAcceptedAt]); parseErr == nil {
			if time.Since(acceptedAt) < maxBootstrapDuration {
				log.Info("test.ping transient failure — retrying in 60s",
					"name", vm.GetName(), "minionID", minionID)
				r.Recorder.Event(vm, corev1.EventTypeWarning, retryReason,
					fmt.Sprintf("test.ping failed for minion %q, retrying: %s", minionID, msg))
				return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
			}
		}

		if cErr := r.clearPreseedKey(ctx, vm, "the failed ping"); cErr != nil {
			return ctrl.Result{}, cErr
		}
		if pErr := r.patchAnnotations(ctx, vm, map[string]string{
			AnnotationSaltStatus: "Failed/Ping",
			AnnotationReady:      annotationFalse,
		}); pErr != nil {
			return ctrl.Result{}, pErr
		}
		r.Recorder.Event(vm, corev1.EventTypeWarning, failReason, msg)
		return ctrl.Result{}, nil
	}

	log.Info("test.ping succeeded", "name", vm.GetName(), "minionID", minionID)
	if err := r.clearPreseedKey(pingCtx, vm, "the first ping"); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.patchAnnotations(ctx, vm, map[string]string{AnnotationSaltStatus: "Pinged"}); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Event(vm, corev1.EventTypeNormal, "SaltPingSucceeded",
		fmt.Sprintf("test.ping succeeded for minion %q", minionID))
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *VirtualMachineReconciler) clearPreseedKey(ctx context.Context, vm *unstructured.Unstructured, trigger string) error {
	if vm.GetAnnotations()[AnnotationMatchMethod] != matchMethodPreseed ||
		vm.GetAnnotations()[AnnotationKeyCleared] == annotationTrue {
		return nil
	}
	if err := r.clearBootstrapKey(ctx, vm); err != nil {
		return err
	}
	if err := r.patchAnnotations(ctx, vm, map[string]string{AnnotationKeyCleared: annotationTrue}); err != nil {
		return err
	}
	minionID := vm.GetAnnotations()[AnnotationMinionID]
	logf.FromContext(ctx).Info("Cleared the private key from the bootstrap Secret",
		"name", vm.GetName(), "minionID", minionID, "trigger", trigger)
	r.Recorder.Event(vm, corev1.EventTypeNormal, "SaltKeyCleared",
		fmt.Sprintf("Private key for minion %q removed from Secret %s after %s",
			minionID, vm.GetName()+bootstrapSecretSuffix, trigger))
	return nil
}

func guestSignalled(vm *unstructured.Unstructured) bool {
	probes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "readinessProbe", "guestInfo")
	if !slices.ContainsFunc(probes, func(p any) bool {
		m, ok := p.(map[string]any)
		return ok && m["key"] == guestSignalKey
	}) {
		return false
	}
	conditions, _, _ := unstructured.NestedSlice(vm.Object, "status", "conditions")
	return slices.ContainsFunc(conditions, func(c any) bool {
		m, ok := c.(map[string]any)
		return ok && m["type"] == vmConditionReady && m["status"] == "True"
	})
}

func sinceVSphereCreate(vm *unstructured.Unstructured) (time.Duration, bool) {
	conditions, _, _ := unstructured.NestedSlice(vm.Object, "status", "conditions")
	for _, c := range conditions {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != vmConditionCreated || m["status"] != "True" {
			continue
		}
		ts, _ := m["lastTransitionTime"].(string)
		createdAt, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			return 0, false
		}
		return time.Since(createdAt), true
	}
	return 0, false
}

func (r *VirtualMachineReconciler) clearBootstrapKey(ctx context.Context, vm *unstructured.Unstructured) error {
	patch, err := json.Marshal(map[string]any{"data": map[string][]byte{
		"minion.pem": []byte(bootstrapKeyCleared),
		"minion.pub": []byte(bootstrapKeyCleared),
	}})
	if err != nil {
		return err
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      vm.GetName() + bootstrapSecretSuffix,
		Namespace: vm.GetNamespace(),
	}}
	if err := r.Patch(ctx, secret, client.RawPatch(types.MergePatchType, patch)); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("clear Secret %s/%s: %w", secret.Namespace, secret.Name, err)
	}
	return nil
}

func (r *VirtualMachineReconciler) runRefreshPillar(
	ctx context.Context,
	vm *unstructured.Unstructured,
	saltClient salt.Client,
	minionID string,
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Running saltutil.refresh_pillar", "name", vm.GetName(), "minionID", minionID)

	masterBefore, started := masterLastSeen(ctx, saltClient), time.Now()
	rfCtx, cancel := context.WithTimeout(ctx, postAcceptStepTimeout)
	defer cancel()

	ok, err := saltClient.RefreshPillar(rfCtx, minionID)
	if err != nil || !ok {
		msg := "refresh_pillar returned false"
		if err != nil {
			msg = fmt.Sprintf("refresh_pillar: %v", err)
		}
		retryReason, failReason := "SaltPillarRefreshRetry", "SaltPillarRefreshFailed"
		if silent := masterSilence(ctx, saltClient, masterBefore, started); silent != "" {
			msg = silent + ", " + msg
			retryReason, failReason = eventMasterSilent, eventMasterSilent
		}
		log.Error(err, "refresh_pillar failed", "name", vm.GetName(), "minionID", minionID, "detail", msg)

		if acceptedAt, parseErr := time.Parse(time.RFC3339, vm.GetAnnotations()[AnnotationAcceptedAt]); parseErr == nil {
			if time.Since(acceptedAt) < maxBootstrapDuration {
				log.Info("refresh_pillar transient failure — retrying in 60s",
					"name", vm.GetName(), "minionID", minionID)
				r.Recorder.Event(vm, corev1.EventTypeWarning, retryReason,
					fmt.Sprintf("refresh_pillar failed for minion %q, retrying: %s", minionID, msg))
				return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
			}
		}

		if pErr := r.patchAnnotations(ctx, vm, map[string]string{
			AnnotationSaltStatus: "Failed/PillarRefresh",
			AnnotationReady:      annotationFalse,
		}); pErr != nil {
			return ctrl.Result{}, pErr
		}
		r.Recorder.Event(vm, corev1.EventTypeWarning, failReason, msg)
		return ctrl.Result{}, nil
	}

	log.Info("refresh_pillar succeeded", "name", vm.GetName(), "minionID", minionID)
	if err := r.patchAnnotations(ctx, vm, map[string]string{AnnotationSaltStatus: "PillarRefreshed"}); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Event(vm, corev1.EventTypeNormal, "SaltPillarRefreshed",
		fmt.Sprintf("refresh_pillar succeeded for minion %q", minionID))
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *VirtualMachineReconciler) runDispatchHighstate(
	ctx context.Context,
	vm *unstructured.Unstructured,
	saltClient salt.Client,
	minionID string,
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Dispatching state.highstate", "name", vm.GetName(), "minionID", minionID)

	dispCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	jid, err := saltClient.DispatchHighstate(dispCtx, minionID)
	if err != nil {
		log.Error(err, "highstate dispatch failed", "name", vm.GetName(), "minionID", minionID)
		if pErr := r.patchAnnotations(ctx, vm, map[string]string{
			AnnotationSaltStatus: "Failed/HighstateDispatch",
			AnnotationReady:      annotationFalse,
		}); pErr != nil {
			return ctrl.Result{}, pErr
		}
		r.Recorder.Event(vm, corev1.EventTypeWarning, "SaltHighstateDispatchFailed",
			fmt.Sprintf("highstate dispatch failed for %q: %v", minionID, err))
		return ctrl.Result{}, nil
	}

	intentHash := computeIntentHash(vm.GetAnnotations())
	if err := r.patchAnnotations(ctx, vm, map[string]string{
		AnnotationSaltStatus:      saltStatusHighstateDispatched,
		AnnotationHighstateJID:    jid,
		AnnotationHighstateStatus: "InProgress",
		AnnotationReady:           annotationFalse,
		AnnotationRolesHash:       intentHash,
	}); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("Highstate dispatched", "name", vm.GetName(), "minionID", minionID, "jid", jid)
	r.Recorder.Event(vm, corev1.EventTypeNormal, "SaltHighstateDispatched",
		fmt.Sprintf("state.highstate dispatched for minion %q (JID: %s)", minionID, jid))
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *VirtualMachineReconciler) runPollHighstate(
	ctx context.Context,
	vm *unstructured.Unstructured,
	saltClient salt.Client,
	minionID, jid string,
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if jid == "" {
		log.Error(nil, "HighstateDispatched but no JID annotation — requeueing", "name", vm.GetName())
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	result, done, err := saltClient.PollJID(ctx, minionID, jid)
	if err != nil {
		log.Error(err, "PollJID transient error — requeueing", "name", vm.GetName(), "jid", jid)
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	if !done {
		log.Info("Highstate not yet complete", "name", vm.GetName(), "jid", jid)
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	failedIDs := result.FailedStateIDs()
	failedStates := strings.Join(failedIDs, ",")
	failedCount := len(failedIDs)

	if result.HighstateOK() {
		log.Info("Highstate succeeded", "name", vm.GetName(), "minionID", minionID, "jid", jid)
		if err := r.patchAnnotations(ctx, vm, map[string]string{
			AnnotationSaltStatus:            "Ready",
			AnnotationReady:                 annotationTrue,
			AnnotationHighstateStatus:       "Success",
			AnnotationHighstateTime:         now,
			AnnotationHighstateFailedCount:  "0",
			AnnotationHighstateFailedStates: "",
		}); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Event(vm, corev1.EventTypeNormal, "SaltHighstateSuccess",
			fmt.Sprintf("state.highstate succeeded for minion %q (JID: %s)", minionID, jid))
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	} else {
		log.Info("Highstate failed", "name", vm.GetName(), "minionID", minionID,
			"jid", jid, "retcode", result.Retcode, "failedStates", failedStates)
		if err := r.patchAnnotations(ctx, vm, map[string]string{
			AnnotationSaltStatus:            "Failed/Highstate",
			AnnotationReady:                 annotationFalse,
			AnnotationHighstateStatus:       "Failed",
			AnnotationHighstateTime:         now,
			AnnotationHighstateFailedCount:  strconv.Itoa(failedCount),
			AnnotationHighstateFailedStates: failedStates,
		}); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Event(vm, corev1.EventTypeWarning, "SaltHighstateFailed",
			fmt.Sprintf("state.highstate failed for minion %q (JID: %s, retcode: %d, failed: %s)",
				minionID, jid, result.Retcode, failedStates))
	}
	return ctrl.Result{}, nil
}

func (r *VirtualMachineReconciler) checkReconvergence(
	ctx context.Context,
	vm *unstructured.Unstructured,
	saltClient salt.Client,
	cfg *saltv1alpha1.SaltKeyConfig,
	minionID string,
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	annotations := vm.GetAnnotations()

	failed := strings.HasPrefix(annotations[AnnotationSaltStatus], failedStatusPrefix)

	previousHash := annotations[AnnotationRolesHash]
	if previousHash == "" {
		if !failed {
			return ctrl.Result{}, nil
		}
		return r.restartFirstChain(ctx, vm, cfg, minionID, requeueAfter)
	}
	if !strings.HasPrefix(previousHash, intentHashVersion) {
		if done, err := r.adoptLegacyIntentHash(ctx, vm, previousHash); done || err != nil {
			return ctrl.Result{}, err
		}
	}

	intentChanged := computeIntentHash(annotations) != previousHash

	retryRequest := annotations[AnnotationRetryRequest]
	retryRequested := retryRequest != "" && retryRequest != annotations[AnnotationRetryHandled]

	var bulkToken string
	if cfg != nil {
		bulkToken = cfg.Spec.RetryToken
	}
	bulkChanged := bulkToken != "" && bulkToken != annotations[AnnotationBulkRetryHandled]

	if !intentChanged && !retryRequested && (!bulkChanged || !failed) {
		if bulkChanged {
			if err := r.patchAnnotations(ctx, vm, map[string]string{
				AnnotationBulkRetryHandled: bulkToken,
			}); err != nil {
				return ctrl.Result{}, err
			}
		}

		if annotations[AnnotationSaltStatus] != saltStatusReady {
			return ctrl.Result{}, nil
		}
		due, wait := enforcementDue(cfg, string(vm.GetUID()), annotations[AnnotationHighstateTime], time.Now().UTC())
		if !due {
			return ctrl.Result{RequeueAfter: wait}, nil
		}
		log.Info("Scheduled enforcement due, re-running refresh_pillar + highstate",
			"name", vm.GetName(), "minionID", minionID,
			"lastHighstate", annotations[AnnotationHighstateTime],
			"interval", cfg.Spec.EnforcementInterval)
	}

	if failed {
		log.Info("Reconvergence triggered from a terminal state, resetting the Salt chain",
			"name", vm.GetName(), "minionID", minionID,
			"from", annotations[AnnotationSaltStatus],
			"intentChanged", intentChanged, "retryRequested", retryRequested,
			"bulkRetry", bulkChanged)
		patch := map[string]string{
			AnnotationSaltStatus:      "",
			AnnotationReady:           annotationFalse,
			AnnotationHighstateStatus: "",
			AnnotationRolesHash:       computeIntentHash(annotations),
		}
		if retryRequested {
			patch[AnnotationRetryHandled] = retryRequest
		}
		if bulkChanged {
			patch[AnnotationBulkRetryHandled] = bulkToken
		}
		if err := r.patchAnnotations(ctx, vm, patch); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Event(vm, corev1.EventTypeNormal, "SaltReconvergenceRequested",
			fmt.Sprintf("Re-running the Salt chain for minion %q after a terminal failure", minionID))
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	log.Info("Reconvergence triggered, running refresh_pillar + highstate",
		"name", vm.GetName(), "minionID", minionID,
		"intentChanged", intentChanged, "retryRequested", retryRequested)

	markers := map[string]string{AnnotationRolesHash: computeIntentHash(annotations)}
	if retryRequested {
		markers[AnnotationRetryHandled] = retryRequest
	}
	if bulkChanged {
		markers[AnnotationBulkRetryHandled] = bulkToken
	}

	return r.runDay2(ctx, vm, saltClient, minionID, markers, requeueAfter)
}

func (r *VirtualMachineReconciler) runDay2(
	ctx context.Context,
	vm *unstructured.Unstructured,
	saltClient salt.Client,
	minionID string,
	markers map[string]string,
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	masterBefore, started := masterLastSeen(ctx, saltClient), time.Now()
	rfCtx, rfCancel := context.WithTimeout(ctx, postAcceptStepTimeout)
	defer rfCancel()
	ok, err := saltClient.RefreshPillar(rfCtx, minionID)
	if err != nil || !ok {
		msg := "Day-2 refresh_pillar returned false"
		if err != nil {
			msg = fmt.Sprintf("Day-2 refresh_pillar: %v", err)
		}
		failReason := "SaltDay2PillarFailed"
		if silent := masterSilence(ctx, saltClient, masterBefore, started); silent != "" {
			msg = silent + ", " + msg
			failReason = eventMasterSilent
		}
		log.Error(err, "Day-2 refresh_pillar failed", "name", vm.GetName(), "minionID", minionID, "detail", msg)
		if pErr := r.patchAnnotations(ctx, vm, withMarkers(markers, map[string]string{
			AnnotationSaltStatus: "Failed/PillarRefresh",
			AnnotationReady:      annotationFalse,
		})); pErr != nil {
			return ctrl.Result{}, pErr
		}
		r.Recorder.Event(vm, corev1.EventTypeWarning, failReason, msg)
		return ctrl.Result{}, nil
	}

	dispCtx, dispCancel := context.WithTimeout(ctx, 30*time.Second)
	defer dispCancel()
	jid, err := saltClient.DispatchHighstate(dispCtx, minionID)
	if err != nil {
		log.Error(err, "Day-2 highstate dispatch failed", "name", vm.GetName(), "minionID", minionID)
		if pErr := r.patchAnnotations(ctx, vm, withMarkers(markers, map[string]string{
			AnnotationSaltStatus: "Failed/HighstateDispatch",
			AnnotationReady:      annotationFalse,
		})); pErr != nil {
			return ctrl.Result{}, pErr
		}
		r.Recorder.Event(vm, corev1.EventTypeWarning, "SaltDay2HighstateFailed",
			fmt.Sprintf("Day-2 highstate dispatch failed: %v", err))
		return ctrl.Result{}, nil
	}

	if err := r.patchAnnotations(ctx, vm, withMarkers(markers, map[string]string{
		AnnotationSaltStatus:      saltStatusHighstateDispatched,
		AnnotationHighstateJID:    jid,
		AnnotationHighstateStatus: "InProgress",
		AnnotationReady:           annotationFalse,
	})); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("Day-2 highstate dispatched", "name", vm.GetName(), "minionID", minionID, "jid", jid)
	r.Recorder.Event(vm, corev1.EventTypeNormal, "SaltDay2Dispatched",
		fmt.Sprintf("Day-2 state.highstate dispatched for minion %q (JID: %s)", minionID, jid))
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func masterLastSeen(ctx context.Context, c salt.Client) string {
	seen, err := c.MasterLastSeen(ctx)
	if err != nil {
		return ""
	}
	return seen
}

func masterSilence(ctx context.Context, c salt.Client, before string, started time.Time) string {
	if before == "" || time.Since(started) < minMasterSilence || masterLastSeen(ctx, c) != before {
		return ""
	}
	return fmt.Sprintf("the Salt master has not reported to RaaS since %s, it is down or cut off from RaaS", before)
}

func withMarkers(markers, patch map[string]string) map[string]string {
	maps.Copy(patch, markers)
	return patch
}

func (r *VirtualMachineReconciler) restartFirstChain(
	ctx context.Context,
	vm *unstructured.Unstructured,
	cfg *saltv1alpha1.SaltKeyConfig,
	minionID string,
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	annotations := vm.GetAnnotations()

	retryRequest := annotations[AnnotationRetryRequest]
	retryRequested := retryRequest != "" && retryRequest != annotations[AnnotationRetryHandled]
	var bulkToken string
	if cfg != nil {
		bulkToken = cfg.Spec.RetryToken
	}
	bulkChanged := bulkToken != "" && bulkToken != annotations[AnnotationBulkRetryHandled]
	if !retryRequested && !bulkChanged {
		return ctrl.Result{}, nil
	}

	logf.FromContext(ctx).Info("Retry requested before the first highstate, restarting the Salt chain",
		"name", vm.GetName(), "minionID", minionID, "from", annotations[AnnotationSaltStatus],
		"retryRequested", retryRequested, "bulkRetry", bulkChanged)
	patch := map[string]string{
		AnnotationSaltStatus: "",
		AnnotationReady:      annotationFalse,
	}
	if retryRequested {
		patch[AnnotationRetryHandled] = retryRequest
	}
	if bulkChanged {
		patch[AnnotationBulkRetryHandled] = bulkToken
	}
	if err := r.patchAnnotations(ctx, vm, patch); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Event(vm, corev1.EventTypeNormal, "SaltReconvergenceRequested",
		fmt.Sprintf("Re-running the Salt chain for minion %q after a failure before its first highstate", minionID))
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func computeIntentHash(annotations map[string]string) string {
	keys := make([]string, 0, len(annotations))
	for k := range annotations {
		if !strings.HasPrefix(k, "salt.vcf.io/") || nonIntentAnnotations[k] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(annotations[k])
		b.WriteByte('\n')
	}
	h := sha256.Sum256([]byte(b.String()))
	return intentHashVersion + hex.EncodeToString(h[:8])
}

func computeLegacyRolesHash(roles string) string {
	h := sha256.Sum256([]byte(roles))
	return hex.EncodeToString(h[:8])
}

func (r *VirtualMachineReconciler) adoptLegacyIntentHash(
	ctx context.Context,
	vm *unstructured.Unstructured,
	storedHash string,
) (bool, error) {
	annotations := vm.GetAnnotations()
	if computeLegacyRolesHash(annotations[AnnotationRoles]) != storedHash {
		return false, nil
	}
	logf.FromContext(ctx).Info("Migrating pre-upgrade intent hash without dispatching",
		"name", vm.GetName())
	if err := r.patchAnnotations(ctx, vm, map[string]string{
		AnnotationRolesHash: computeIntentHash(annotations),
	}); err != nil {
		return true, err
	}
	return true, nil
}

func (r *VirtualMachineReconciler) reconcileDelete(
	ctx context.Context,
	vm *unstructured.Unstructured,
	cfg *saltv1alpha1.SaltKeyConfig,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	minionID := vm.GetAnnotations()[AnnotationMinionID]
	if minionID == "" {
		return ctrl.Result{}, nil
	}
	if vm.GetAnnotations()[AnnotationKeyStatus] == keyStatusDeleted {
		return r.removeSaltFinalizer(ctx, vm)
	}

	saltClient, err := r.saltClientFromConfig(ctx, cfg)
	if err != nil {
		log.Error(err, "Could not get salt client for key deletion, skipping", "name", vm.GetName())
		return ctrl.Result{}, nil
	}

	deleteErr := saltClient.DeleteKey(ctx, minionID)
	status := keyStatusDeleted
	eventType, reason := corev1.EventTypeNormal, "SaltKeyDeleted"
	eventMsg := fmt.Sprintf("Salt key deleted for minion %q", minionID)
	if deleteErr != nil {
		log.Error(deleteErr, "Could not delete Salt key (fire-and-forget)", "name", vm.GetName(), "minionID", minionID)
		status = "DeleteFailed"
		eventType, reason = corev1.EventTypeWarning, "SaltKeyDeleteFailed"
		eventMsg = fmt.Sprintf("Salt key deletion failed for minion %q: %v", minionID, deleteErr)
	} else {
		log.Info("Deleted Salt key", "name", vm.GetName(), "minionID", minionID)
	}

	_ = r.patchAnnotations(ctx, vm, map[string]string{AnnotationKeyStatus: status})
	r.Recorder.Event(vm, eventType, reason, eventMsg)
	return r.removeSaltFinalizer(ctx, vm)
}

func (r *VirtualMachineReconciler) removeSaltFinalizer(
	ctx context.Context,
	vm *unstructured.Unstructured,
) (ctrl.Result, error) {
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

func (r *VirtualMachineReconciler) findSaltKeyConfig(ctx context.Context, namespace string) (*saltv1alpha1.SaltKeyConfig, error) {
	configList := &saltv1alpha1.SaltKeyConfigList{}
	if err := r.List(ctx, configList, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	var enrolled *saltv1alpha1.SaltKeyConfig
	for i := range configList.Items {
		cfg := &configList.Items[i]
		if !cfg.IsReady() {
			continue
		}
		if cfg.Labels[LabelManagedBy] != ManagedByOperator {
			return cfg, nil
		}
		if enrolled == nil {
			enrolled = cfg
		}
	}
	return enrolled, nil
}

func (r *VirtualMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(virtualMachineGVK)

	b := ctrl.NewControllerManagedBy(mgr).
		For(vm, builder.WithPredicates(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool { return !ownWritesOnly(e.ObjectOld, e.ObjectNew) },
		})).
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

func ownWritesOnly(oldObj, newObj client.Object) bool {
	oldU, ok1 := oldObj.(*unstructured.Unstructured)
	newU, ok2 := newObj.(*unstructured.Unstructured)
	if !ok1 || !ok2 {
		return false
	}
	return equality.Semantic.DeepEqual(withoutOwnWrites(oldU), withoutOwnWrites(newU))
}

func withoutOwnWrites(vm *unstructured.Unstructured) map[string]any {
	c := vm.DeepCopy()
	unstructured.RemoveNestedField(c.Object, "metadata", "resourceVersion")
	unstructured.RemoveNestedField(c.Object, "metadata", "managedFields")
	annotations := c.GetAnnotations()
	for k := range annotations {
		if nonIntentAnnotations[k] && k != AnnotationRetryRequest {
			delete(annotations, k)
		}
	}
	delete(annotations, vmBackupVersionAnnotation)
	delete(annotations, vmConversionAnnotation)
	c.SetAnnotations(annotations)

	conds, _, _ := unstructured.NestedSlice(c.Object, "status", "conditions")
	kept := make([]any, 0, len(conds))
	for _, cond := range conds {
		if m, ok := cond.(map[string]any); ok && m["type"] == vmBackupCondition {
			continue
		}
		kept = append(kept, cond)
	}
	if len(kept) == 0 {
		unstructured.RemoveNestedField(c.Object, "status", "conditions")
	} else {
		_ = unstructured.SetNestedSlice(c.Object, kept, "status", "conditions")
	}
	return c.Object
}

const (
	vmBackupVersionAnnotation = "vmoperator.vmware.com/backup-version"
	vmConversionAnnotation    = "vmoperator.vmware.com/conversion"
	vmBackupCondition         = "VirtualMachineBackupUpToDate"
	vmConditionReady          = "Ready"
	vmConditionCreated        = "VirtualMachineCreated"
)

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
		if vm.GetAnnotations()[AnnotationManaged] != annotationTrue {
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

func matchesVMSelector(cfg *saltv1alpha1.SaltKeyConfig, vm *unstructured.Unstructured) bool {
	if cfg.Spec.VMSelector == nil {
		return true
	}
	sel, err := metav1.LabelSelectorAsSelector(cfg.Spec.VMSelector)
	if err != nil {
		return false
	}
	return sel.Matches(labels.Set(vm.GetLabels()))
}

func (r *VirtualMachineReconciler) saltClientFromConfig(
	ctx context.Context,
	cfg *saltv1alpha1.SaltKeyConfig,
) (salt.Client, error) {
	key := types.NamespacedName{Name: cfg.Name, Namespace: cfg.Namespace}
	if cl, ok := r.getCache().get(key, cfg.ResourceVersion); ok {
		return cl, nil
	}

	conn, err := raasconn.Resolve(ctx, r.Client, cfg, r.DefaultConnection)
	if err != nil {
		return nil, err
	}

	cl := r.SaltClientFactory(conn.URL, conn.Username, conn.Password, conn.MasterID, conn.SkipTLSVerify, logf.FromContext(ctx))

	loginCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := cl.Login(loginCtx); err != nil {
		return nil, fmt.Errorf("salt login for %s/%s: %w", cfg.Namespace, cfg.Name, err)
	}

	r.getCache().set(key, cfg.ResourceVersion, cl)
	return cl, nil
}

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

func enforcementDue(cfg *saltv1alpha1.SaltKeyConfig, uid, lastHighstate string, now time.Time) (bool, time.Duration) {
	if cfg == nil || cfg.Spec.EnforcementInterval == "" {
		return false, 0
	}
	interval, err := time.ParseDuration(cfg.Spec.EnforcementInterval)
	if err != nil || interval <= 0 {
		return false, 0
	}
	if lastHighstate == "" {
		return false, 0
	}
	last, err := time.Parse(time.RFC3339, lastHighstate)
	if err != nil {
		return false, 0
	}

	elapsed := now.Sub(last)
	effective := interval + enforcementJitter(uid, interval)
	if elapsed >= effective {
		return true, 0
	}
	return false, effective - elapsed
}

func enforcementJitter(uid string, interval time.Duration) time.Duration {
	spread := interval / 10
	if spread <= 0 || uid == "" {
		return 0
	}
	sum := sha256.Sum256([]byte(uid))
	return time.Duration(binary.BigEndian.Uint64(sum[:8]) % uint64(spread))
}

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
