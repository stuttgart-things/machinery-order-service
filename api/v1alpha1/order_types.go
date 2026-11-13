package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Label and annotation keys an order carries, on the Order CR and on every
// document it writes to git (stuttgart-things/stuttgart-things#4296).
const (
	// OrderIDKey is the order ID: an annotation on every rendered document,
	// and a label on the XR and on the Order CR.
	OrderIDKey = "stuttgart-things.com/order-id"
	// RequestedByKey holds the orderer on the Order CR.
	RequestedByKey = "stuttgart-things.com/requested-by"
	// Finalizer keeps an Order until it is safe to forget it. Deleting an
	// Order never tears anything down.
	Finalizer = "order.machinery.stuttgart-things.com/finalizer"
)

// Phase is where an order stands, across git and the live side.
// +kubebuilder:validation:Enum=Ordered;Placed;PROpen;Merged;Applied;Building;Ready;Cancelled;Failed
type Phase string

const (
	PhaseOrdered   Phase = "Ordered"
	PhasePlaced    Phase = "Placed"
	PhasePROpen    Phase = "PROpen"
	PhaseMerged    Phase = "Merged"
	PhaseApplied   Phase = "Applied"
	PhaseBuilding  Phase = "Building"
	PhaseReady     Phase = "Ready"
	PhaseCancelled Phase = "Cancelled"
	PhaseFailed    Phase = "Failed"
)

// Terminal reports whether nothing moves an order out of the phase any more.
func (p Phase) Terminal() bool {
	return p == PhaseCancelled || p == PhaseFailed
}

// BeforeMerge reports whether the order's files are not on the default
// branch yet, so cancelling it only closes its pull request.
func (p Phase) BeforeMerge() bool {
	return p == "" || p == PhaseOrdered || p == PhasePlaced || p == PhasePROpen
}

// DeliveryMode is how the files reach git.
// +kubebuilder:validation:Enum=pullRequest
type DeliveryMode string

const (
	// DeliveryPullRequest opens a pull request against the base branch.
	DeliveryPullRequest DeliveryMode = "pullRequest"
)

// Placement says where an order goes.
type Placement struct {
	// ManagementCluster is the name of the ManagementCluster that applies the
	// order. The first version knows pinned placement only, so it is required.
	// +kubebuilder:validation:MinLength=1
	ManagementCluster string `json:"managementCluster"`
	// InfraTarget is the infrastructure the order lands on (later, with the
	// scheduler); informational for now.
	// +optional
	InfraTarget string `json:"infraTarget,omitempty"`
}

// OrderSpec is what was ordered. Only Cancel changes after the intake.
//
// +kubebuilder:validation:XValidation:rule="self.template == oldSelf.template && self.placement == oldSelf.placement",message="template and placement are immutable"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.rendered) || (has(self.rendered) && self.rendered == oldSelf.rendered)",message="rendered is immutable once set"
type OrderSpec struct {
	// Template is the claim-machinery-api ClaimTemplate to render.
	// +kubebuilder:validation:MinLength=1
	Template string `json:"template"`
	// Parameters are the template parameters as the orderer gave them, a JSON
	// object. The service adds order_id, management_cluster, source_path and
	// environment_config from the placement.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=object
	Parameters *runtime.RawExtension `json:"parameters,omitempty"`
	// RequestedBy is the orderer (user:default/<name>), taken from the token.
	// +optional
	RequestedBy string `json:"requestedBy,omitempty"`
	// Reason is a free-text note from the orderer, shown in the PR body.
	// +optional
	// +kubebuilder:validation:MaxLength=500
	Reason string `json:"reason,omitempty"`
	// Placement pins the order to a management cluster.
	Placement Placement `json:"placement"`
	// Rendered is the order as the API rendered it at intake, with the
	// orderer's token (so claim-machinery-api bound requested_by/owner to the
	// login). When it is empty, the controller renders with the service's own
	// identity and keeps the result in status.rendered.
	// +optional
	Rendered *Rendered `json:"rendered,omitempty"`
	// Delivery is how the files reach git.
	// +kubebuilder:default=pullRequest
	// +optional
	Delivery DeliveryMode `json:"delivery,omitempty"`
	// Cancel asks to withdraw the order. Before the merge the pull request is
	// closed and the order ends Cancelled; after the merge it is refused
	// (that needs a teardown, which comes later).
	// +optional
	Cancel bool `json:"cancel,omitempty"`
}

// RenderedFile is one file of the rendered order.
type RenderedFile struct {
	// Path is relative to the repository root.
	Path string `json:"path"`
	// Content is the file as written.
	Content string `json:"content"`
}

// Rendered is the order as claim-machinery-api rendered it.
type Rendered struct {
	// Parameters are the parameters sent to claim-machinery-api, a JSON
	// object.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=object
	Parameters *runtime.RawExtension `json:"parameters,omitempty"`
	// Files are the files the order writes, in render order.
	Files []RenderedFile `json:"files"`
	// RenderedAt is when it was rendered.
	RenderedAt metav1.Time `json:"renderedAt"`
}

// ObjectRef names the XR the order creates.
type ObjectRef struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// PullRequest is the pull request that delivers the order.
type PullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	// State is open, closed or merged.
	State string `json:"state"`
}

// DeliveryStatus is the git side of an order.
type DeliveryStatus struct {
	// Repository is owner/name.
	Repository string `json:"repository,omitempty"`
	BaseBranch string `json:"baseBranch,omitempty"`
	Branch     string `json:"branch,omitempty"`
	// CommitSHA is the commit on Branch with the order's files.
	CommitSHA string `json:"commitSHA,omitempty"`
	// +optional
	PullRequest *PullRequest `json:"pullRequest,omitempty"`
	// MergeSHA is the commit on the base branch that brought the files in.
	MergeSHA string `json:"mergeSHA,omitempty"`
	// Kustomizations are the kustomization.yaml files the order appended to.
	// +optional
	Kustomizations []string `json:"kustomizations,omitempty"`
}

// FluxStatus is whether Flux on the management cluster applied the merge.
type FluxStatus struct {
	// LastAppliedRevision of the management cluster's Kustomization, as
	// machinery reports it (e.g. main@sha1:<sha>).
	LastAppliedRevision string `json:"lastAppliedRevision,omitempty"`
	// +optional
	AppliedAt *metav1.Time `json:"appliedAt,omitempty"`
}

// LiveStatus is the XR as machinery sees it.
type LiveStatus struct {
	Found         bool   `json:"found"`
	Ready         bool   `json:"ready"`
	StatusMessage string `json:"statusMessage,omitempty"`
	// Address is machinery's connection details (for a VM, its IP).
	Address string `json:"address,omitempty"`
	// Info are machinery's extra status fields.
	// +optional
	Info map[string]string `json:"info,omitempty"`
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}

// PhaseTransition is one step of the order's timeline.
type PhaseTransition struct {
	Phase   Phase       `json:"phase"`
	Time    metav1.Time `json:"time"`
	Message string      `json:"message,omitempty"`
}

// OrderStatus is where the order stands.
type OrderStatus struct {
	// OrderID is the order ID, the same as the Order's name.
	OrderID string `json:"orderID,omitempty"`
	// +optional
	Phase Phase `json:"phase,omitempty"`
	// Message explains the phase, or the last error.
	// +optional
	Message string `json:"message,omitempty"`
	// Placement is where the order went.
	// +optional
	Placement *Placement `json:"placement,omitempty"`
	// Rendered is set when the controller rendered the order (spec.rendered
	// was empty).
	// +optional
	Rendered *Rendered `json:"rendered,omitempty"`
	// Files are the repository paths of every file the order writes.
	// +optional
	Files []string `json:"files,omitempty"`
	// +optional
	XRRef *ObjectRef `json:"xrRef,omitempty"`
	// +optional
	Delivery *DeliveryStatus `json:"delivery,omitempty"`
	// +optional
	Flux *FluxStatus `json:"flux,omitempty"`
	// +optional
	Live *LiveStatus `json:"live,omitempty"`
	// History is the phase timeline, oldest first.
	// +optional
	History []PhaseTransition `json:"history,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Order is one order of a resource through machinery-order-service. Its name
// is the order ID.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ord
// +kubebuilder:printcolumn:name="Template",type=string,JSONPath=`.spec.template`
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.placement.managementCluster`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="PR",type=string,JSONPath=`.status.delivery.pullRequest.url`,priority=1
// +kubebuilder:printcolumn:name="Requested-By",type=string,JSONPath=`.spec.requestedBy`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Order struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OrderSpec   `json:"spec,omitempty"`
	Status OrderStatus `json:"status,omitempty"`
}

// OrderList is a list of Orders.
//
// +kubebuilder:object:root=true
type OrderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Order `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Order{}, &OrderList{})
}

// EffectiveRendered is the rendered order: the intake's, else the
// controller's.
func (o *Order) EffectiveRendered() *Rendered {
	if o.Spec.Rendered != nil {
		return o.Spec.Rendered
	}
	return o.Status.Rendered
}
