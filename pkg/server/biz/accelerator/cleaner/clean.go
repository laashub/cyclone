package cleaner

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/caicloud/nirvana/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/caicloud/cyclone/pkg/k8s/clientset"
	"github.com/caicloud/cyclone/pkg/meta"
	"github.com/caicloud/cyclone/pkg/server/apis/v1alpha1"
	"github.com/caicloud/cyclone/pkg/server/common"
	"github.com/caicloud/cyclone/pkg/server/config"
)

// Cleaner cleans acceleration caches of a project
type Cleaner struct {
	clusterClient    kubernetes.Interface
	client           clientset.Interface
	projectName      string
	projectNamespace string
	config           *config.CacheCleaner
}

// NewCleaner creates a new acceleration caches cleaner.
func NewCleaner(clusterClient kubernetes.Interface, client clientset.Interface, projectNamespace, projectName string) *Cleaner {
	return &Cleaner{
		clusterClient:    clusterClient,
		client:           client,
		projectName:      projectName,
		projectNamespace: projectNamespace,
		config:           &config.Config.CacheCleaner,
	}
}

// Clean start a pod to do the clean work, and returns the pod name.
func (c *Cleaner) Clean(pvcNamespace, pvcName string) (*v1alpha1.AccelerationCacheCleanupStatus, error) {
	// Create a cache clean pod to clean acceleration cache data on PV.
	cacheCleanPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.podName(c.projectName),
			Namespace: pvcNamespace,
			Labels: map[string]string{
				// meta.LabelWorkflowRunName: o.wfr.Name,
				meta.LabelPodKind:      meta.PodKindAccelerationGC.String(),
				meta.LabelPodCreatedBy: meta.CycloneCreator,
			},
			Annotations: map[string]string{
				meta.AnnotationIstioInject: meta.AnnotationValueFalse,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:    common.CacheCleanupContainerName,
					Image:   c.config.Image,
					Command: []string{"rm", "-rf", fmt.Sprintf("/%s/%s", common.CachePrefixPath, c.projectName)},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      common.DefaultCacheVolumeName,
							MountPath: fmt.Sprintf("/%s", common.CachePrefixPath),
							SubPath:   common.CachePrefixPath,
						},
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(getOrDefault(c.config, corev1.ResourceRequestsCPU, "50m")),
							corev1.ResourceMemory: resource.MustParse(getOrDefault(c.config, corev1.ResourceRequestsMemory, "32Mi")),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(getOrDefault(c.config, corev1.ResourceLimitsCPU, "100m")),
							corev1.ResourceMemory: resource.MustParse(getOrDefault(c.config, corev1.ResourceLimitsMemory, "128Mi")),
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: common.DefaultCacheVolumeName,
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName,
							ReadOnly:  false,
						},
					},
				},
			},
		},
	}

	pod, err := c.clusterClient.CoreV1().Pods(pvcNamespace).Create(cacheCleanPod)
	if err != nil {
		return nil, err
	}

	go c.watch(pvcNamespace, pod.Name)

	startTime := pod.DeepCopy().CreationTimestamp
	status := &v1alpha1.AccelerationCacheCleanupStatus{
		TaskID:             pod.Name,
		Phase:              v1alpha1.CacheCleanupRunning,
		StartTime:          startTime,
		LastTransitionTime: startTime,
	}
	if err := c.writeDownResult(status); err != nil {
		log.Warningf("Write cache clean resutl error: %v", err)
		return status, err
	}
	return status, nil
}

func (c *Cleaner) watch(namespace, podName string) {
	var statusToUpdate *v1alpha1.AccelerationCacheCleanupStatus

	defer func(ns, name string) {
		if err := c.writeDownResult(statusToUpdate); err != nil {
			log.Errorf("write down result error: %v, result: %v", err, statusToUpdate)
		}
		if err := c.stopClean(ns, name); err != nil {
			log.Errorf("Stop cache cleaner pod %s/%s error: %v", ns, name, err)
		}
	}(namespace, podName)

	w, err := c.clusterClient.CoreV1().Pods(namespace).Watch(metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", podName),
	})
	if err != nil {
		log.Warningf("Watch cache cleanup, start to watch pod failed, namespace: %v, pod name: %v", namespace, podName)
		return
	}

	defer w.Stop()

	for {
		for e := range w.ResultChan() {
			pod, ok := e.Object.(*corev1.Pod)
			if !ok {
				log.Warningf("Watch cache cleanup, object is not a pod, event type: %v, event object: %v", e.Type, e.Object)
			}

			switch pod.Status.Phase {
			case corev1.PodSucceeded:
				statusToUpdate = &v1alpha1.AccelerationCacheCleanupStatus{
					TaskID:             pod.Name,
					Phase:              v1alpha1.CacheCleanupSucceeded,
					StartTime:          pod.DeepCopy().CreationTimestamp,
					LastTransitionTime: metav1.NewTime(time.Now()),
				}
				if len(pod.Status.ContainerStatuses) > 0 && pod.Status.ContainerStatuses[0].Name == common.CacheCleanupContainerName {
					statusToUpdate.LastTransitionTime = pod.Status.ContainerStatuses[0].State.Terminated.FinishedAt
				}
				return
			case corev1.PodRunning, corev1.PodPending:
			default:
				if len(pod.Status.ContainerStatuses) > 0 && pod.Status.ContainerStatuses[0].Name == common.CacheCleanupContainerName {
					if pod.Status.ContainerStatuses[0].State.Terminated != nil {
						if pod.Status.ContainerStatuses[0].State.Terminated.ExitCode == 0 {
							statusToUpdate = &v1alpha1.AccelerationCacheCleanupStatus{
								TaskID:             pod.Name,
								Phase:              v1alpha1.CacheCleanupSucceeded,
								StartTime:          pod.DeepCopy().CreationTimestamp,
								LastTransitionTime: pod.Status.ContainerStatuses[0].State.Terminated.FinishedAt,
								Reason:             pod.Status.ContainerStatuses[0].State.Terminated.Reason,
							}
						} else {
							statusToUpdate = &v1alpha1.AccelerationCacheCleanupStatus{
								TaskID:             pod.Name,
								Phase:              v1alpha1.CacheCleanupFailed,
								StartTime:          pod.DeepCopy().CreationTimestamp,
								LastTransitionTime: pod.Status.ContainerStatuses[0].State.Terminated.FinishedAt,
								Reason:             pod.Status.ContainerStatuses[0].State.Terminated.Reason,
							}

						}
						return
					}
				}
			}
		}
	}
}

func (c *Cleaner) writeDownResult(status *v1alpha1.AccelerationCacheCleanupStatus) error {
	if status == nil {
		return fmt.Errorf("Status is nil")
	}
	// Update Project status event with retry.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest, err := c.client.CycloneV1alpha1().Projects(c.projectNamespace).Get(c.projectName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		project := latest.DeepCopy()

		var latestStatus v1alpha1.CacheCleanupStatus
		if project.Annotations != nil {
			s, ok := project.Annotations[meta.AnnotationCacheCleanupStatus]
			if ok && len(s) > 0 {
				if err = json.Unmarshal([]byte(s), &latestStatus); err != nil {
					return err
				}
			}
		} else {
			project.Annotations = make(map[string]string)
		}

		if status.Phase == v1alpha1.CacheCleanupSucceeded && status.LastTransitionTime.After(latestStatus.Acceleration.LatestSucceededTimestamp.Time) {
			latestStatus.Acceleration.LatestSucceededTimestamp = status.LastTransitionTime
		}
		latestStatus.Acceleration.LatestStatus = *status

		ss, err := json.Marshal(latestStatus)
		if err != nil {
			return err
		}
		project.Annotations[meta.AnnotationCacheCleanupStatus] = string(ss)

		// update project
		_, err = c.client.CycloneV1alpha1().Projects(c.projectNamespace).Update(project)
		return err
	})
}

// StopClean stops cache cleanup work, note this will NOT update project cache cleanup status.
func (c *Cleaner) stopClean(namespace, name string) error {
	return c.clusterClient.CoreV1().Pods(namespace).Delete(name, metav1.NewDeleteOptions(0))
}

func (c *Cleaner) podName(project string) string {
	return fmt.Sprintf("%s-cache-cleaner", project)
}

// getOrDefault gets resource requirement from config, if not set, use default value.
func getOrDefault(config *config.CacheCleaner, key corev1.ResourceName, defaultValue string) string {
	v, ok := config.ResourceRequirements[key]
	if ok {
		return v
	}

	return defaultValue
}
