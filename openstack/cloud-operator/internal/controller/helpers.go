// Package controller implements Kubernetes reconcilers for TenantStoragePool,
// VirtualNetwork, and CloudWorkspace custom resources.
package controller

import (
	"cmp"
	"net/http"
	"slices"

	"github.com/gophercloud/gophercloud/v2"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	iaasv1alpha1 "github.com/omerharuncetin/openstack-ceph-operator/openstack/cloud-operator/api/v1alpha1"
)

// EventEmitter matches k8s.io/client-go/tools/events.EventRecorder.
type EventEmitter interface {
	Eventf(regarding runtime.Object, related runtime.Object, eventtype, reason, action, note string, args ...any)
}

func emitEvent(rec EventEmitter, obj runtime.Object, eventType, reason, msgFmt string, args ...any) {
	if rec != nil {
		rec.Eventf(obj, nil, eventType, reason, reason, msgFmt, args...)
	}
}

func setCondition(conditions *[]metav1.Condition, gen int64, condType string, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: gen,
		Reason:             reason,
		Message:            message,
	})
	// Keep conditions sorted by Type so status serialization is deterministic.
	slices.SortFunc(*conditions, func(a, b metav1.Condition) int {
		return cmp.Compare(a.Type, b.Type)
	})
}

func shouldForceDelete(obj metav1.Object) bool {
	ann := obj.GetAnnotations()
	return ann != nil && ann[iaasv1alpha1.ForceDeleteAnnotation] == "true"
}

func isOpenStackNotFound(err error) bool {
	return err != nil && gophercloud.ResponseCodeIs(err, http.StatusNotFound)
}
