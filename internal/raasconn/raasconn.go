/*
SPDX-License-Identifier: MIT-0
*/

package raasconn

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	saltv1alpha1 "github.com/stanimir-kozarev/vcf-salt-operator/api/v1alpha1"
)

const (
	KeyURL           = "url"
	KeyMasterID      = "masterID"
	KeyUsername      = "username"
	KeyPassword      = "password"
	KeySkipTLSVerify = "skipTLSVerify"
)

const (
	ReasonSecretNotFound     = "SecretNotFound"
	ReasonSecretInvalid      = "SecretInvalid"
	ReasonGetSecretFailed    = "GetSecretFailed"
	ReasonIncompleteSpec     = "IncompleteSpec"
	ReasonDefaultNotSet      = "DefaultConnectionNotConfigured"
	ReasonConnectionMismatch = "ConnectionMismatch"
)

const (
	SourceNamespaceSecret   = "namespace"
	SourceDefaultConnection = "default"
)

const DefaultConnectionFlag = "default-connection-secret"

type Default struct {
	Namespace  string
	SecretName string
}

func (d Default) Configured() bool {
	return d.Namespace != "" && d.SecretName != ""
}

type Connection struct {
	URL           string
	MasterID      string
	Username      string
	Password      string
	SkipTLSVerify bool
	Source        string
}

type Error struct {
	Reason  string
	Message string
}

func (e *Error) Error() string { return e.Reason + ": " + e.Message }

func Resolve(ctx context.Context, r client.Reader, cfg *saltv1alpha1.SaltKeyConfig, def Default) (*Connection, error) {
	if cfg.Spec.CredentialsSecret != "" {
		return resolveNamespaceSecret(ctx, r, cfg)
	}
	return resolveDefault(ctx, r, cfg, def)
}

func resolveNamespaceSecret(ctx context.Context, r client.Reader, cfg *saltv1alpha1.SaltKeyConfig) (*Connection, error) {
	if cfg.Spec.RaasURL == "" || cfg.Spec.MasterID == "" {
		return nil, &Error{ReasonIncompleteSpec,
			"spec.raasURL and spec.masterID are required when spec.credentialsSecret is set"}
	}
	data, err := readSecret(ctx, r, cfg.Namespace, cfg.Spec.CredentialsSecret)
	if err != nil {
		return nil, err
	}
	if len(data[KeyUsername]) == 0 || len(data[KeyPassword]) == 0 {
		return nil, &Error{ReasonSecretInvalid, fmt.Sprintf(
			"Secret %q must contain non-empty keys \"username\" and \"password\"", cfg.Spec.CredentialsSecret)}
	}
	return &Connection{
		URL:           cfg.Spec.RaasURL,
		MasterID:      cfg.Spec.MasterID,
		Username:      string(data[KeyUsername]),
		Password:      string(data[KeyPassword]),
		SkipTLSVerify: cfg.Spec.SkipTLSVerify,
		Source:        SourceNamespaceSecret,
	}, nil
}

func resolveDefault(
	ctx context.Context,
	r client.Reader,
	cfg *saltv1alpha1.SaltKeyConfig,
	def Default,
) (*Connection, error) {
	if !def.Configured() {
		return nil, &Error{ReasonDefaultNotSet, fmt.Sprintf(
			"spec.credentialsSecret is empty and the operator has no default connection (--%s)",
			DefaultConnectionFlag)}
	}
	conn, err := def.Load(ctx, r)
	if err != nil {
		return nil, err
	}

	if cfg.Spec.RaasURL != "" && cfg.Spec.RaasURL != conn.URL {
		return nil, &Error{ReasonConnectionMismatch, fmt.Sprintf(
			"spec.raasURL %q differs from the operator's default connection, set spec.credentialsSecret to use another endpoint",
			cfg.Spec.RaasURL)}
	}
	if cfg.Spec.MasterID != "" && cfg.Spec.MasterID != conn.MasterID {
		return nil, &Error{ReasonConnectionMismatch, fmt.Sprintf(
			"spec.masterID %q differs from the operator's default connection", cfg.Spec.MasterID)}
	}
	if cfg.Spec.SkipTLSVerify && !conn.SkipTLSVerify {
		return nil, &Error{ReasonConnectionMismatch,
			"spec.skipTLSVerify cannot disable TLS verification for the operator's default connection"}
	}
	return conn, nil
}

func (d Default) Load(ctx context.Context, r client.Reader) (*Connection, error) {
	data, err := readSecret(ctx, r, d.Namespace, d.SecretName)
	if err != nil {
		return nil, err
	}
	for _, k := range []string{KeyURL, KeyMasterID, KeyUsername, KeyPassword} {
		if len(data[k]) == 0 {
			return nil, &Error{ReasonSecretInvalid, fmt.Sprintf(
				"default connection Secret %s/%s must contain non-empty keys %q, %q, %q and %q",
				d.Namespace, d.SecretName, KeyURL, KeyMasterID, KeyUsername, KeyPassword)}
		}
	}
	return &Connection{
		URL:           string(data[KeyURL]),
		MasterID:      string(data[KeyMasterID]),
		Username:      string(data[KeyUsername]),
		Password:      string(data[KeyPassword]),
		SkipTLSVerify: strings.EqualFold(string(data[KeySkipTLSVerify]), "true"),
		Source:        SourceDefaultConnection,
	}, nil
}

func readSecret(ctx context.Context, r client.Reader, namespace, name string) (map[string][]byte, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &Error{ReasonSecretNotFound, fmt.Sprintf("Secret %q not found in namespace %q", name, namespace)}
		}
		return nil, &Error{ReasonGetSecretFailed, fmt.Sprintf("Could not read Secret %s/%s: %v", namespace, name, err)}
	}
	return secret.Data, nil
}
