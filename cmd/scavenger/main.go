/*
Copyright (c) 2025 Stan Kozarev.

SPDX-License-Identifier: MIT
*/

// cmd/scavenger is a one-shot job intended to run as a Kubernetes CronJob. It reconciles
// stale accepted Salt minion keys - keys still "accepted" in VCF Salt (RaaS) but whose
// VirtualMachine no longer carries the matching salt.vcf.io/minion-id annotation in K8s,
// either because the operator missed a VM deletion event, or because a VM's CR was
// recreated and lost the annotation while its key was already accepted. The second case is
// a false positive, not a confirmed orphan, which is why deletion defaults off.
//
// The default is report-only: deletion requires an explicit --dry-run=false. A key with no
// correlated VM is logged at error level in both modes, so log-based alerting can catch it
// before anything is removed.
//
// Usage:
//
//	scavenger [--dry-run=false] [--kubeconfig <path>]
//
// --kubeconfig is registered by an imported controller-runtime package, not here.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	saltv1alpha1 "github.com/stanimir-kozarev/vcf-salt-operator/api/v1alpha1"
	"github.com/stanimir-kozarev/vcf-salt-operator/pkg/salt"
)

var vmGVK = schema.GroupVersionKind{
	Group:   "vmoperator.vmware.com",
	Version: "v1alpha4",
	Kind:    "VirtualMachine",
}

const annotationMinionID = "salt.vcf.io/minion-id"

// defaultDryRun is report-only, since the only correlation signal this job trusts is an
// annotation a recreated VM CR can legitimately lack while still being live.
const defaultDryRun = true

// saltClientFactory builds the RaaS client, a parameter rather than a direct
// salt.NewRaaSClient call so tests can substitute a fake client.
type saltClientFactory func(
	raasURL, username, password, masterID string,
	skipTLSVerify bool,
	log logr.Logger,
) salt.Client

func main() {
	var dryRun bool

	flag.BoolVar(&dryRun, "dry-run", defaultDryRun,
		"Report stale keys without deleting them. Pass --dry-run=false to actually delete")
	// --kubeconfig itself is registered by sigs.k8s.io/controller-runtime/pkg/client/config's
	// own init(), not here. Registering it a second time panics with "flag redefined".
	flag.Parse()

	logf.SetLogger(zap.New(zap.UseDevMode(true)))
	log := logf.Log.WithName("scavenger")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	k8sClient, err := buildClient()
	if err != nil {
		log.Error(err, "Could not build Kubernetes client")
		os.Exit(1)
	}

	if dryRun {
		log.Info("Running in dry-run mode, no keys will be deleted")
	}

	newSaltClient := func(raasURL, username, password, masterID string, skipTLS bool, log logr.Logger) salt.Client {
		return salt.NewRaaSClient(raasURL, username, password, masterID, skipTLS, log)
	}
	if err := scavenge(ctx, k8sClient, dryRun, newSaltClient); err != nil {
		log.Error(err, "Scavenge run failed")
		os.Exit(1)
	}
	log.Info("Scavenge complete")
}

// buildClient creates a controller-runtime client. ctrlconfig.GetConfig() already
// honors --kubeconfig, the KUBECONFIG environment variable, in-cluster config, and
// $HOME/.kube/config, in that order, so no separate flag handling is needed here.
func buildClient() (client.Client, error) {
	restCfg, err := ctrlconfig.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("build rest config: %w", err)
	}

	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := saltv1alpha1.AddToScheme(s); err != nil {
		return nil, err
	}

	return client.New(restCfg, client.Options{Scheme: s})
}

// raasGroup holds all the ready SaltKeyConfigs that share a single RaaS endpoint.
type raasGroup struct {
	representative *saltv1alpha1.SaltKeyConfig
	namespaces     []string
}

// scavenge iterates over all ready SaltKeyConfigs, groups them by RaaS URL so
// that live-VM lookups cover every managed namespace on that RaaS, and then
// deletes accepted keys that have no corresponding VM in any of those namespaces.
func scavenge(ctx context.Context, k8sClient client.Client, dryRun bool, newSaltClient saltClientFactory) error {
	log := logf.FromContext(ctx).WithName("scavenge")

	cfgList := &saltv1alpha1.SaltKeyConfigList{}
	if err := k8sClient.List(ctx, cfgList); err != nil {
		return fmt.Errorf("list SaltKeyConfigs: %w", err)
	}

	// Group ready configs by RaaS URL so we query each RaaS server only once
	// and avoid incorrectly deleting keys that belong to sibling namespaces.
	groups := make(map[string]*raasGroup)
	for i := range cfgList.Items {
		cfg := &cfgList.Items[i]
		if !cfg.IsReady() {
			log.Info("Skipping unready SaltKeyConfig", "namespace", cfg.Namespace, "name", cfg.Name)
			continue
		}
		g, ok := groups[cfg.Spec.RaasURL]
		if !ok {
			g = &raasGroup{representative: cfg}
			groups[cfg.Spec.RaasURL] = g
		}
		g.namespaces = append(g.namespaces, cfg.Namespace)
	}

	for url, g := range groups {
		if err := scavengeRaasGroup(ctx, k8sClient, g, dryRun, newSaltClient); err != nil {
			log.Error(err, "Failed to scavenge RaaS group", "raasURL", url)
		}
	}
	return nil
}

// scavengeRaasGroup handles all namespaces that share a single RaaS endpoint.
// It builds a union of live minion IDs from every managed namespace so that keys
// for VMs in sibling namespaces are never incorrectly deleted.
func scavengeRaasGroup(
	ctx context.Context,
	k8sClient client.Client,
	g *raasGroup,
	dryRun bool,
	newSaltClient saltClientFactory,
) error {
	cfg := g.representative
	log := logf.FromContext(ctx).WithValues("raasURL", cfg.Spec.RaasURL, "namespaces", g.namespaces)

	// Read credentials from the representative config
	secret := &corev1.Secret{}
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Name:      cfg.Spec.CredentialsSecret,
		Namespace: cfg.Namespace,
	}, secret); err != nil {
		return fmt.Errorf("get credentials secret: %w", err)
	}

	username := string(secret.Data["username"])
	password := string(secret.Data["password"])

	saltClient := newSaltClient(
		cfg.Spec.RaasURL, username, password,
		cfg.Spec.MasterID, cfg.Spec.SkipTLSVerify, log.WithName("raas"),
	)

	loginCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := saltClient.Login(loginCtx); err != nil {
		return fmt.Errorf("login to RaaS: %w", err)
	}

	acceptedKeys, err := saltClient.ListAcceptedKeys(ctx)
	if err != nil {
		return fmt.Errorf("list accepted keys: %w", err)
	}
	if len(acceptedKeys) == 0 {
		log.Info("No accepted keys found in RaaS")
		return nil
	}

	// Collect live minion IDs from ALL managed namespaces for this RaaS.
	// Using the union prevents deleting keys that belong to sibling namespaces.
	liveMinions := make(map[string]bool)
	for _, ns := range g.namespaces {
		nsMinions, err := liveVMMinionIDs(ctx, k8sClient, ns)
		if err != nil {
			log.Error(err, "Could not list live VMs — skipping this RaaS group", "namespace", ns)
			return nil
		}
		for id := range nsMinions {
			liveMinions[id] = true
		}
	}

	// Delete accepted keys with no corresponding VM in any managed namespace
	deleted, stale := 0, 0
	for _, key := range acceptedKeys {
		if liveMinions[key] {
			continue
		}
		stale++
		if dryRun {
			log.Error(nil, "Accepted key has no correlated VM, not deleting in dry-run mode", "minionID", key)
			continue
		}
		log.Error(nil, "Accepted key has no correlated VM, deleting", "minionID", key)
		if err := saltClient.DeleteKey(ctx, key); err != nil {
			log.Error(err, "Could not delete stale key, continuing", "minionID", key)
		} else {
			log.Info("Deleted stale Salt key", "minionID", key)
			deleted++
		}
	}

	log.Info("RaaS group scavenge complete",
		"acceptedKeys", len(acceptedKeys),
		"liveVMs", len(liveMinions),
		"staleFound", stale,
		"deleted", deleted,
	)
	return nil
}

// liveVMMinionIDs returns the set of salt.vcf.io/minion-id annotation values
// for all VirtualMachines in the given namespace that have that annotation set.
func liveVMMinionIDs(ctx context.Context, k8sClient client.Client, namespace string) (map[string]bool, error) {
	vmList := &unstructured.UnstructuredList{}
	vmList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   vmGVK.Group,
		Version: vmGVK.Version,
		Kind:    vmGVK.Kind + "List",
	})
	if err := k8sClient.List(ctx, vmList, client.InNamespace(namespace)); err != nil {
		return nil, err
	}

	result := make(map[string]bool, len(vmList.Items))
	for _, vm := range vmList.Items {
		if id := vm.GetAnnotations()[annotationMinionID]; id != "" {
			result[id] = true
		}
	}
	return result, nil
}
