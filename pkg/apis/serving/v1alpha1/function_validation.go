/*
Copyright 2018 The Knative Authors

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
	"strconv"

	"github.com/knative/pkg/apis"
	"k8s.io/apimachinery/pkg/api/equality"
)

func (f *Function) Validate() *apis.FieldError {
	return ValidateObjectMetadata(f.GetObjectMeta()).ViaField("metadata").
		Also(f.Spec.Validate().ViaField("spec"))
}

func (fs *FunctionSpec) Validate() *apis.FieldError {
	if equality.Semantic.DeepEqual(fs, &FunctionSpec{}) {
		return apis.ErrMissingField(apis.CurrentField)
	}

	if fs.PoolSize < 0 {
		return apis.ErrInvalidValue(strconv.Itoa(int(fs.PoolSize)), "poolSize")
	}

	return nil
}
