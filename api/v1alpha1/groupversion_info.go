// Package v1alpha1 holds the API types of machinery-order-service: Order and
// ManagementCluster (stuttgart-things/stuttgart-things#4297).
//
// +kubebuilder:object:generate=true
// +groupName=order.machinery.stuttgart-things.com
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group and version of the Order and
	// ManagementCluster kinds.
	GroupVersion = schema.GroupVersion{Group: "order.machinery.stuttgart-things.com", Version: "v1alpha1"}

	// SchemeBuilder adds the kinds to a scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the kinds of this group to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
