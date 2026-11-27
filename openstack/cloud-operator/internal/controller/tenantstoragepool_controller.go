package controller

import (
	"cmp"
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/omerharuncetin/openstack-ceph-operator/ceph/ceph-client/pkg/cephapi"
	iaasv1alpha1 "github.com/omerharuncetin/openstack-ceph-operator/openstack/cloud-operator/api/v1alpha1"
	"github.com/omerharuncetin/openstack-ceph-operator/openstack/cloud-operator/internal/cloud"
	opmetrics "github.com/omerharuncetin/openstack-ceph-operator/openstack/cloud-operator/internal/metrics"
)

const storagePoolFinalizer = "iaas.cloud.io/storage-pool-finalizer"

// TenantStoragePoolReconciler reconciles TenantStoragePool objects against a
// Ceph cluster via the ceph-mgr Dashboard REST API (pkg/cephapi).
type TenantStoragePoolReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Cloud    *cloud.Clients
	Recorder EventEmitter
}

// Reconcile ensures the declared Ceph RADOS storage pool exists and reports
// live cluster health in Status.Conditions.
func (r *TenantStoragePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	start := time.Now()
	logger := log.FromContext(ctx)
	outcome := "success"
	defer func() {
		opmetrics.ReconcileDurationSeconds.WithLabelValues("tenantstoragepool").Observe(time.Since(start).Seconds())
		opmetrics.ReconcileTotal.WithLabelValues("tenantstoragepool", outcome).Inc()
	}()

	var pool iaasv1alpha1.TenantStoragePool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		outcome = "error"
		return ctrl.Result{}, err
	}

	poolName := cmp.Or(pool.Spec.PoolName, pool.Name)

	// Handle deletion with finalizer cleanup.
	if !pool.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&pool, storagePoolFinalizer) {
			return ctrl.Result{}, nil
		}
		if !shouldForceDelete(&pool) {
			if err := r.Cloud.Ceph.DeletePool(ctx, poolName); err != nil && !cephapi.IsNotFound(err) {
				outcome = "error"
				emitEvent(r.Recorder, &pool, corev1.EventTypeWarning, "PoolDeleteFailed", "Failed to delete Ceph pool %q: %v", poolName, err)
				return ctrl.Result{RequeueAfter: 10 * time.Second}, err
			}
			emitEvent(r.Recorder, &pool, corev1.EventTypeNormal, "PoolDeleted", "Deleted Ceph RADOS pool %q", poolName)
		}
		controllerutil.RemoveFinalizer(&pool, storagePoolFinalizer)
		if err := r.Update(ctx, &pool); err != nil {
			outcome = "error"
			return ctrl.Result{}, err
		}
		opmetrics.ManagedResources.DeleteLabelValues("TenantStoragePool", pool.Namespace, pool.Name)
		return ctrl.Result{}, nil
	}

	// Ensure finalizer is installed before creating external Ceph state.
	if !controllerutil.ContainsFinalizer(&pool, storagePoolFinalizer) {
		controllerutil.AddFinalizer(&pool, storagePoolFinalizer)
		if err := r.Update(ctx, &pool); err != nil {
			outcome = "error"
			return ctrl.Result{}, err
		}
	}

	// 1. Check live Ceph cluster health.
	health, err := r.Cloud.Ceph.Health(ctx)
	if err != nil {
		outcome = "error"
		pool.Status.Phase = "Degraded"
		setCondition(&pool.Status.Conditions, pool.Generation, "CephHealthy", metav1.ConditionFalse, "CephUnreachable", err.Error())
		setCondition(&pool.Status.Conditions, pool.Generation, "Ready", metav1.ConditionFalse, "CephUnreachable", err.Error())
		_ = r.Status().Update(ctx, &pool)
		opmetrics.ManagedResources.WithLabelValues("TenantStoragePool", pool.Namespace, pool.Name).Set(0)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}

	cephStatus := health.Health.Status
	if cephStatus == "HEALTH_ERR" {
		setCondition(&pool.Status.Conditions, pool.Generation, "CephHealthy", metav1.ConditionFalse, "ClusterError", fmt.Sprintf("Ceph cluster reports %s", cephStatus))
	} else {
		setCondition(&pool.Status.Conditions, pool.Generation, "CephHealthy", metav1.ConditionTrue, "ClusterHealthy", fmt.Sprintf("Ceph cluster reports %s (%d/%d OSDs up)", cephStatus, health.NumUpOSDs(), health.NumOSDs()))
	}

	// 2. Ensure RADOS pool exists in Ceph.
	livePool, err := r.Cloud.Ceph.GetPool(ctx, poolName)
	if err != nil {
		if !cephapi.IsNotFound(err) {
			outcome = "error"
			setCondition(&pool.Status.Conditions, pool.Generation, "Ready", metav1.ConditionFalse, "CephAPIError", err.Error())
			_ = r.Status().Update(ctx, &pool)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, err
		}

		app := cmp.Or(pool.Spec.Application, "rbd")
		createReq := cephapi.CreatePoolRequest{
			Pool:                poolName,
			PoolType:            cmp.Or(pool.Spec.PoolType, "replicated"),
			PGNum:               cmp.Or(pool.Spec.PGNum, 16),
			Size:                pool.Spec.Size,
			MinSize:             pool.Spec.MinSize,
			RuleName:            pool.Spec.CrushRule,
			ApplicationMetadata: []string{app},
			QuotaMaxBytes:       pool.Spec.QuotaMaxBytes,
		}
		if err := r.Cloud.Ceph.CreatePool(ctx, createReq); err != nil {
			outcome = "error"
			pool.Status.Phase = "Failed"
			setCondition(&pool.Status.Conditions, pool.Generation, "Ready", metav1.ConditionFalse, "CreatePoolFailed", err.Error())
			_ = r.Status().Update(ctx, &pool)
			emitEvent(r.Recorder, &pool, corev1.EventTypeWarning, "PoolCreateFailed", "Failed creating Ceph pool %q: %v", poolName, err)
			return ctrl.Result{RequeueAfter: 15 * time.Second}, err
		}

		logger.Info("created Ceph RADOS storage pool", "pool", poolName, "pg_num", createReq.PGNum)
		emitEvent(r.Recorder, &pool, corev1.EventTypeNormal, "PoolCreated", "Created Ceph RADOS pool %q (pg_num=%d, app=%s)", poolName, createReq.PGNum, app)

		livePool, err = r.Cloud.Ceph.GetPool(ctx, poolName)
		if err != nil {
			outcome = "error"
			return ctrl.Result{RequeueAfter: 5 * time.Second}, err
		}
	}

	pool.Status.ObservedGeneration = pool.Generation
	pool.Status.Phase = "Ready"
	pool.Status.PoolID = livePool.PoolID
	pool.Status.PoolName = livePool.PoolName
	pool.Status.CephHealth = cephStatus
	setCondition(&pool.Status.Conditions, pool.Generation, "Ready", metav1.ConditionTrue, "Reconciled", fmt.Sprintf("Ceph pool %q (id=%d, pg_num=%d) is active", livePool.PoolName, livePool.PoolID, livePool.PGNum))

	if err := r.Status().Update(ctx, &pool); err != nil {
		outcome = "error"
		return ctrl.Result{}, err
	}

	opmetrics.ManagedResources.WithLabelValues("TenantStoragePool", pool.Namespace, pool.Name).Set(1)
	return ctrl.Result{}, nil
}

// SetupWithManager registers the TenantStoragePool controller with the manager.
func (r *TenantStoragePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&iaasv1alpha1.TenantStoragePool{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}
