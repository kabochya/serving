package names

import (
	"github.com/knative/serving/pkg/apis/serving/v1alpha1"
)

func Pool(function *v1alpha1.Function) string {
	return function.Name + "-pool"
}

func Deployment(function *v1alpha1.Function) string {
	return Pool(function) + "-deployment"
}
