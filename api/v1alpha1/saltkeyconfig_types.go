/*
Copyright (c) 2025 Stan Kozarev.

SPDX-License-Identifier: MIT
*/

package v1alpha1

import (
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// SaltKeyConfigSpec defines the desired state of SaltKeyConfig.
// Create one SaltKeyConfig per Supervisor namespace to enroll it for Salt key management.
type SaltKeyConfigSpec struct {
	// raasURL is the base URL of the VCF Salt RaaS API.
	// Example: "https://aria-config.corp:443"
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https?://`
	RaasURL string `json:"raasURL"`

	// credentialsSecret is the name of a Secret in the same namespace that holds
	// the RaaS username and password. The Secret must have keys "username" and "password".
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	CredentialsSecret string `json:"credentialsSecret"`

	// acceptTimeout is the maximum time to wait for a Salt minion key to appear in the
	// pending list after a VM becomes Running. Defaults to "10m".
	// +optional
	// +kubebuilder:default="10m"
	AcceptTimeout string `json:"acceptTimeout,omitempty"`

	// requeueInterval is how often the controller re-checks for a pending key when one
	// has not been found yet. Defaults to "30s".
	// +optional
	// +kubebuilder:default="30s"
	RequeueInterval string `json:"requeueInterval,omitempty"`

	// masterID is the Salt master identifier as registered in VCF Salt.
	// This value is used when accepting or deleting minion keys via the RaaS API.
	// Example: "saltstack_enterprise_installer"
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	MasterID string `json:"masterID"`

	// skipTLSVerify disables TLS certificate verification when connecting to the RaaS API.
	// Set to true for environments with self-signed certificates.
	// +optional
	// +kubebuilder:default=false
	SkipTLSVerify bool `json:"skipTLSVerify,omitempty"`

	// vmLabelSelector restricts which VirtualMachine resources in this namespace are managed.
	// Only VMs matching this selector AND having the annotation salt.vcf.io/managed=true
	// will have their Salt keys managed. If omitted, all annotated VMs are managed.
	// +optional
	// TODO: VMSelector is defined in the API but not yet enforced by the controller.
	VMSelector *metav1.LabelSelector `json:"vmSelector,omitempty"`

	// retryToken re-runs the Salt chain for every VirtualMachine in this namespace that is
	// currently in a terminal Failed/<step> state. Any change to the value requests one
	// re-run. The value itself is opaque, and a change-request identifier is preferred over
	// a timestamp so the request carries its own audit reference. Each VM records the token
	// it acted on in the salt.vcf.io/bulk-retry-handled annotation, so the operator acts
	// once per distinct value and this field is never cleared by the controller.
	//
	// Healthy VMs are not disturbed: they record the token as handled without re-running,
	// which both keeps "retry this environment" from becoming a fleet-wide convergence and
	// stops a stale token re-arming against a VM that fails later for an unrelated reason.
	//
	// This is the Ops-side lever, for a platform-caused failure that stranded VMs across
	// several tenant repositories. The per-VM equivalent is the salt.vcf.io/retry-request
	// annotation, which a team sets on its own manifest.
	// +optional
	RetryToken string `json:"retryToken,omitempty"`
}

// SaltKeyConfigStatus defines the observed state of SaltKeyConfig.
type SaltKeyConfigStatus struct {
	// observedGeneration is the most recent generation observed by the controller.
	// It is updated every time the controller processes the object, whether or not
	// the spec has changed. Clients can use this to determine whether the controller
	// has processed the latest spec version.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions represent the current state of the SaltKeyConfig resource.
	// The Ready condition is set True when the credentials Secret is found and valid
	// and all duration fields parse correctly.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// SaltKeyConfig is the Schema for the saltkeyconfigs API
type SaltKeyConfig struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired state of SaltKeyConfig
	// +required
	Spec SaltKeyConfigSpec `json:"spec"`

	// status defines the observed state of SaltKeyConfig
	// +optional
	Status SaltKeyConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SaltKeyConfigList contains a list of SaltKeyConfig
type SaltKeyConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SaltKeyConfig `json:"items"`
}

// IsReady reports whether the SaltKeyConfig credentials have been validated
// and the config is ready to be used by the VirtualMachine controller.
func (cfg *SaltKeyConfig) IsReady() bool {
	return apimeta.IsStatusConditionTrue(cfg.Status.Conditions, "Ready")
}

func init() {
	SchemeBuilder.Register(&SaltKeyConfig{}, &SaltKeyConfigList{})
}
