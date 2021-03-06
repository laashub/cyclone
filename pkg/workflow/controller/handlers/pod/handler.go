package pod

import (
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/caicloud/cyclone/pkg/k8s/clientset"
	"github.com/caicloud/cyclone/pkg/workflow/controller/handlers"
)

// Handler ...
type Handler struct {
	ClusterClient kubernetes.Interface
	Client        clientset.Interface
}

// Ensure *Handler has implemented handlers.Interface interface.
var _ handlers.Interface = (*Handler)(nil)

// ObjectCreated ...
func (h *Handler) ObjectCreated(obj interface{}) {
	// If Workflow Controller got restarted, previous started pods would be
	// observed by controller with create event. We need to handle update in
	// this case as well. Otherwise WorkflowRun may stuck in running state.
	h.onUpdate(obj)
}

// ObjectUpdated ...
func (h *Handler) ObjectUpdated(old, new interface{}) {
	h.onUpdate(new)
}

// ObjectDeleted ...
func (h *Handler) ObjectDeleted(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		log.Warning("unknown resource type")
		return
	}
	log.WithField("name", pod.Name).Debug("Observed pod deleted")

	// Check whether it's GC pod.
	if IsGCPod(pod) {
		return
	}

	operator, err := NewOperator(h.ClusterClient, h.Client, pod)
	if err != nil {
		log.Error("Create operator error: ", err)
		return
	}

	err = operator.OnDelete()
	if err != nil {
		log.WithField("pod", pod.Name).Error("process deleted pod error: ", err)
	}
}

func (h *Handler) onUpdate(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		log.Warning("unknown resource type")
		return
	}
	log.WithField("name", pod.Name).Debug("Observed pod updated")

	// Check whether it's GC pod.
	if IsGCPod(pod) {
		GCPodUpdated(h.ClusterClient, pod)
		return
	}

	// For stage pod, create operator to handle it.
	operator, err := NewOperator(h.ClusterClient, h.Client, pod)
	if err != nil {
		log.Error("Create operator error: ", err)
		return
	}

	err = operator.OnUpdated()
	if err != nil {
		log.WithField("pod", pod.Name).Error("process updated pod error: ", err)
	}
}
