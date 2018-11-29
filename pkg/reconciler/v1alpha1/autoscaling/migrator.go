package autoscaling

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	v1alpha1 "github.com/knative/serving/pkg/apis/autoscaling/v1alpha1"
	"github.com/knative/serving/pkg/apis/serving"
	clientset "github.com/knative/serving/pkg/client/clientset/versioned"
	"github.com/knative/serving/pkg/queue"
	"go.uber.org/zap"
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

var (
	emptyGetOpts = metav1.GetOptions{}
)

func (m *migrator) Migrate(kpa *v1alpha1.PodAutoscaler, desiredScale int32, currentScale int32) (int32, error) {
	if err := m.migrate(kpa, desiredScale, currentScale); err != nil {
		if _, err = m.updateStatus(kpa); err != nil {
			m.logger.Errorf("Failed updating migration status:", zap.Error(err))
		}
		return 0, err
	}
	return desiredScale, nil
}

func (m *migrator) migrate(kpa *v1alpha1.PodAutoscaler, desiredScale int32, currentScale int32) error {
	ns := kpa.Namespace
	f := kpa.Labels[serving.FunctionLabelKey]
	dn := kpa.Spec.ScaleTargetRef.Name
	rev := kpa.Labels[serving.RevisionLabelKey]
	deltaScale := desiredScale - currentScale
	zero := int32(0)

	d, err := m.kubeClientSet.ExtensionsV1beta1().Deployments(ns).Get(dn, emptyGetOpts)
	if err != nil {
		m.logger.Errorf("Failed to find corresponding deployment %s for KPA: %v", dn, zap.Error(err))
		return err
	}
	dclone := d.DeepCopy()

	if *dclone.Spec.Replicas != currentScale {
		return errors.New("Mismatch in scale")
	}

	if deltaScale == 0 {
		return nil
	} else if deltaScale < 0 {

		dclone.Spec.Replicas = &desiredScale
		_, err = m.kubeClientSet.ExtensionsV1beta1().Deployments(ns).Update(dclone)
		if err != nil {
			m.logger.Errorf("Failed to update deployment %s: %v", dclone.Name, zap.Error(err))
			return err
		}
		return nil
	}
	t := time.Now()
	m.logger.Info("KPA migrating")
	// Step 1: pause target deployment rollout
	dclone.Spec.Paused = true
	dclone.Spec.Replicas = &zero
	dclone.Spec.Template.Labels["tmp"] = "true"
	dclone.Spec.Selector.MatchLabels["tmp"] = "true"
	_, err = m.kubeClientSet.ExtensionsV1beta1().Deployments(ns).Update(dclone)
	if err != nil {
		m.logger.Errorf("Failed to pause deployment %s: %v", dn, zap.Error(err))
		return err
	}

	// Step 2: pick pods in pool to migrate

	poolsel := fmt.Sprintf("%s=%s,pool=true", serving.FunctionLabelKey, f)
	if err != nil {
		m.logger.Errorf("Failed to parse selector: %v", zap.Error(err))
		return err
	}
	poolListOpts := metav1.ListOptions{
		LabelSelector: poolsel,
	}

	podsList, err := m.kubeClientSet.Core().Pods(ns).List(poolListOpts)
	if err != nil {
		m.logger.Errorf("Failed to find pods of function %s: %v", f, zap.Error(err))
		return err
	}

	migratedScale := int32(0)
	migratedPodNames := make(map[string]bool)
MigratePod:
	for _, i := range podsList.Items {
		p := i.DeepCopy()

		for _, c := range p.Status.Conditions {
			if c.Status != "True" {
				continue MigratePod
			}
		}
		delete(p.Labels, "pool")
		p, err = m.kubeClientSet.Core().Pods(ns).Update(p)
		migratedScale++
		migratedPodNames[p.Name] = true
		if err != nil {
			m.logger.Errorf("Failed to update warm pod in pool %s: %v", f, zap.Error(err))
			return err
		}
		if migratedScale == deltaScale {
			break
		}
	}

	// Step 3: Change the target rs labels and # of replicas so we can migrate warm pods
	targetrsssel := fmt.Sprintf("%s=%s", serving.RevisionLabelKey, rev)
	if err != nil {
		m.logger.Errorf("Failed to parse selector: %v", zap.Error(err))
		return err
	}
	targetrss, err := m.kubeClientSet.ExtensionsV1beta1().ReplicaSets(ns).List(metav1.ListOptions{LabelSelector: targetrsssel})
	if err != nil {
		m.logger.Errorf("Failed to find target replicaSet of revision %s: %v", rev, zap.Error(err))
		return err
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

	targetrs.Spec.Replicas = &migratedScale
	targetrs.Spec.Selector = targetsel
	targetrs.Spec.Template.Labels = map[string]string{
		serving.FunctionLabelKey: f,
	}

	_, err = m.kubeClientSet.ExtensionsV1beta1().ReplicaSets(ns).Update(targetrs)
	if err != nil {
		m.logger.Errorf("Failed to update target rs %s: %v", targetrs.Name, zap.Error(err))
		return err
	}
	// Step 4: Add labels to migrated warm pods so we can restore the target rs to original labels
	migratePodOpt := metav1.ListOptions{
		LabelSelector: metav1.FormatLabelSelector(targetsel),
	}
	migratePods, err := m.kubeClientSet.CoreV1().Pods(ns).List(migratePodOpt)
	if err != nil {
		m.logger.Errorf("Failed to list migrated pods", zap.Error(err))
		return err
	}

	m.logger.Infof("number of migrated warm pods = %d", len(migratePods.Items))

	for _, p := range migratePods.Items {
		pclone := p.DeepCopy()
		pclone.Labels = targetrs.Labels
		newOwner := p.ObjectMeta.OwnerReferences[0]
		newOwner.Name = targetrs.Name
		newOwner.UID = targetrs.UID
		pclone.ObjectMeta.OwnerReferences[0] = newOwner
		_, err = m.kubeClientSet.Core().Pods(ns).Update(pclone)
		if err != nil {
			m.logger.Errorf("Failed to relabel pod %s: %v", p.Name, zap.Error(err))
			return err
		}
		if _, ok := migratedPodNames[p.Name]; ok {
			ip2str := strings.Replace(p.Status.PodIP, ".", "-", -1)
			podMigrateURL := fmt.Sprintf("http://%s.%s.pod.cluster.local:%d/%s?revision=%s", ip2str, p.Namespace, queue.RequestQueueAdminPort, queue.RequestQueuePoolMigratePath, rev)
			m.logger.Infof("pod migrate url: %s", podMigrateURL)
			resp, err := http.Get(podMigrateURL)
			if err != nil {
				m.logger.Errorf("init stat for pod %s:", p.Name, zap.Error(err))
			} else {
				defer resp.Body.Close()
			}
		}
	}
	// Step 5: Change the target rs labels back to original
	restorers, err := m.kubeClientSet.ExtensionsV1beta1().ReplicaSets(ns).Get(targetrs.Name, emptyGetOpts)
	if err != nil {
		m.logger.Errorf("Failed to find corresponding target rs %s: %v", restorers.Name, zap.Error(err))
		return err
	}
	rsclone := restorers.DeepCopy()
	rsclone.Spec.Selector = &metav1.LabelSelector{
		MatchLabels: rsclone.Labels,
	}
	rsclone.Spec.Template.Labels = rsclone.Labels
	rsclone.Spec.Replicas = &desiredScale
	_, err = m.kubeClientSet.ExtensionsV1beta1().ReplicaSets(ns).Update(rsclone)
	if err != nil {
		m.logger.Errorf("Failed to restore target rs %s", rsclone.Name, zap.Error(err))
		return err
	}

	// Step 6: Resume target deployment rollout
	d, err = m.kubeClientSet.ExtensionsV1beta1().Deployments(ns).Get(dn, emptyGetOpts)
	if err != nil {
		m.logger.Errorf("Failed to find corresponding deployment %s for KPA: %v", dn, zap.Error(err))
		return err
	}
	dclone = d.DeepCopy()

	dclone.Spec.Paused = false
	dclone.Spec.Replicas = &desiredScale
	delete(dclone.Spec.Selector.MatchLabels, "tmp")
	delete(dclone.Spec.Template.Labels, "tmp")
	_, err = m.kubeClientSet.ExtensionsV1beta1().Deployments(ns).Update(dclone)
	if err != nil {
		m.logger.Errorf("Failed to update deployment %s: %v", dclone.Name, zap.Error(err))
		return err
	}
	m.logger.Infof("Migration time = %d", time.Since(t)/time.Microsecond)

	return nil
}
func (m *migrator) updateStatus(desired *v1alpha1.PodAutoscaler) (*v1alpha1.PodAutoscaler, error) {
	kpa, err := m.servingClientSet.AutoscalingV1alpha1().PodAutoscalers(desired.Namespace).Get(desired.Name, emptyGetOpts)
	if err != nil {
		return nil, err
	}
	// Check if there is anything to update.
	if !reflect.DeepEqual(kpa.Status, desired.Status) {
		// Don't modify the informers copy
		existing := kpa.DeepCopy()
		existing.Status = desired.Status

		m.logger.Infof("migrator update kpa status: %+v", existing.Status)

		// TODO: for CRD there's no updatestatus, so use normal update
		return m.servingClientSet.AutoscalingV1alpha1().PodAutoscalers(kpa.Namespace).Update(existing)
		//	return prClient.UpdateStatus(newKPA)
	}
	return kpa, nil
}
