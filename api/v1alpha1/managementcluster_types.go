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
	// ClaimAPIURL is the claim-machinery-api this cluster's orders are
	// rendered with (and their templates read from); empty = the service's
	// CLAIM_API_URL. A staging cluster names its own, so the version under
	// test renders its orders (stuttgart-things/stuttgart-things#4828). The
	// service's own token (CLAIM_API_*) is sent there too, so that API must
	// accept the same audience.
	// +kubebuilder:validation:Pattern=`^https?://`
	// +optional
	ClaimAPIURL string `json:"claimApiUrl,omitempty"`
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
	// RevisionField is the label of the machinery info field holding
	// status.lastAppliedRevision. machinery keys info fields by label; the
	// shared watch config (stuttgart-things/flux cicd/machinery) uses
	// "Revision".
	// +kubebuilder:default=Revision
	// +optional
	RevisionField string `json:"revisionField,omitempty"`
}

// AutoMerge says which orders the service may merge itself once their
// checks pass (docs/adr/0002-auto-merge.md). Only orders that ask for it.
type AutoMerge struct {
	// Method is the merge method.
	// +kubebuilder:validation:Enum=merge;squash;rebase
	// +kubebuilder:default=merge
	// +optional
	Method string `json:"method,omitempty"`
	// Rules: an order that asks for auto-merge must match one of them.
	// +optional
	// +listType=map
	// +listMapKey=name
	Rules []AutoMergeRule `json:"rules,omitempty"`
}

// AutoMergeRule allows auto-merge for some orderers, templates and
// parameter values.
type AutoMergeRule struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Users are orderers (user:default/<name>). A rule without users and
	// groups matches nobody.
	// +optional
	Users []string `json:"users,omitempty"`
	// Groups are the orderer's groups (group:default/<name>).
	// +optional
	Groups []string `json:"groups,omitempty"`
	// Templates the rule covers; empty = any.
	// +optional
	Templates []string `json:"templates,omitempty"`
	// Parameters limit rendered parameter values (defaults included): each
	// listed parameter must have one of the values.
	// +optional
	Parameters map[string][]string `json:"parameters,omitempty"`
	// ParameterPrefixes limit rendered parameter values by prefix: each
	// listed parameter must start with one of the prefixes, e.g.
	// name: [persona-] so test orders can be told apart from real ones.
	// +optional
	ParameterPrefixes map[string][]string `json:"parameterPrefixes,omitempty"`
	// Actions the rule allows: order (the order's own pull request) and
	// teardown (its delete-claim pull request, docs/adr/0003-teardown.md).
	// Empty = [order].
	// +optional
	// +listType=set
	Actions []AutoMergeAction `json:"actions,omitempty"`
}

// AutoMergeAction is what a rule lets the service merge.
// +kubebuilder:validation:Enum=order;teardown
type AutoMergeAction string

const (
	ActionOrder    AutoMergeAction = "order"
	ActionTeardown AutoMergeAction = "teardown"
)

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
	// AutoMerge allows the service to merge some orders' pull requests
	// itself. Unset: every pull request waits for a human.
	// +optional
	AutoMerge *AutoMerge `json:"autoMerge,omitempty"`
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
