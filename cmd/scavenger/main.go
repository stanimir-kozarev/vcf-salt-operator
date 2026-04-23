/*
Copyright (c) 2025 Stan Kozarev.

SPDX-License-Identifier: MIT
*/

// cmd/scavenger is a one-shot job intended to run as a Kubernetes CronJob.
// It reconciles stale accepted Salt minion keys — keys that are still "accepted"
// in VCF Salt (RaaS) but whose VirtualMachine no longer exists in K8s.
//
// This handles the case where the operator missed a VM deletion event
// (e.g., the operator was down when the VM was deleted).
//
// Usage:
//
//	scavenger [--dry-run] [--kubeconfig <path>]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
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

func main() {
	var dryRun bool
	var kubeconfig string

	flag.BoolVar(&dryRun, "dry-run", false, "Log stale keys but do not delete them")
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig (defaults to in-cluster config)")
	flag.Parse()

	logf.SetLogger(zap.New(zap.UseDevMode(true)))
	log := logf.Log.WithName("scavenger")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	k8sClient, err := buildClient(kubeconfig)
	if err != nil {
		log.Error(err, "Could not build Kubernetes client")
		os.Exit(1)
	}

	if dryRun {
		log.Info("Running in dry-run mode — no keys will be deleted")
	}

	if err := scavenge(ctx, k8sClient, dryRun); err != nil {
		log.Error(err, "Scavenge run failed")
		os.Exit(1)
	}
	log.Info("Scavenge complete")
}

// buildClient creates a controller-runtime client.
func buildClient(kubeconfigPath string) (client.Client, error) {
	var restCfg *rest.Config
	var err error

	if kubeconfigPath != "" {
		restCfg, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	} else {
		restCfg, err = ctrlconfig.GetConfig()
	}
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
func scavenge(ctx context.Context, k8sClient client.Client, dryRun bool) error {
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
		if err := scavengeRaasGroup(ctx, k8sClient, g, dryRun); err != nil {
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

	saltClient := salt.NewRaaSClient(
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
			log.Info("Would delete stale key (dry-run)", "minionID", key)
			continue
		}
		if err := saltClient.DeleteKey(ctx, key); err != nil {
			log.Error(err, "Could not delete stale key (continuing)", "minionID", key)
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
