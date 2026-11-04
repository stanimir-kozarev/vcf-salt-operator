/*
SPDX-License-Identifier: MIT-0
*/

package v1alpha1

import (
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	KeyAcceptanceDiscovery = "discovery"
	KeyAcceptancePreseed   = "preseed"
)

// SaltKeyConfigSpec defines the desired state of SaltKeyConfig.
// Create one SaltKeyConfig per Supervisor namespace to enroll it for Salt key management.
type SaltKeyConfigSpec struct {
	// raasURL is the base URL of the VCF Salt RaaS API.
	// Example: "https://aria-config.corp:443"
	// Required when credentialsSecret is set. Without credentialsSecret the operator's default
	// connection supplies it, and a value here must equal the default's.
	// +optional
	// +kubebuilder:validation:Pattern=`^https?://`
	RaasURL string `json:"raasURL,omitempty"`

	// credentialsSecret is the name of a Secret in the same namespace that holds
	// the RaaS username and password. The Secret must have keys "username" and "password".
	// Leave it empty to use the operator's default connection, a Secret in the operator's
	// own namespace set at install time, which keeps RaaS credentials out of this namespace.
	// +optional
	// +kubebuilder:validation:MinLength=1
	CredentialsSecret string `json:"credentialsSecret,omitempty"`

	// keyAcceptance selects how a VM's Salt minion key becomes accepted.
	// "discovery" accepts a pending key whose minion ID matches the VM by name or IP.
	// "preseed" generates the key pair on the master through RaaS, accepted before the VM
	// boots, and writes it with the master's public key to the Secret <vm>-salt-bootstrap,
	// which the VM's cloud-init places under /etc/salt. The minion ID is the VM name.
	// A VM whose key is already accepted keeps it when the namespace switches mode.
	// Preseed needs the operator's permission to create, update and patch Secrets.
	// +optional
	// +kubebuilder:validation:Enum=discovery;preseed
	// +kubebuilder:default=discovery
	KeyAcceptance string `json:"keyAcceptance,omitempty"`

	// acceptTimeout is the maximum time, counted from the VM's creation in vSphere (its
	// VirtualMachineCreated condition), to wait for a Salt minion key to appear in the
	// pending list. In preseed it bounds how long the private key stays in the bootstrap
	// Secret: once it passes, the key is cleared, and a VM not yet running with an IP is
	// marked Failed. A VM not yet created in vSphere waits without a limit. Defaults to "10m".
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
	// Required when credentialsSecret is set. Without credentialsSecret the operator's default
	// connection supplies it, and a value here must equal the default's.
	// +optional
	// +kubebuilder:validation:MinLength=1
	MasterID string `json:"masterID,omitempty"`

	// skipTLSVerify disables TLS certificate verification when connecting to the RaaS API.
	// Set to true for environments with self-signed certificates. With the operator's default
	// connection it can only be true if the default connection allows it, and the TLS mode in
	// use is the default connection's, which the Ready condition states.
	// +optional
	SkipTLSVerify bool `json:"skipTLSVerify,omitempty"`

	// vmSelector restricts which VirtualMachine resources in this namespace are managed.
	// Only VMs matching this selector AND having the annotation salt.vcf.io/managed=true
	// will have their Salt keys managed. If omitted, all annotated VMs are managed.
	// A VM that is being deleted always gets its key removed, whatever the selector says,
	// so narrowing the selector never strands a key or a finalizer.
	// +optional
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

	// enforcementInterval re-runs the Salt chain for VirtualMachines in this namespace whose
	// last highstate completed longer ago than this duration, correcting configuration that
	// drifted outside Git between events. Example: "24h".
	//
	// Empty or absent disables scheduled enforcement, which is the default. Enforcement stays
	// purely event-driven until an operator opts in, so adding this field to the API changes
	// no existing deployment's behaviour.
	//
	// Only VMs reporting salt.vcf.io/salt-status=Ready are re-run. A VM sitting in a terminal
	// Failed/<step> state is never picked up here, which preserves the no-automatic-retry rule
	// the controller applies elsewhere: a broken VM stays visibly broken until something
	// triggers it explicitly, rather than looping against a failure nobody has looked at.
	//
	// Due times spread themselves across the estate without any pacing machinery, since each
	// VM's interval is measured from its own last highstate and VMs bootstrap at different
	// moments. A small per-VM jitter is added on top so that VMs onboarded in one batch do not
	// stay in lockstep forever.
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|ms|s|m|h))+$`
	EnforcementInterval string `json:"enforcementInterval,omitempty"`
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
	// The Ready condition is set True when the RaaS connection resolves, from the credentials
	// Secret or the operator's default connection, and all duration fields parse correctly.
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

func (cfg *SaltKeyConfig) IsReady() bool {
	return apimeta.IsStatusConditionTrue(cfg.Status.Conditions, "Ready")
}

func init() {
	SchemeBuilder.Register(&SaltKeyConfig{}, &SaltKeyConfigList{})
}
