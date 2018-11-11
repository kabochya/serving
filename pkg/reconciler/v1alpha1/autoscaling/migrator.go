package autoscaling

import (
	kpa "github.com/knative/serving/pkg/apis/autoscaling/v1alpha1"
	"github.com/knative/serving/pkg/apis/serving"
	clientset "github.com/knative/serving/pkg/client/clientset/versioned"
	servinginformers "github.com/knative/serving/pkg/client/informers/externalversions/serving/v1alpha1"
	servinglisters "github.com/knative/serving/pkg/client/listers/serving/v1alpha1"
	"go.uber.org/zap"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	appsv1informers "k8s.io/client-go/informers/apps/v1"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	appsv1listers "k8s.io/client-go/listers/apps/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

type migrator struct {
	kubeClientSet    kubernetes.Interface
	servingClientSet clientset.Interface
	functionLister   servinglisters.FunctionLister
	deploymentLister appsv1listers.DeploymentLister
	replicaSetLister appsv1listers.ReplicaSetLister
	podLister        corev1listers.PodLister

	logger *zap.SugaredLogger
}

// NewMigrator creates a migrator
func NewMigrator(kubeClientSet kubernetes.Interface, servingClientSet clientset.Interface,
	functionInformer servinginformers.FunctionInformer, podInformer corev1informers.PodInformer,
	replicaSetInformer appsv1informers.ReplicaSetInformer, deploymentInformer appsv1informers.DeploymentInformer,
	logger *zap.SugaredLogger) Migrator {

	return &migrator{
		kubeClientSet:    kubeClientSet,
		servingClientSet: servingClientSet,
		functionLister:   functionInformer.Lister(),
		replicaSetLister: replicaSetInformer.Lister(),
		deploymentLister: deploymentInformer.Lister(),
		logger:           logger,
	}
}

type lsr metav1.LabelSelectorRequirement

func (m *migrator) Migrate(kpa *kpa.PodAutoscaler, desiredScale int32, currentScale int32) (int32, error) {
	ns := kpa.Namespace
	f := kpa.Labels[serving.FunctionLabelKey]
	dn := kpa.Spec.ScaleTargetRef.Name
	rev := kpa.Labels[serving.RevisionLabelKey]
	deltaScale := desiredScale - currentScale

	d, err := m.deploymentLister.Deployments(ns).Get(dn)
	if err != nil {
		m.logger.Errorf("Failed to find corresponding deployment %s for KPA: %v", dn, zap.Error(err))
		return desiredScale, err
	}

	// Step 1: pause target deployment rollout
	dclone := d.DeepCopy()
	dclone.Spec.Paused = true
	dclone.Spec.Template.Labels["tmp"] = "true"
	dclone.Spec.Selector.MatchLabels["tmp"] = "true"
	_, err = m.kubeClientSet.Apps().Deployments(ns).Update(dclone)
	if err != nil {
		m.logger.Errorf("Failed to pause deployment %s: %v", dn, zap.Error(err))
		return desiredScale, err
	}

	// Step 2: pick pods in pool to migrate

	sel, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: map[string]string{
			serving.FunctionLabelKey: f,
			"pool":                   "true",
		},
	})
	if err != nil {
		m.logger.Errorf("Failed to parse selector: %v", zap.Error(err))
		return desiredScale, err
	}

	poolrss, err := m.replicaSetLister.ReplicaSets(ns).List(sel)
	if err != nil {
		m.logger.Errorf("Failed to find corresponding replicaSet for function %s: %v", f, zap.Error(err))
		return desiredScale, err
	} else if len(poolrss) != 1 {
		m.logger.Warnf("%d replicaSet found for function %s.", len(poolrss), f)
	}

	poolrs := poolrss[0]
	readyScale := poolrs.Status.ReadyReplicas
	migrateScale := deltaScale
	if readyScale < migrateScale {
		migrateScale = readyScale
	}

	pods, err := m.podLister.Pods(ns).List(sel)
	if err != nil {
		m.logger.Errorf("Failed to find pods of function %s: %v", f, zap.Error(err))
		return desiredScale, err
	}

	migratePods := make([]*v1.Pod, migrateScale)
	for i := int32(0); i < migrateScale; i++ {
		p := pods[i].DeepCopy()
		delete(p.Labels, "pool")
		p, err = m.kubeClientSet.Core().Pods(ns).Update(p)
		migratePods = append(migratePods, p)
		if err != nil {
			m.logger.Errorf("Failed to update warm pod in pool %s: %v", f, zap.Error(err))
			return desiredScale, nil
		}
	}

	// Step 3: Change the target rs labels and # of replicas so we can migrate warm pods
	targetrssel, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: map[string]string{
			serving.RevisionLabelKey: rev,
		},
	})
	if err != nil {
		m.logger.Errorf("Failed to parse selector: %v", zap.Error(err))
		return desiredScale, err
	}
	targetrss, err := m.replicaSetLister.ReplicaSets(ns).List(targetrssel)
	if err != nil {
		m.logger.Errorf("Failed to find target replicaSet of revision %s: %v", rev, zap.Error(err))
		return desiredScale, err
	} else if len(targetrss) != 1 {
		m.logger.Warnf("%d replicaSet found for revision %s.", len(targetrss), rev)
	}
	targetrs := targetrss[0].DeepCopy()

	targetsel := &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			metav1.LabelSelectorRequirement{
				Key:      serving.FunctionLabelKey,
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{f},
			},
			metav1.LabelSelectorRequirement{
				Key:      "pool",
				Operator: metav1.LabelSelectorOpNotIn,
				Values:   []string{"true"},
			},
		},
	}
	migratedScale := currentScale + deltaScale

	targetrs.Spec.Replicas = &migratedScale
	targetrs.Spec.Selector = targetsel
	targetrs.Spec.Template.Labels = map[string]string{
		serving.FunctionLabelKey: f,
	}

	targetrs, err = m.kubeClientSet.Apps().ReplicaSets(ns).Update(targetrs)
	if err != nil {
		m.logger.Errorf("Failed to update target rs %s: %v", targetrs.Name, zap.Error(err))
		return desiredScale, nil
	}
	// Step 4: Add labels to migrated warm pods so we can restore the target rs to original labels
	for _, p := range migratePods {
		p.Labels = targetrs.Labels
		p, err = m.kubeClientSet.Core().Pods(ns).Update(p)
		if err != nil {
			m.logger.Errorf("Failed to relabel pod %s: %v", p.Name, zap.Error(err))
			return desiredScale, nil
		}
		// TODO: change pod stat vars
	}
	// Step 5: Change the target rs labels back to original
	targetrs.Spec.Selector = &metav1.LabelSelector{
		MatchLabels: targetrs.Labels,
	}
	targetrs.Spec.Template.Labels = targetrs.Labels
	_, err = m.kubeClientSet.Apps().ReplicaSets(ns).Update(targetrs)

	// Step 6: Resume target deployment rollout
	dclone.Spec.Paused = false
	dclone.Spec.Replicas = &migratedScale
	delete(dclone.Spec.Selector.MatchLabels, "tmp")
	delete(dclone.Spec.Template.Labels, "tmp")
	m.kubeClientSet.Apps().Deployments(ns).Update(dclone)

	return desiredScale, nil
}
