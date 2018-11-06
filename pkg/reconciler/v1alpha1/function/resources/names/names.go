package names

import (
	"github.com/knative/serving/pkg/apis/serving/v1alpha1"
)

func Deployment(function *v1alpha1.Function) string {
	return function.Name + "-pool"
}
