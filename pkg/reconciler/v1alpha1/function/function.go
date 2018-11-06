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

package function

import (
	"context"
	"fmt"
	"reflect"

	"github.com/google/go-cmp/cmp"
	"github.com/knative/pkg/controller"
	"github.com/knative/pkg/logging"
	"github.com/knative/serving/pkg/apis/serving"
	"github.com/knative/serving/pkg/apis/serving/v1alpha1"
	servinginformers "github.com/knative/serving/pkg/client/informers/externalversions/serving/v1alpha1"
	listers "github.com/knative/serving/pkg/client/listers/serving/v1alpha1"
	"github.com/knative/serving/pkg/reconciler"
	resourcenames "github.com/knative/serving/pkg/reconciler/v1alpha1/function/resources/names"
	"github.com/knative/serving/pkg/reconciler/v1alpha1/revision/config"
	revresources "github.com/knative/serving/pkg/reconciler/v1alpha1/revision/resources"
	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	appsv1informers "k8s.io/client-go/informers/apps/v1"
	appsv1listers "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/tools/cache"
)

const controllerAgentName = "function-controller"

// Reconciler implements controller.Reconciler for Service resources.
type Reconciler struct {
	*reconciler.Base

	// listers index properties about resources
	functionLister   listers.FunctionLister
	revisionLister   listers.RevisionLister
	deploymentLister appsv1listers.DeploymentLister
	configMapLister  corev1listers.ConfigMapLister

	configStore configStore
}

// Check that our Reconciler implements controller.Reconciler
var _ controller.Reconciler = (*Reconciler)(nil)

// NewController initializes the controller and is called by the generated code
// Registers eventhandlers to enqueue events
func NewController(
	opt reconciler.Options,
	functionInformer servinginformers.FunctionInformer,
	revisionInformer servinginformers.RevisionInformer,
	deploymentInformer appsv1informers.DeploymentInformer,
) *controller.Impl {

	c := &Reconciler{
		Base:             reconciler.NewBase(opt, controllerAgentName),
		functionLister:   functionInformer.Lister(),
		revisionLister:   revisionInformer.Lister(),
		deploymentLister: deploymentInformer.Lister(),
		configMapLister:  configMapInformer.Lister(),
	}
	impl := controller.NewImpl(c, c.Logger, "Functions", reconciler.MustNewStatsReporter("Functions", c.Logger))

	c.Logger.Info("Setting up event handlers")
	functionInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    impl.Enqueue,
		UpdateFunc: controller.PassNew(impl.Enqueue),
		DeleteFunc: impl.Enqueue,
	})

	deploymentInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: controller.Filter(v1alpha1.SchemeGroupVersion.WithKind("Function")),
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    impl.EnqueueControllerOf,
			UpdateFunc: controller.PassNew(impl.EnqueueControllerOf),
			DeleteFunc: impl.EnqueueControllerOf,
		},
	})

	configMapInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: controller.Filter(v1alpha1.SchemeGroupVersion.WithKind("Function")),
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    impl.EnqueueControllerOf,
			UpdateFunc: controller.PassNew(impl.EnqueueControllerOf),
			DeleteFunc: impl.EnqueueControllerOf,
		},
	})

	c.configStore = config.NewStore(c.Logger.Named("config-store"))
	c.configStore.WatchConfigs(opt.ConfigMapWatcher)

	return impl
}

// Reconcile compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the Service resource
// with the current status of the resource.
func (c *Reconciler) Reconcile(ctx context.Context, key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		c.Logger.Errorf("invalid resource key: %s", key)
		return nil
	}
	logger := logging.FromContext(ctx)

	ctx = c.configStore.ToContext(ctx)

	// Get the Function resource with this namespace/name
	original, err := c.functionLister.Functions(namespace).Get(name)
	if apierrs.IsNotFound(err) {
		// The resource may no longer exist, in which case we stop processing.
		logger.Errorf("function %q in work queue no longer exists", key)
		return nil
	} else if err != nil {
		return err
	}

	// Don't modify the informers copy
	function := original.DeepCopy()

	err = c.reconcile(ctx, function)
	logger.Infof("Updating Status (-old, +new): %v", cmp.Diff(original, function))
	if equality.Semantic.DeepEqual(original.Status, function.Status) {
		// If we didn't change anything then don't call updateStatus.
		// This is important because the copy we loaded from the informer's
		// cache may be stale and we don't want to overwrite a prior update
		// to status with this stale state.
	} else if _, err := c.updateStatus(function); err != nil {
		logger.Warn("Failed to update function status", zap.Error(err))
		return err
	}
	return err
}

func (c *Reconciler) reconcile(ctx context.Context, function *v1alpha1.Function) error {
	logger := logging.FromContext(ctx)
	function.Status.InitializeConditions()

	deploymentName := resourcenames.Deployment(function)
	deployment, getDepErr := c.deploymentLister.Deployments(function.Namespace).Get(deploymentName)
	if apierrs.IsNotFound(getDepErr) {
		var err error
		deployment, err = c.createDeployment(ctx, function)
		if err != nil {
			logger.Errorf("Error creating deployment %q: %v", deploymentName, err)
			return err
		}
		logger.Infof("Created deployment %q", deploymentName)
	} else if getDepErr != nil {
		logger.Errorf("Error reconciling deployment %q: %v", deploymentName, getDepErr)
		return getDepErr
	}

	// TODO: same as revision/reconcile_resources.go:L62

	if hasDeploymentTimedOut(deployment) {
		function.Status.MarkProgressDeadlineExceeded(fmt.Sprintf(
			"Unable to create pods for more than %d seconds.", revresources.ProgressDeadlineSeconds))
		c.Recorder.Eventf(function, corev1.EventTypeNormal, "ProgressDeadlineExceeded",
			"Function %s not ready due to Deployment timeout", function.Name)
	}

	function.Status.MarkReady()
	function.Status.ObservedGeneration = function.Spec.Generation

	return nil
}

func (c *Reconciler) updateStatus(desired *v1alpha1.Function) (*v1alpha1.Function, error) {
	function, err := c.functionLister.Functions(desired.Namespace).Get(desired.Name)
	if err != nil {
		return nil, err
	}
	// Check if there is anything to update.
	if !reflect.DeepEqual(function.Status, desired.Status) {
		// Don't modify the informers copy
		existing := function.DeepCopy()
		existing.Status = desired.Status
		functionClient := c.ServingClientSet.ServingV1alpha1().Functions(desired.Namespace)
		// TODO: for CRD there's no updatestatus, so use normal update.
		return functionClient.Update(desired)
	}
	return function, nil
}

func hasDeploymentTimedOut(deployment *appsv1.Deployment) bool {
	// as per https://kubernetes.io/docs/concepts/workloads/controllers/deployment
	for _, cond := range deployment.Status.Conditions {
		// Look for Deployment with status False
		if cond.Status != corev1.ConditionFalse {
			continue
		}
		// with Type Progressing and Reason Timeout
		// TODO(arvtiwar): hard coding "ProgressDeadlineExceeded" to avoid import kubernetes/kubernetes
		if cond.Type == appsv1.DeploymentProgressing && cond.Reason == "ProgressDeadlineExceeded" {
			return true
		}
	}
	return false
}

func (c *Reconciler) createDeployment(ctx context.Context, function *v1alpha1.Function) (*appsv1.Deployment, error) {
	cfgs := config.FromContext(ctx)

	// reuse revision makeDeployment for now
	poolRev := &v1alpha1.Revision{
		ObjectMeta: metav1.ObjectMeta{
			Name:        resourcenames.Deployment(function),
			Namespace:   function.Namespace,
			Labels:      function.ObjectMeta.Labels,
			Annotations: function.ObjectMeta.Annotations,
		},
		Spec: v1alpha1.RevisionSpec{
			Container: function.Spec.Container,
		},
	}

	deployment := revresources.MakeDeployment(
		poolRev,
		cfgs.Logging,
		cfgs.Network,
		cfgs.Observability,
		cfgs.Autoscaler,
		cfgs.Controller,
	)

	// remove revision-related labels
	delete(deployment.Labels, serving.RevisionLabelKey)
	delete(deployment.Labels, serving.RevisionUID)

	// add function-related labels
	deployment.Labels[serving.FunctionLabelKey] = function.Name

	podSpecLabels := make(map[string]string, len(deployment.Labels)+2)
	for k, v := range deployment.Labels {
		podSpecLabels[k] = v
	}
	podSpecLabels["pool"] = "true"

	poolSize := int32(function.Spec.PoolSize)
	deployment.Spec.Replicas = &poolSize

	deployment.ObjectMeta.OwnerReferences = nil
	deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: deployment.Labels}
	deployment.Spec.Template.ObjectMeta.Labels = podSpecLabels
	deployment.Spec.Template.Spec.ServiceAccountName = function.Spec.ServiceAccountName
	// modify queue envs
	env := deployment.Spec.Template.Spec.Containers[1].Env
	for i, k := range env {
		if k.Name == "SERVING_NAMESPACE" {
			env[i].Value = function.Namespace
		} else if k.Name == "SERVING_REVISION" {
			env[i].Value = function.Name
		}
	}

	// not taking care of fluentd stuff now

	return c.KubeClientSet.AppsV1().Deployments(deployment.Namespace).Create(deployment)
}
