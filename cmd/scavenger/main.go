/*
SPDX-License-Identifier: MIT-0
*/

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	saltv1alpha1 "github.com/stanimir-kozarev/vcf-salt-operator/api/v1alpha1"
	"github.com/stanimir-kozarev/vcf-salt-operator/internal/raasconn"
	"github.com/stanimir-kozarev/vcf-salt-operator/pkg/salt"
)

var vmGVK = schema.GroupVersionKind{
	Group:   "vmoperator.vmware.com",
	Version: "v1alpha4",
	Kind:    "VirtualMachine",
}

const annotationMinionID = "salt.vcf.io/minion-id"

const defaultDryRun = true

type saltClientFactory func(
	raasURL, username, password, masterID string,
	skipTLSVerify bool,
	log logr.Logger,
) salt.Client

func main() {
	var dryRun bool
	var defaultConnection raasconn.Default
	var excludeMinions string

	flag.BoolVar(&dryRun, "dry-run", defaultDryRun,
		"Report stale keys without deleting them. Pass --dry-run=false to actually delete")
	flag.StringVar(&defaultConnection.SecretName, raasconn.DefaultConnectionFlag, "",
		"Secret in the scavenger's namespace holding the default RaaS connection, as for the operator")
	flag.StringVar(&defaultConnection.Namespace, "default-connection-namespace", os.Getenv("POD_NAMESPACE"),
		"Namespace of the default connection Secret. Defaults to the POD_NAMESPACE environment variable.")
	flag.StringVar(&excludeMinions, "exclude-minions", "",
		"Comma-separated glob patterns of minion IDs never scavenged, such as the master's own minion and "+
			"any minion that is not a VM, for example saltmaster,infra-*")
	flag.Parse()

	logf.SetLogger(zap.New())
	log := logf.Log.WithName("scavenger")

	exclude := splitList(excludeMinions)
	for _, pattern := range exclude {
		if _, err := path.Match(pattern, ""); err != nil {
			log.Error(err, "Invalid --exclude-minions pattern", "pattern", pattern)
			os.Exit(1)
		}
	}
	log.Info("Excluded minions", "patterns", exclude)

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
	if err := scavenge(ctx, k8sClient, dryRun, exclude, defaultConnection, newSaltClient); err != nil {
		log.Error(err, "Scavenge run failed")
		os.Exit(1)
	}
	log.Info("Scavenge complete")
}

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

type raasGroup struct {
	conn       *raasconn.Connection
	namespaces []string
}

func scavenge(
	ctx context.Context,
	k8sClient client.Client,
	dryRun bool,
	exclude []string,
	defaultConnection raasconn.Default,
	newSaltClient saltClientFactory,
) error {
	log := logf.FromContext(ctx).WithName("scavenge")

	cfgList := &saltv1alpha1.SaltKeyConfigList{}
	if err := k8sClient.List(ctx, cfgList); err != nil {
		return fmt.Errorf("list SaltKeyConfigs: %w", err)
	}

	groups := make(map[string]*raasGroup)
	for i := range cfgList.Items {
		cfg := &cfgList.Items[i]
		if !cfg.IsReady() {
			log.Info("Skipping unready SaltKeyConfig", "namespace", cfg.Namespace, "name", cfg.Name)
			continue
		}
		conn, err := raasconn.Resolve(ctx, k8sClient, cfg, defaultConnection)
		if err != nil {
			log.Error(err, "Skipping SaltKeyConfig whose RaaS connection does not resolve",
				"namespace", cfg.Namespace, "name", cfg.Name)
			continue
		}
		g, ok := groups[conn.URL]
		if !ok {
			g = &raasGroup{conn: conn}
			groups[conn.URL] = g
		}
		g.namespaces = append(g.namespaces, cfg.Namespace)
	}

	for url, g := range groups {
		if err := scavengeRaasGroup(ctx, k8sClient, g, dryRun, exclude, newSaltClient); err != nil {
			log.Error(err, "Failed to scavenge RaaS group", "raasURL", url)
		}
	}
	return nil
}

func scavengeRaasGroup(
	ctx context.Context,
	k8sClient client.Client,
	g *raasGroup,
	dryRun bool,
	exclude []string,
	newSaltClient saltClientFactory,
) error {
	conn := g.conn
	log := logf.FromContext(ctx).WithValues("raasURL", conn.URL, "namespaces", g.namespaces)

	saltClient := newSaltClient(
		conn.URL, conn.Username, conn.Password,
		conn.MasterID, conn.SkipTLSVerify, log.WithName("raas"),
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

	deleted, stale, excluded := 0, 0, 0
	for _, key := range acceptedKeys {
		if liveMinions[key] {
			continue
		}
		if matchesAny(exclude, key) {
			excluded++
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
		"excluded", excluded,
		"staleFound", stale,
		"deleted", deleted,
	)
	return nil
}

func matchesAny(patterns []string, minionID string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, minionID); ok {
			return true
		}
	}
	return false
}

func splitList(v string) []string {
	var out []string
	for item := range strings.SplitSeq(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

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
