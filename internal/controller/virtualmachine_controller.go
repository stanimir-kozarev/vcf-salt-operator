/*
Copyright (c) 2025 Stan Kozarev.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"
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

// postAcceptStepTimeout is the context deadline for blocking Salt operations (ping, pillar refresh).
const postAcceptStepTimeout = 2 * time.Minute

// postAcceptDelay is the grace period after key acceptance before the first test.ping.
// Gives the salt-minion time to re-establish its ZeroMQ connection to the master
// before the operator dispatches commands.
const postAcceptDelay = 30 * time.Second

// maxBootstrapDuration caps the total window in which transient ping/pillar-refresh
// failures are retried automatically. After this window expires, the failure is
// terminal and requires operator intervention.
const maxBootstrapDuration = 10 * time.Minute

// intentHashVersion prefixes every digest computeIntentHash produces, so a legacy-format
// hash (no prefix) is distinguishable from a current one. See adoptLegacyIntentHash.
const intentHashVersion = "v2:"

// failedStatusPrefix marks the terminal Failed/<step> states. checkReconvergence is the
// only way out of one.
const failedStatusPrefix = "Failed/"

// Salt annotation keys written by this operator onto VirtualMachine resources.
const (
	// AnnotationManaged opts a VirtualMachine into Salt key management.
	// Namespace owner sets this to "true" on the VM spec.
	AnnotationManaged = "salt.vcf.io/managed"

	// AnnotationKeyStatus is the key-acceptance lifecycle state.
	// Values: Accepted | Failed | Deleted | DeleteFailed
	AnnotationKeyStatus = "salt.vcf.io/key-status"

	// AnnotationMinionID records which Salt minion ID was matched and accepted.
	AnnotationMinionID = "salt.vcf.io/minion-id"

	// AnnotationMatchMethod records which matching strategy was used.
	AnnotationMatchMethod = "salt.vcf.io/match-method"

	// AnnotationSaltStatus is the post-accept Salt management phase.
	// Values: Pinged | PillarRefreshed | HighstateDispatched | Ready | Failed/<step>
	AnnotationSaltStatus = "salt.vcf.io/salt-status"

	// AnnotationReady reflects whether Salt management is complete and successful.
	// Values: "true" | "false"
	AnnotationReady = "salt.vcf.io/ready"

	// AnnotationHighstateJID is the JID of the currently running (or last completed) highstate.
	AnnotationHighstateJID = "salt.vcf.io/highstate-jid"

	// AnnotationHighstateStatus is the outcome of the last highstate execution.
	// Values: InProgress | Success | Failed
	AnnotationHighstateStatus = "salt.vcf.io/highstate-status"

	// AnnotationHighstateTime is the RFC3339 timestamp of the last highstate completion.
	AnnotationHighstateTime = "salt.vcf.io/highstate-time"

	// AnnotationRoles is the roles list set by Argo CD (observed, never written by the operator).
	// The operator watches this annotation for Day-2 change detection.
	AnnotationRoles = "salt.vcf.io/roles"

	// AnnotationRolesHash is the SHA256 hash of the full tenant-declared intent-annotation
	// set (every salt.vcf.io/* key except the operator's own status keys, see
	// operatorStatusAnnotations) at the time of the last highstate dispatch. A mismatch
	// triggers a Day-2 refresh_pillar + highstate cycle. Kept under its original name (this
	// used to hash AnnotationRoles alone, see computeIntentHash's doc comment) since it is
	// already part of the documented intent-vs-status annotation split app teams rely on.
	AnnotationRolesHash = "salt.vcf.io/roles-hash"

	// AnnotationAcceptedAt is the RFC3339 timestamp when the Salt key was accepted.
	// Used to enforce postAcceptDelay before the first test.ping and to bound the
	// total retry window (maxBootstrapDuration) for transient ping/pillar failures.
	AnnotationAcceptedAt = "salt.vcf.io/accepted-at"

	// AnnotationHighstateFailedCount is the number of failed states in the last highstate.
	// "0" means all states succeeded. Safe to expose to app teams (no secret values).
	AnnotationHighstateFailedCount = "salt.vcf.io/highstate-failed-count"

	// AnnotationHighstateFailedStates is a comma-separated list of failed state IDs from
	// the last highstate run. Contains only state map keys, never rendered values or secrets.
	AnnotationHighstateFailedStates = "salt.vcf.io/highstate-failed-states"

	// AnnotationRetryRequest is a tenant-set, Git-delivered trigger asking the operator to
	// re-run the Salt chain. An opaque token compared against AnnotationRetryHandled: any
	// change requests one re-run.
	//
	// The operator never clears this annotation, only records what it acted on. Clearing it
	// would fight Argo CD's selfHeal, since the Git-asserted value would never be satisfied.
	AnnotationRetryRequest = "salt.vcf.io/retry-request"

	// AnnotationRetryHandled records the AnnotationRetryRequest value the operator last
	// acted on. An annotation, not a status field, because this operator does not own the
	// VirtualMachine CRD.
	AnnotationRetryHandled = "salt.vcf.io/retry-handled"

	// AnnotationBulkRetryHandled records the SaltKeyConfig.spec.retryToken value this VM
	// last acted on. Held per VM, not once on the SaltKeyConfig, since a single shared
	// record would race: the first VM to reconcile would mark the token consumed for every
	// other VM in the namespace.
	AnnotationBulkRetryHandled = "salt.vcf.io/bulk-retry-handled"
)

// nonIntentAnnotations is the set of salt.vcf.io/* keys excluded from the intent hash.
// A deny-list, not an allow-list, so a new tenant annotation is picked up for Day-2 change
// detection automatically, with no code change.
//
// Two reasons for exclusion:
//   - Operator-written status: including these would make the operator's own writes look
//     like tenant intent, and every completed highstate would re-trigger itself.
//   - Operational triggers: AnnotationRetryRequest is tenant-written like an intent
//     annotation, but requests an action rather than describing desired configuration, so
//     it is handled explicitly in checkReconvergence instead.
//
// AnnotationManaged and AnnotationRoles are deliberately absent - both are tenant-declared
// configuration, so both belong in the hash.
var nonIntentAnnotations = map[string]bool{
	// Operator-written status
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
	AnnotationHighstateFailedCount:  true,
	AnnotationHighstateFailedStates: true,
	AnnotationRetryHandled:          true,
	AnnotationBulkRetryHandled:      true,

	// Operational triggers
	AnnotationRetryRequest: true,
}

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

// reconcileCreate handles the VM create/update path. Phase 1 accepts the Salt minion key;
// Phases 2–5 run the post-accept bootstrap chain (ping → refresh_pillar → highstate)
// and subsequently watch for Day-2 role changes.
func (r *VirtualMachineReconciler) reconcileCreate(
	ctx context.Context,
	vm *unstructured.Unstructured,
	cfg *saltv1alpha1.SaltKeyConfig,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	requeueAfter := parseDurationOrDefault(cfg.Spec.RequeueInterval, 30*time.Second)

	// Key not yet accepted: wait for the VM to be running with an IP before polling RaaS.
	// v1alpha1 exposes status.phase; v1alpha4 exposes status.powerState.
	if vm.GetAnnotations()[AnnotationKeyStatus] != "Accepted" {
		acceptTimeout := parseDurationOrDefault(cfg.Spec.AcceptTimeout, 10*time.Minute)
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
		saltClient, err := r.saltClientFromConfig(ctx, cfg)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("get salt client for %s: %w", vm.GetName(), err)
		}
		return r.runAcceptKey(ctx, vm, saltClient, primaryIP, acceptTimeout, requeueAfter)
	}

	// Key is accepted — ensure finalizer is present (upgrade-safety: may be absent on pre-finalizer VMs).
	if !controllerutil.ContainsFinalizer(vm, saltFinalizer) {
		fp := client.MergeFrom(vm.DeepCopy())
		controllerutil.AddFinalizer(vm, saltFinalizer)
		if err := r.Patch(ctx, vm, fp); err != nil {
			return ctrl.Result{}, fmt.Errorf("re-add finalizer to %s: %w", vm.GetName(), err)
		}
	}

	// Post-accept phases do not require the VM's IP — the minion communicates with the
	// Salt master independently. We only need a live RaaS client.
	saltClient, err := r.saltClientFromConfig(ctx, cfg)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get salt client for %s: %w", vm.GetName(), err)
	}
	minionID := vm.GetAnnotations()[AnnotationMinionID]
	return r.reconcilePostAccept(ctx, vm, saltClient, cfg, minionID, requeueAfter)
}

// runAcceptKey looks for a pending Salt key that matches this VM and accepts it.
// Returns early (requeue) if no key is found yet; marks Failed after acceptTimeout.
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
		return ctrl.Result{}, nil
	}

	elapsed := time.Since(vm.GetCreationTimestamp().Time)
	if elapsed < acceptTimeout {
		log.Info("No matching pending key yet, requeueing",
			"name", vm.GetName(), "elapsed", elapsed.Round(time.Second), "timeout", acceptTimeout)
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
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

// reconcilePostAccept drives the post-accept state machine:
// "" → Pinged → PillarRefreshed → HighstateDispatched → Ready (then Day-2 loop).
//
// Both Ready and every terminal Failed/<step> state route to checkReconvergence. Failed
// does not mean the state machine will never advance again - it means it will not advance
// on its own, and needs one of checkReconvergence's explicit triggers to leave.
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
		// Enforce grace period: wait postAcceptDelay after key acceptance before the
		// first ping. This gives the salt-minion time to re-establish its ZeroMQ
		// connection — no annotation patch during the wait, so no spurious watch events.
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
	case "HighstateDispatched":
		return r.runPollHighstate(ctx, vm, saltClient, minionID,
			vm.GetAnnotations()[AnnotationHighstateJID], requeueAfter)
	case "Ready":
		return r.checkReconvergence(ctx, vm, saltClient, cfg, minionID, requeueAfter)
	default:
		// Failed/<step>. Not self-advancing, but reachable by an explicit trigger.
		return r.checkReconvergence(ctx, vm, saltClient, cfg, minionID, requeueAfter)
	}
}

// runPing dispatches test.ping and advances the state machine to "Pinged" on success.
func (r *VirtualMachineReconciler) runPing(
	ctx context.Context,
	vm *unstructured.Unstructured,
	saltClient salt.Client,
	minionID string,
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Running test.ping", "name", vm.GetName(), "minionID", minionID)

	pingCtx, cancel := context.WithTimeout(ctx, postAcceptStepTimeout)
	defer cancel()

	ok, err := saltClient.TestPing(pingCtx, minionID)
	if err != nil || !ok {
		msg := "test.ping returned false"
		if err != nil {
			msg = fmt.Sprintf("test.ping: %v", err)
		}
		log.Error(err, "test.ping failed", "name", vm.GetName(), "minionID", minionID)

		// Retry without patching (no watch event) while within maxBootstrapDuration.
		if acceptedAt, parseErr := time.Parse(time.RFC3339, vm.GetAnnotations()[AnnotationAcceptedAt]); parseErr == nil {
			if time.Since(acceptedAt) < maxBootstrapDuration {
				log.Info("test.ping transient failure — retrying in 60s",
					"name", vm.GetName(), "minionID", minionID)
				r.Recorder.Event(vm, corev1.EventTypeWarning, "SaltPingRetry",
					fmt.Sprintf("test.ping failed for minion %q, retrying: %s", minionID, msg))
				return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
			}
		}

		if pErr := r.patchAnnotations(ctx, vm, map[string]string{
			AnnotationSaltStatus: "Failed/Ping",
			AnnotationReady:      "false",
		}); pErr != nil {
			return ctrl.Result{}, pErr
		}
		r.Recorder.Event(vm, corev1.EventTypeWarning, "SaltPingFailed", msg)
		return ctrl.Result{}, nil
	}

	log.Info("test.ping succeeded", "name", vm.GetName(), "minionID", minionID)
	if err := r.patchAnnotations(ctx, vm, map[string]string{AnnotationSaltStatus: "Pinged"}); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Event(vm, corev1.EventTypeNormal, "SaltPingSucceeded",
		fmt.Sprintf("test.ping succeeded for minion %q", minionID))
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// runRefreshPillar dispatches saltutil.refresh_pillar and advances state to "PillarRefreshed".
// Per the load-bearing invariant: refresh_pillar MUST precede every state.highstate.
func (r *VirtualMachineReconciler) runRefreshPillar(
	ctx context.Context,
	vm *unstructured.Unstructured,
	saltClient salt.Client,
	minionID string,
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Running saltutil.refresh_pillar", "name", vm.GetName(), "minionID", minionID)

	rfCtx, cancel := context.WithTimeout(ctx, postAcceptStepTimeout)
	defer cancel()

	ok, err := saltClient.RefreshPillar(rfCtx, minionID)
	if err != nil || !ok {
		msg := "refresh_pillar returned false"
		if err != nil {
			msg = fmt.Sprintf("refresh_pillar: %v", err)
		}
		log.Error(err, "refresh_pillar failed", "name", vm.GetName(), "minionID", minionID)

		if acceptedAt, parseErr := time.Parse(time.RFC3339, vm.GetAnnotations()[AnnotationAcceptedAt]); parseErr == nil {
			if time.Since(acceptedAt) < maxBootstrapDuration {
				log.Info("refresh_pillar transient failure — retrying in 60s",
					"name", vm.GetName(), "minionID", minionID)
				r.Recorder.Event(vm, corev1.EventTypeWarning, "SaltPillarRefreshRetry",
					fmt.Sprintf("refresh_pillar failed for minion %q, retrying: %s", minionID, msg))
				return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
			}
		}

		if pErr := r.patchAnnotations(ctx, vm, map[string]string{
			AnnotationSaltStatus: "Failed/PillarRefresh",
			AnnotationReady:      "false",
		}); pErr != nil {
			return ctrl.Result{}, pErr
		}
		r.Recorder.Event(vm, corev1.EventTypeWarning, "SaltPillarRefreshFailed", msg)
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

// runDispatchHighstate dispatches state.highstate asynchronously and stores the JID.
// The next reconcile will poll the result via runPollHighstate.
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
			AnnotationReady:      "false",
		}); pErr != nil {
			return ctrl.Result{}, pErr
		}
		r.Recorder.Event(vm, corev1.EventTypeWarning, "SaltHighstateDispatchFailed",
			fmt.Sprintf("highstate dispatch failed for %q: %v", minionID, err))
		return ctrl.Result{}, nil
	}

	intentHash := computeIntentHash(vm.GetAnnotations())
	if err := r.patchAnnotations(ctx, vm, map[string]string{
		AnnotationSaltStatus:      "HighstateDispatched",
		AnnotationHighstateJID:    jid,
		AnnotationHighstateStatus: "InProgress",
		AnnotationReady:           "false",
		AnnotationRolesHash:       intentHash,
	}); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("Highstate dispatched", "name", vm.GetName(), "minionID", minionID, "jid", jid)
	r.Recorder.Event(vm, corev1.EventTypeNormal, "SaltHighstateDispatched",
		fmt.Sprintf("state.highstate dispatched for minion %q (JID: %s)", minionID, jid))
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// runPollHighstate makes a single PollJID call and advances state when the job completes.
// If not done it requeues; transient poll errors are retried on the next reconcile.
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
			AnnotationReady:                 "true",
			AnnotationHighstateStatus:       "Success",
			AnnotationHighstateTime:         now,
			AnnotationHighstateFailedCount:  "0",
			AnnotationHighstateFailedStates: "",
		}); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Event(vm, corev1.EventTypeNormal, "SaltHighstateSuccess",
			fmt.Sprintf("state.highstate succeeded for minion %q (JID: %s)", minionID, jid))
	} else {
		log.Info("Highstate failed", "name", vm.GetName(), "minionID", minionID,
			"jid", jid, "retcode", result.Retcode, "failedStates", failedStates)
		if err := r.patchAnnotations(ctx, vm, map[string]string{
			AnnotationSaltStatus:            "Failed/Highstate",
			AnnotationReady:                 "false",
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

// checkReconvergence decides whether a VM that will not advance on its own should re-run
// the Salt chain. Reached from Ready and from every terminal Failed/<step> state, evaluating
// three triggers:
//
//  1. The intent hash changed - Argo CD delivered new desired configuration. Needs no new
//     annotation; alone, this is what lets a second self-service request after a failed
//     first one actually take effect.
//  2. AnnotationRetryRequest differs from AnnotationRetryHandled - covers what a hash
//     cannot: the VM's own intent never changed, but the platform content that broke it has
//     since been fixed, or Ops repaired the VM out of band.
//  3. SaltKeyConfig.spec.retryToken differs from AnnotationBulkRetryHandled - the Ops-side
//     lever for a platform-caused failure that stranded VMs across several tenant repos.
//
// Not gated on AnnotationRoles being non-empty, since a role-less VM still receives the
// unconditional CIS baseline. The real gate is whether a highstate has ever been dispatched
// (an empty AnnotationRolesHash).
//
// No automatic retry: every path needs an explicit trigger, so a broken VM stays visibly
// broken rather than looping against a failure nobody has looked at.
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

	previousHash := annotations[AnnotationRolesHash]
	if previousHash == "" {
		return ctrl.Result{}, nil // no prior dispatch to compare against
	}
	if !strings.HasPrefix(previousHash, intentHashVersion) {
		if done, err := r.adoptLegacyIntentHash(ctx, vm, previousHash); done || err != nil {
			return ctrl.Result{}, err
		}
	}

	failed := strings.HasPrefix(annotations[AnnotationSaltStatus], failedStatusPrefix)
	intentChanged := computeIntentHash(annotations) != previousHash

	retryRequest := annotations[AnnotationRetryRequest]
	retryRequested := retryRequest != "" && retryRequest != annotations[AnnotationRetryHandled]

	var bulkToken string
	if cfg != nil {
		bulkToken = cfg.Spec.RetryToken
	}
	bulkChanged := bulkToken != "" && bulkToken != annotations[AnnotationBulkRetryHandled]

	// A bulk token rescues stranded VMs only. Healthy VMs still record it as handled, so
	// that "retry this environment" cannot be re-armed against a VM that fails weeks later
	// for an unrelated reason.
	if !intentChanged && !retryRequested && (!bulkChanged || !failed) {
		if bulkChanged {
			return ctrl.Result{}, r.patchAnnotations(ctx, vm, map[string]string{
				AnnotationBulkRetryHandled: bulkToken,
			})
		}
		return ctrl.Result{}, nil
	}

	// Reset to the start of the chain rather than resuming at the failed step: ping proves
	// the minion is reachable again, and re-entering at "" keeps refresh_pillar always
	// preceding highstate.
	//
	// Trigger markers are recorded HERE, before the chain runs, not after it succeeds. A
	// retry that fails again would otherwise land back in Failed/<step> with its trigger
	// still unequal to its handled marker, and the operator would retry forever.
	if failed {
		log.Info("Reconvergence triggered from a terminal state, resetting the Salt chain",
			"name", vm.GetName(), "minionID", minionID,
			"from", annotations[AnnotationSaltStatus],
			"intentChanged", intentChanged, "retryRequested", retryRequested,
			"bulkRetry", bulkChanged)
		patch := map[string]string{
			AnnotationSaltStatus:      "",
			AnnotationReady:           "false",
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

	rfCtx, rfCancel := context.WithTimeout(ctx, postAcceptStepTimeout)
	defer rfCancel()
	ok, err := saltClient.RefreshPillar(rfCtx, minionID)
	if err != nil || !ok {
		msg := "Day-2 refresh_pillar returned false"
		if err != nil {
			msg = fmt.Sprintf("Day-2 refresh_pillar: %v", err)
		}
		log.Error(err, "Day-2 refresh_pillar failed", "name", vm.GetName(), "minionID", minionID)
		if pErr := r.patchAnnotations(ctx, vm, map[string]string{
			AnnotationSaltStatus: "Failed/PillarRefresh",
			AnnotationReady:      "false",
		}); pErr != nil {
			return ctrl.Result{}, pErr
		}
		r.Recorder.Event(vm, corev1.EventTypeWarning, "SaltDay2PillarFailed", msg)
		return ctrl.Result{}, nil
	}

	dispCtx, dispCancel := context.WithTimeout(ctx, 30*time.Second)
	defer dispCancel()
	jid, err := saltClient.DispatchHighstate(dispCtx, minionID)
	if err != nil {
		log.Error(err, "Day-2 highstate dispatch failed", "name", vm.GetName(), "minionID", minionID)
		if pErr := r.patchAnnotations(ctx, vm, map[string]string{
			AnnotationSaltStatus: "Failed/HighstateDispatch",
			AnnotationReady:      "false",
		}); pErr != nil {
			return ctrl.Result{}, pErr
		}
		r.Recorder.Event(vm, corev1.EventTypeWarning, "SaltDay2HighstateFailed",
			fmt.Sprintf("Day-2 highstate dispatch failed: %v", err))
		return ctrl.Result{}, nil
	}

	// Record the new intent hash, every trigger marker, and the JID atomically, so the next
	// reconcile enters runPollHighstate with nothing left that could re-trigger this path.
	patch := map[string]string{
		AnnotationSaltStatus:      "HighstateDispatched",
		AnnotationHighstateJID:    jid,
		AnnotationHighstateStatus: "InProgress",
		AnnotationReady:           "false",
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
	log.Info("Day-2 highstate dispatched", "name", vm.GetName(), "minionID", minionID, "jid", jid)
	r.Recorder.Event(vm, corev1.EventTypeNormal, "SaltDay2Dispatched",
		fmt.Sprintf("Day-2 state.highstate dispatched for minion %q (JID: %s)", minionID, jid))
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// computeIntentHash returns a short SHA-256 hex digest over every tenant-declared
// salt.vcf.io/* annotation on the VM: every key with that prefix except the operator's own
// status keys (operatorStatusAnnotations). Used for Day-2 change detection: a mismatch with
// AnnotationRolesHash triggers a refresh_pillar + highstate re-dispatch.
//
// Hashing the full intent set, not just roles, means a tag/*, cis-profile,
// cis-exceptions/*, environment, or vault-path change also triggers Day-2, matching the
// documented vm:tags:db_port override mechanism (which needs a change to take effect after
// initial bootstrap, not only at creation time). The deny-list is checked against, rather
// than an allow-list of known intent keys, so a newly introduced tenant annotation kind is
// covered automatically, with no need to special-case each one here.
//
// Keys are sorted before hashing so the digest is independent of map iteration order. Go
// randomizes map iteration order per process, so an unsorted digest would flap between
// reconciles even with no actual annotation change, causing a spurious Day-2 dispatch loop.
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

// computeLegacyRolesHash reproduces the legacy hash algorithm, which covers only the roles
// annotation with no version prefix. adoptLegacyIntentHash uses it to tell whether a VM
// carrying a legacy-format hash has had a genuine roles change. Not for use in change
// detection generally.
func computeLegacyRolesHash(roles string) string {
	h := sha256.Sum256([]byte(roles))
	return hex.EncodeToString(h[:8])
}

// adoptLegacyIntentHash handles a stored hash in the legacy format (no intentHashVersion
// prefix, roles-only).
//
// A legacy-format hash can never equal a current computeIntentHash digest, so comparing them
// directly would mark every such VM as changed and dispatch all of them at once - an
// unannounced fleet-wide mass action, unacceptable in a change-controlled environment even
// though the underlying convergence is harmless (state runs are idempotent).
//
// Blindly treating a legacy hash as already current would risk silently swallowing a real,
// pending roles change. Instead this recomputes computeLegacyRolesHash against the VM's
// current roles value: a match means nothing the legacy hash could observe has changed, so
// the VM is migrated to the current format in place with no dispatch; a mismatch means roles
// genuinely changed, and the caller dispatches normally, writing the current format as a
// side effect.
//
// Returns true when the caller should stop and requeue (migration written, nothing to do).
func (r *VirtualMachineReconciler) adoptLegacyIntentHash(
	ctx context.Context,
	vm *unstructured.Unstructured,
	storedHash string,
) (bool, error) {
	annotations := vm.GetAnnotations()
	if computeLegacyRolesHash(annotations[AnnotationRoles]) != storedHash {
		return false, nil // real roles change predating the upgrade: let the caller dispatch
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
