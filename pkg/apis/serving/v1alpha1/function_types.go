/*
Copyright 2018 The Knative Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"github.com/knative/pkg/apis"
	duckv1alpha1 "github.com/knative/pkg/apis/duck/v1alpha1"
	"github.com/knative/pkg/kmeta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Function is an immutable snapshot of code and configuration.  A Function
// references a container image, and optionally a build that is responsible for
// materializing that container image from source. Functions are created by
// updates to a Configuration.
//
// See also: https://github.com/knative/serving/blob/master/docs/spec/overview.md#Function
type Function struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// +optional
	Spec FunctionSpec `json:"spec,omitempty"`
	// +optional
	Status FunctionStatus `json:"status,omitempty"`
}

// Check that Function can be validated, can be defaulted, and has immutable fields.
var _ apis.Validatable = (*Function)(nil)
var _ apis.Defaultable = (*Function)(nil)

// Check that we can create OwnerReferences to a Function.
var _ kmeta.OwnerRefable = (*Function)(nil)

// Check that FunctionStatus may have its conditions managed.
var _ duckv1alpha1.ConditionsAccessor = (*FunctionStatus)(nil)

// FunctionSpec holds the desired state of the Function (from the client).
type FunctionSpec struct {
	// TODO: Generation does not work correctly with CRD. They are scrubbed
	// by the APIserver (https://github.com/kubernetes/kubernetes/issues/58778)
	// So, we add Generation here. Once that gets fixed, remove this and use
	// ObjectMeta.Generation instead.
	// +optional
	Generation int64 `json:"generation,omitempty"`

	// PoolSize describes the desired size of the function pool
	// +optional
	PoolSize int64 `json:"poolSize,omitempty`
}

const (
	// FunctionConditionReady is set when the Function is starting to materialize
	// runtime resources, and becomes true when those resources are ready.
	FunctionConditionReady = duckv1alpha1.ConditionReady
)

var funcCondSet = duckv1alpha1.NewLivingConditionSet()

// FunctionStatus communicates the observed state of the Function (from the controller).
type FunctionStatus struct {
	// ServiceName holds the name of a core Kubernetes Service resource that
	// load balances over the pods backing this Function. When the Function
	// is Active, this service would be an appropriate ingress target for
	// targeting the Function.
	// +optional
	ServiceName string `json:"serviceName,omitempty"`

	// Conditions communicates information about ongoing/complete
	// reconciliation processes that bring the "spec" inline with the observed
	// state of the world.
	// +optional
	Conditions duckv1alpha1.Conditions `json:"conditions,omitempty"`

	// ObservedGeneration is the 'Generation' of the Function that
	// was last processed by the controller. The observed generation is updated
	// even if the controller failed to process the spec and create the Function.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// FunctionList is a list of Function resources
type FunctionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []Function `json:"items"`
}

func (r *Function) GetGroupVersionKind() schema.GroupVersionKind {
	return SchemeGroupVersion.WithKind("Function")
}

// IsReady looks at the conditions to see if they are happy.
func (fs *FunctionStatus) IsReady() bool {
	return funcCondSet.Manage(fs).IsHappy()
}

// GetConditions returns the Conditions array. This enables generic handling of
// conditions by implementing the duckv1alpha1.Conditions interface.
func (fs *FunctionStatus) GetConditions() duckv1alpha1.Conditions {
	return fs.Conditions
}

func (fs *FunctionStatus) InitializeConditions() {
	funcCondSet.Manage(fs).InitializeConditions()
}

// SetConditions sets the Conditions array. This enables generic handling of
// conditions by implementing the duckv1alpha1.Conditions interface.
func (fs *FunctionStatus) SetConditions(conditions duckv1alpha1.Conditions) {
	fs.Conditions = conditions
}
