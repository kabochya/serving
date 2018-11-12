package autoscaling

import (
	"fmt"

	kpa "github.com/knative/serving/pkg/apis/autoscaling/v1alpha1"
	"github.com/knative/serving/pkg/apis/serving"
	clientset "github.com/knative/serving/pkg/client/clientset/versioned"
	"go.uber.org/zap"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type migrator struct {
	kubeClientSet    kubernetes.Interface
	servingClientSet clientset.Interface

	logger *zap.SugaredLogger
}

// NewMigrator creates a migrator
func NewMigrator(kubeClientSet kubernetes.Interface, servingClientSet clientset.Interface, logger *zap.SugaredLogger) Migrator {

	return &migrator{
		kubeClientSet:    kubeClientSet,
		servingClientSet: servingClientSet,
		logger:           logger,
	}
}

func (m *migrator) Migrate(kpa *kpa.PodAutoscaler, desiredScale int32, currentScale int32) (int32, error) {
	ns := kpa.Namespace
	f := kpa.Labels[serving.FunctionLabelKey]
	dn := kpa.Spec.ScaleTargetRef.Name
	rev := kpa.Labels[serving.RevisionLabelKey]
	deltaScale := desiredScale - currentScale
	emptyGetOpts := metav1.GetOptions{}

	d, err := m.kubeClientSet.ExtensionsV1beta1().Deployments(ns).Get(dn, emptyGetOpts)
	if err != nil {
		m.logger.Errorf("Failed to find corresponding deployment %s for KPA: %v", dn, zap.Error(err))
		return desiredScale, err
	}

	// Step 1: pause target deployment rollout
	dclone := d.DeepCopy()
	dclone.Spec.Paused = true
	dclone.Spec.Template.Labels["tmp"] = "true"
	dclone.Spec.Selector.MatchLabels["tmp"] = "true"
	_, err = m.kubeClientSet.ExtensionsV1beta1().Deployments(ns).Update(dclone)
	if err != nil {
		m.logger.Errorf("Failed to pause deployment %s: %v", dn, zap.Error(err))
		return desiredScale, err
	}

	// Step 2: pick pods in pool to migrate

	poolsel := fmt.Sprintf("%s=%s,pool=true", serving.FunctionLabelKey, f)
	if err != nil {
		m.logger.Errorf("Failed to parse selector: %v", zap.Error(err))
		return desiredScale, err
	}
	poolListOpts := metav1.ListOptions{
		LabelSelector: poolsel,
	}

	poolrsList, err := m.kubeClientSet.ExtensionsV1beta1().ReplicaSets(ns).List(poolListOpts)
	if err != nil {
		m.logger.Errorf("Failed to find corresponding replicaSet for function %s: %v", f, zap.Error(err))
		return desiredScale, err
	} else if l := len(poolrsList.Items); l != 1 {
		m.logger.Warnf("%d replicaSet found for function %s.", l, f)
	}

	poolrs := poolrsList.Items[0]
	readyScale := poolrs.Status.ReadyReplicas
	migrateScale := deltaScale
	if readyScale < migrateScale {
		migrateScale = readyScale
	}

	podsList, err := m.kubeClientSet.Core().Pods(ns).List(poolListOpts)
	if err != nil {
		m.logger.Errorf("Failed to find pods of function %s: %v", f, zap.Error(err))
		return desiredScale, err
	}

	migratePods := make([]*v1.Pod, migrateScale)
	for i := int32(0); i < migrateScale; i++ {
		p := podsList.Items[i].DeepCopy()
		delete(p.Labels, "pool")
		p, err = m.kubeClientSet.Core().Pods(ns).Update(p)
		migratePods = append(migratePods, p)
		if err != nil {
			m.logger.Errorf("Failed to update warm pod in pool %s: %v", f, zap.Error(err))
			return desiredScale, nil
		}
	}

	// Step 3: Change the target rs labels and # of replicas so we can migrate warm pods
	targetrsssel := fmt.Sprintf("%s=%s", serving.RevisionLabelKey, rev)
	if err != nil {
		m.logger.Errorf("Failed to parse selector: %v", zap.Error(err))
		return desiredScale, err
	}
	targetrss, err := m.kubeClientSet.ExtensionsV1beta1().ReplicaSets(ns).List(metav1.ListOptions{LabelSelector: targetrsssel})
	if err != nil {
		m.logger.Errorf("Failed to find target replicaSet of revision %s: %v", rev, zap.Error(err))
		return desiredScale, err
	} else if l := len(targetrss.Items); l != 1 {
		m.logger.Warnf("%d replicaSet found for revision %s.", l, rev)
	}
	targetrs := targetrss.Items[0].DeepCopy()

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

	targetrs, err = m.kubeClientSet.ExtensionsV1beta1().ReplicaSets(ns).Update(targetrs)
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
	_, err = m.kubeClientSet.ExtensionsV1beta1().ReplicaSets(ns).Update(targetrs)

	// Step 6: Resume target deployment rollout
	dclone.Spec.Paused = false
	dclone.Spec.Replicas = &migratedScale
	delete(dclone.Spec.Selector.MatchLabels, "tmp")
	delete(dclone.Spec.Template.Labels, "tmp")
	m.kubeClientSet.ExtensionsV1beta1().Deployments(ns).Update(dclone)

	return desiredScale, nil
}
