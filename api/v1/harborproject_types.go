package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RobotPermission defines a single robot account permission action
type RobotPermission struct {
	// +kubebuilder:validation:Enum=push;pull;scanner-pull
	Action string `json:"action"`
}

// HarborProjectSpec defines the desired state of HarborProject
type HarborProjectSpec struct {
	// Owner is the final owner of this HarborProject, used for metering and billing.
	// Typically a user ID, tenant ID, or namespace UID.
	// +optional
	Owner string `json:"owner,omitempty"`

	// ProjectName is the name of the project in Harbor.
	// If empty, auto-generated as "hp-{name}".
	// +optional
	ProjectName string `json:"projectName,omitempty"`

	// DisplayName is the human-readable display name of the project
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// NamespaceRefs specifies the target Kubernetes namespace names where docker credentials
	// will be distributed.
	// +optional
	NamespaceRefs []string `json:"namespaceRefs,omitempty"`

	// StorageLimit is the storage quota in bytes. -1 means unlimited.
	// +kubebuilder:default:=-1
	StorageLimit int64 `json:"storageLimit,omitempty"`

	// AutoScan enables automatic vulnerability scanning on push
	// +kubebuilder:default:=false
	AutoScan bool `json:"autoScan,omitempty"`

	// Public makes the project publicly pullable without authentication
	// +kubebuilder:default:=false
	Public bool `json:"public,omitempty"`

	// RobotPermissions defines the default list of robot account permissions (push/pull).
	// +kubebuilder:default:={{action: "push"},{action: "pull"}}
	RobotPermissions []RobotPermission `json:"robotPermissions,omitempty"`
}

// HarborProjectPhase defines the phase of HarborProject
type HarborProjectPhase string

const (
	HarborPhasePending  HarborProjectPhase = "Pending"
	HarborPhaseCreating HarborProjectPhase = "Creating"
	HarborPhaseReady    HarborProjectPhase = "Ready"
	HarborPhaseDeleting HarborProjectPhase = "Deleting"
	HarborPhaseFailed   HarborProjectPhase = "Failed"
)

// HarborProjectStatus defines the observed state of HarborProject
type HarborProjectStatus struct {
	Phase              HarborProjectPhase `json:"phase,omitempty"`
	HarborProjectID    int64              `json:"harborProjectID,omitempty"`
	HarborProjectName  string             `json:"harborProjectName,omitempty"`
	RobotName          string             `json:"robotName,omitempty"`
	RobotID            int64              `json:"robotID,omitempty"`
	Owner              string             `json:"owner,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName={hp,hproj}
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="ProjectID",type="integer",JSONPath=".status.harborProjectID"
// +kubebuilder:printcolumn:name="Owner",type="string",JSONPath=".status.owner"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// HarborProject is the Schema for the harborprojects API
type HarborProject struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HarborProjectSpec   `json:"spec,omitempty"`
	Status HarborProjectStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// HarborProjectList contains a list of HarborProject
type HarborProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HarborProject `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HarborProject{}, &HarborProjectList{})
}
