// Package v1alpha1 contains the NetcupFailoverIP API types. It depends only
// on apimachinery so it stays cheap to import.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the API group and version of the types in this package.
	GroupVersion = schema.GroupVersion{Group: "netcup.digilol.net", Version: "v1alpha1"}
	// SchemeBuilder collects the functions that register this package's types.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	// AddToScheme registers this package's types with a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &NetcupFailoverIP{}, &NetcupFailoverIPList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
