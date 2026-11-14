package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GitOps is where a management cluster's orders live in git.
type GitOps struct {
	// Repository is owner/name on GitHub.
	// +kubebuilder:default="stuttgart-things/stuttgart-things"
	// +optional
	Repository string `json:"repository,omitempty"`
	// BaseBranch is the branch Flux applies.
	// +kubebuilder:default=main
	// +optional
	BaseBranch string `json:"baseBranch,omitempty"`
	// OrdersPath is the directory the XR files go to, e.g.
	// clusters/labda/vsphere/machinery-orders-xrs. Passed to the template as
	// source_path = <OrdersPath>/<name>.yaml; empty leaves the template's
	// default.
	// +optional
	OrdersPath string `json:"ordersPath,omitempty"`
}

// Render is what the management cluster adds to every order's parameters.
type Render struct {
	// ClusterName is passed as management_cluster; empty = the
	// ManagementCluster's name.
	// +optional
	ClusterName string `json:"clusterName,omitempty"`
	// EnvironmentConfig is passed as environment_config; empty leaves the
	// template's default (the lab's).
	// +optional
	EnvironmentConfig string `json:"environmentConfig,omitempty"`
}

// SecretKeyRef points to one key of a Secret.
type SecretKeyRef struct {
	// Namespace defaults to the service's namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	// +kubebuilder:default=token
	// +optional
	Key string `json:"key,omitempty"`
}

// Machinery is how the service reaches machinery on the management cluster
// for the live side. The service has no kubeconfig for the cluster.
type Machinery struct {
	// Endpoint is machinery's gRPC address, host:port.
	// +kubebuilder:validation:MinLength=1
	Endpoint string `json:"endpoint"`
	// Insecure dials without TLS (in-cluster or tests).
	// +optional
	Insecure bool `json:"insecure,omitempty"`
	// TokenSecretRef holds machinery's gRPC bearer token, if it wants one.
	// +optional
	TokenSecretRef *SecretKeyRef `json:"tokenSecretRef,omitempty"`
}

// Flux names the Kustomization that applies the orders directory, as
// machinery watches it: the order is Applied once its lastAppliedRevision
// is at or past the merge SHA.
type Flux struct {
	// Kind is the kind machinery serves the Kustomization under.
	// +kubebuilder:default=Kustomization
	// +optional
	Kind string `json:"kind,omitempty"`
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:default=flux-system
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// RevisionField is the machinery info field holding
	// status.lastAppliedRevision.
	// +kubebuilder:default=lastAppliedRevision
	// +optional
	RevisionField string `json:"revisionField,omitempty"`
}

// ManagementClusterSpec describes a cluster that applies orders: Flux pulls
// its orders directory, Crossplane builds the XRs.
type ManagementClusterSpec struct {
	// Lab is informational (labda, labul).
	// +optional
	Lab string `json:"lab,omitempty"`
	// +optional
	GitOps GitOps `json:"gitops,omitempty"`
	// +optional
	Render Render `json:"render,omitempty"`
	// Machinery is the live side.
	// +optional
	Machinery *Machinery `json:"machinery,omitempty"`
	// Flux is the applied check. Without it an order goes from Merged to
	// Building once machinery sees its XR.
	// +optional
	Flux *Flux `json:"flux,omitempty"`
	// Cordoned refuses new orders.
	// +optional
	Cordoned bool `json:"cordoned,omitempty"`
}

// ManagementClusterStatus is what the service last saw of the cluster.
type ManagementClusterStatus struct {
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ManagementCluster is a cluster orders can be placed on.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=mc
// +kubebuilder:printcolumn:name="Lab",type=string,JSONPath=`.spec.lab`
// +kubebuilder:printcolumn:name="Orders",type=string,JSONPath=`.spec.gitops.ordersPath`
// +kubebuilder:printcolumn:name="Cordoned",type=boolean,JSONPath=`.spec.cordoned`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ManagementCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ManagementClusterSpec   `json:"spec,omitempty"`
	Status ManagementClusterStatus `json:"status,omitempty"`
}

// ManagementClusterList is a list of ManagementClusters.
//
// +kubebuilder:object:root=true
type ManagementClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ManagementCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ManagementCluster{}, &ManagementClusterList{})
}
