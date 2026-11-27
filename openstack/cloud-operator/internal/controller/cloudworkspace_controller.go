package controller

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/imagedata"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/portsbinding"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	iaasv1alpha1 "github.com/omerharuncetin/openstack-ceph-operator/openstack/cloud-operator/api/v1alpha1"
	"github.com/omerharuncetin/openstack-ceph-operator/openstack/cloud-operator/internal/cloud"
	opmetrics "github.com/omerharuncetin/openstack-ceph-operator/openstack/cloud-operator/internal/metrics"
)

const cloudWorkspaceFinalizer = "iaas.cloud.io/cloud-workspace-finalizer"

type portWithOVNBinding struct {
	ports.Port
	portsbinding.PortsBindingExt
}

// CloudWorkspaceReconciler reconciles CloudWorkspace objects: Glance image,
// Cinder RBD boot volume, Neutron OVN port, and Nova VM.
type CloudWorkspaceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Cloud    *cloud.Clients
	Recorder EventEmitter
}

// Reconcile converges a CloudWorkspace to its target state.
func (r *CloudWorkspaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	start := time.Now()
	logger := log.FromContext(ctx)
	outcome := "success"
	defer func() {
		opmetrics.ReconcileDurationSeconds.WithLabelValues("cloudworkspace").Observe(time.Since(start).Seconds())
		opmetrics.ReconcileTotal.WithLabelValues("cloudworkspace", outcome).Inc()
	}()

	var ws iaasv1alpha1.CloudWorkspace
	if err := r.Get(ctx, req.NamespacedName, &ws); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		outcome = "error"
		return ctrl.Result{}, err
	}

	// Delete in reverse order: Nova VM -> Cinder volume -> Neutron port.
	if !ws.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&ws, cloudWorkspaceFinalizer) {
			return ctrl.Result{}, nil
		}
		if !shouldForceDelete(&ws) {
			if err := r.cleanupWorkspace(ctx, &ws); err != nil {
				outcome = "error"
				emitEvent(r.Recorder, &ws, corev1.EventTypeWarning, "CleanupFailed", "Reverse teardown failed: %v", err)
				return ctrl.Result{RequeueAfter: 10 * time.Second}, err
			}
			emitEvent(r.Recorder, &ws, corev1.EventTypeNormal, "WorkspaceDeleted", "Tore down Nova -> Cinder -> Neutron resources for %q", ws.Name)
		}
		controllerutil.RemoveFinalizer(&ws, cloudWorkspaceFinalizer)
		if err := r.Update(ctx, &ws); err != nil {
			outcome = "error"
			return ctrl.Result{}, err
		}
		opmetrics.ManagedResources.DeleteLabelValues("CloudWorkspace", ws.Namespace, ws.Name)
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&ws, cloudWorkspaceFinalizer) {
		controllerutil.AddFinalizer(&ws, cloudWorkspaceFinalizer)
		if err := r.Update(ctx, &ws); err != nil {
			outcome = "error"
			return ctrl.Result{}, err
		}
	}

	// 1. Resolve referenced VirtualNetwork CR.
	var vn iaasv1alpha1.VirtualNetwork
	if err := r.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: ws.Spec.VirtualNetworkRef}, &vn); err != nil {
		if apierrors.IsNotFound(err) {
			ws.Status.Phase = "Pending"
			setCondition(&ws.Status.Conditions, ws.Generation, "Ready", metav1.ConditionFalse, "WaitingForVirtualNetwork",
				fmt.Sprintf("VirtualNetwork %q not found yet", ws.Spec.VirtualNetworkRef))
			_ = r.Status().Update(ctx, &ws)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		outcome = "error"
		return ctrl.Result{}, err
	}
	if vn.Status.NetworkID == "" {
		ws.Status.Phase = "Pending"
		setCondition(&ws.Status.Conditions, ws.Generation, "Ready", metav1.ConditionFalse, "WaitingForVirtualNetwork",
			fmt.Sprintf("VirtualNetwork %q is not ready yet", vn.Name))
		_ = r.Status().Update(ctx, &ws)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// 2. Ensure Glance v2 base image exists in Ceph "images" pool with protected @base snapshot.
	imgID, err := r.ensureGlanceImage(ctx, &ws)
	if err != nil {
		outcome = "error"
		r.markFailed(ctx, &ws, "ImageReady", "GlanceError", err)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}
	ws.Status.ImageID = imgID
	setCondition(&ws.Status.Conditions, ws.Generation, "ImageReady", metav1.ConditionTrue, "ImageActive", fmt.Sprintf("Glance image %q (%s) is active in Ceph", ws.Spec.ImageName, imgID))

	// 3. Ensure Cinder v3 CoW boot volume exists (and expand online if spec.storageGiB grew).
	vol, err := r.ensureBootVolume(ctx, &ws, imgID)
	if err != nil {
		outcome = "error"
		r.markFailed(ctx, &ws, "VolumeReady", "CinderError", err)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}
	ws.Status.VolumeID = vol.ID
	ws.Status.VolumeSizeGiB = vol.Size
	ws.Status.CephRBDParent = vol.Metadata["rbd_parent"]
	setCondition(&ws.Status.Conditions, ws.Generation, "VolumeReady", metav1.ConditionTrue, "CoWVolumeReady",
		fmt.Sprintf("Cinder Ceph RBD volume %s (%d GiB, parent=%s) is ready", vol.ID, vol.Size, ws.Status.CephRBDParent))

	// 4. Ensure dedicated Neutron port on the tenant VirtualNetwork.
	port, err := r.ensurePort(ctx, &ws, &vn)
	if err != nil {
		outcome = "error"
		r.markFailed(ctx, &ws, "InstanceReady", "NeutronPortError", err)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}
	ws.Status.PortID = port.ID
	if len(port.FixedIPs) > 0 {
		ws.Status.IPAddress = port.FixedIPs[0].IPAddress
	}

	// 5. Ensure Nova VM exists and honor spec.desiredHost via zero-copy os-migrateLive.
	srv, err := r.ensureServer(ctx, &ws, vol.ID, port.ID)
	if err != nil {
		outcome = "error"
		r.markFailed(ctx, &ws, "InstanceReady", "NovaError", err)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}

	if ws.Spec.DesiredHost != "" && srv.Host != ws.Spec.DesiredHost {
		logger.Info("triggering zero-copy live migration over shared Ceph RBD",
			"workspace", ws.Name, "server_id", srv.ID, "from_host", srv.Host, "to_host", ws.Spec.DesiredHost)
		if err := servers.LiveMigrate(ctx, r.Cloud.Nova, srv.ID, servers.LiveMigrateOpts{
			Host:           new(ws.Spec.DesiredHost),
			BlockMigration: new(false),
		}).ExtractErr(); err != nil {
			opmetrics.LiveMigrationsTotal.WithLabelValues("error").Inc()
			outcome = "error"
			r.markFailed(ctx, &ws, "InstanceReady", "LiveMigrationFailed", err)
			emitEvent(r.Recorder, &ws, corev1.EventTypeWarning, "LiveMigrationFailed", "Zero-copy live migration to %q failed: %v", ws.Spec.DesiredHost, err)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, err
		}
		opmetrics.LiveMigrationsTotal.WithLabelValues("success").Inc()
		emitEvent(r.Recorder, &ws, corev1.EventTypeNormal, "LiveMigrated", "Zero-copy live migrated VM %s from %s to %s over shared Ceph RBD", srv.ID, srv.Host, ws.Spec.DesiredHost)

		srv, err = servers.Get(ctx, r.Cloud.Nova, srv.ID).Extract()
		if err != nil {
			outcome = "error"
			return ctrl.Result{RequeueAfter: 5 * time.Second}, err
		}
	}

	// Observe live Neutron OVN port chassis binding (binding:host_id).
	var bound portWithOVNBinding
	if err := ports.Get(ctx, r.Cloud.Neutron, port.ID).ExtractInto(&bound); err != nil {
		outcome = "error"
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
	}

	ws.Status.ObservedGeneration = ws.Generation
	ws.Status.Phase = "Running"
	ws.Status.ServerID = srv.ID
	ws.Status.ComputeHost = srv.Host
	ws.Status.OVNPortHost = bound.HostID
	setCondition(&ws.Status.Conditions, ws.Generation, "InstanceReady", metav1.ConditionTrue, "ServerActive",
		fmt.Sprintf("Nova instance %s is %s on host %s (OVN chassis %s)", srv.ID, srv.Status, srv.Host, bound.HostID))
	setCondition(&ws.Status.Conditions, ws.Generation, "Ready", metav1.ConditionTrue, "Reconciled",
		fmt.Sprintf("CloudWorkspace %q running at %s on %s", ws.Name, ws.Status.IPAddress, srv.Host))

	if err := r.Status().Update(ctx, &ws); err != nil {
		outcome = "error"
		return ctrl.Result{}, err
	}

	opmetrics.ManagedResources.WithLabelValues("CloudWorkspace", ws.Namespace, ws.Name).Set(1)
	return ctrl.Result{}, nil
}

func (r *CloudWorkspaceReconciler) ensureGlanceImage(ctx context.Context, ws *iaasv1alpha1.CloudWorkspace) (string, error) {
	if ws.Status.ImageID != "" {
		if img, err := images.Get(ctx, r.Cloud.Image, ws.Status.ImageID).Extract(); err == nil && img.Status == images.ImageStatusActive {
			return img.ID, nil
		} else if err != nil && !isOpenStackNotFound(err) {
			return "", err
		}
	}

	pages, err := images.List(r.Cloud.Image, images.ListOpts{Name: ws.Spec.ImageName}).AllPages(ctx)
	if err != nil {
		return "", err
	}
	existing, err := images.ExtractImages(pages)
	if err != nil {
		return "", err
	}
	for _, img := range existing {
		if img.Status == images.ImageStatusActive {
			return img.ID, nil
		}
	}

	// Seed Glance base image into Ceph "images" pool with protected @base snapshot.
	vis := images.ImageVisibilityPublic
	created, err := images.Create(ctx, r.Cloud.Image, images.CreateOpts{
		Name:            ws.Spec.ImageName,
		Visibility:      &vis,
		ContainerFormat: "bare",
		DiskFormat:      "raw",
	}).Extract()
	if err != nil {
		return "", err
	}
	stub := bytes.Repeat([]byte("glance-ceph-base-image-"), 64)
	if err := imagedata.Upload(ctx, r.Cloud.Image, created.ID, bytes.NewReader(stub)).ExtractErr(); err != nil {
		return "", err
	}
	emitEvent(r.Recorder, ws, corev1.EventTypeNormal, "ImageProvisioned", "Uploaded Glance base image %q (%s) to Ceph RBD", ws.Spec.ImageName, created.ID)
	return created.ID, nil
}

func (r *CloudWorkspaceReconciler) ensureBootVolume(ctx context.Context, ws *iaasv1alpha1.CloudWorkspace, imgID string) (*volumes.Volume, error) {
	desiredGiB := cmp.Or(ws.Spec.StorageGiB, 1)

	if ws.Status.VolumeID != "" {
		vol, err := volumes.Get(ctx, r.Cloud.Cinder, ws.Status.VolumeID).Extract()
		if err == nil {
			if desiredGiB > vol.Size {
				if err := volumes.ExtendSize(ctx, r.Cloud.Cinder, vol.ID, volumes.ExtendSizeOpts{NewSize: desiredGiB}).ExtractErr(); err != nil {
					return nil, fmt.Errorf("online resize volume %s to %d GiB: %w", vol.ID, desiredGiB, err)
				}
				emitEvent(r.Recorder, ws, corev1.EventTypeNormal, "VolumeResized", "Resized Ceph RBD boot volume %s from %d GiB to %d GiB", vol.ID, vol.Size, desiredGiB)
				return volumes.Get(ctx, r.Cloud.Cinder, vol.ID).Extract()
			}
			return vol, nil
		}
		if !isOpenStackNotFound(err) {
			return nil, err
		}
	}

	vol, err := volumes.Create(ctx, r.Cloud.Cinder, volumes.CreateOpts{
		Name:    ws.Name + "-boot",
		Size:    desiredGiB,
		ImageID: imgID,
		Metadata: map[string]string{
			"k8s_namespace": ws.Namespace,
			"k8s_workspace": ws.Name,
		},
	}, nil).Extract()
	if err != nil {
		return nil, err
	}
	emitEvent(r.Recorder, ws, corev1.EventTypeNormal, "VolumeCloned", "Provisioned Ceph CoW boot volume %s (%d GiB) from image %s", vol.ID, vol.Size, imgID)
	return vol, nil
}

func (r *CloudWorkspaceReconciler) ensurePort(ctx context.Context, ws *iaasv1alpha1.CloudWorkspace, vn *iaasv1alpha1.VirtualNetwork) (*ports.Port, error) {
	if ws.Status.PortID != "" {
		if p, err := ports.Get(ctx, r.Cloud.Neutron, ws.Status.PortID).Extract(); err == nil {
			return p, nil
		} else if !isOpenStackNotFound(err) {
			return nil, err
		}
	}

	opts := ports.CreateOpts{
		Name:         ws.Name + "-nic0",
		NetworkID:    vn.Status.NetworkID,
		AdminStateUp: new(true),
	}
	if vn.Status.SecurityGroupID != "" {
		sgs := []string{vn.Status.SecurityGroupID}
		opts.SecurityGroups = &sgs
	}
	return ports.Create(ctx, r.Cloud.Neutron, opts).Extract()
}

func (r *CloudWorkspaceReconciler) ensureServer(ctx context.Context, ws *iaasv1alpha1.CloudWorkspace, volID, portID string) (*servers.Server, error) {
	if ws.Status.ServerID != "" {
		if srv, err := servers.Get(ctx, r.Cloud.Nova, ws.Status.ServerID).Extract(); err == nil {
			return srv, nil
		} else if !isOpenStackNotFound(err) {
			return nil, err
		}
	}

	createOpts := servers.CreateOpts{
		Name:               ws.Name,
		FlavorRef:          cmp.Or(ws.Spec.FlavorRef, "m1.small"),
		HypervisorHostname: ws.Spec.DesiredHost,
		Networks:           []servers.Network{{Port: portID}},
		BlockDevice: []servers.BlockDevice{
			{
				UUID:            volID,
				SourceType:      servers.SourceVolume,
				DestinationType: servers.DestinationVolume,
				BootIndex:       0,
			},
		},
		Metadata: map[string]string{
			"k8s_namespace": ws.Namespace,
			"k8s_workspace": ws.Name,
		},
	}

	srv, err := servers.Create(ctx, r.Cloud.Nova, createOpts, nil).Extract()
	if err != nil {
		return nil, err
	}
	emitEvent(r.Recorder, ws, corev1.EventTypeNormal, "InstanceCreated", "Booted Nova VM %q (%s) on host %s", srv.Name, srv.ID, srv.Host)
	return srv, nil
}

// cleanupWorkspace deletes in reverse order: Nova VM -> Cinder volume -> Neutron port.
func (r *CloudWorkspaceReconciler) cleanupWorkspace(ctx context.Context, ws *iaasv1alpha1.CloudWorkspace) error {
	if ws.Status.ServerID != "" {
		if err := servers.Delete(ctx, r.Cloud.Nova, ws.Status.ServerID).ExtractErr(); err != nil && !isOpenStackNotFound(err) {
			return fmt.Errorf("delete nova server %s: %w", ws.Status.ServerID, err)
		}
	}
	if ws.Status.VolumeID != "" {
		if err := volumes.Delete(ctx, r.Cloud.Cinder, ws.Status.VolumeID, nil).ExtractErr(); err != nil && !isOpenStackNotFound(err) {
			return fmt.Errorf("delete cinder volume %s: %w", ws.Status.VolumeID, err)
		}
	}
	if ws.Status.PortID != "" {
		if err := ports.Delete(ctx, r.Cloud.Neutron, ws.Status.PortID).ExtractErr(); err != nil && !isOpenStackNotFound(err) {
			return fmt.Errorf("delete neutron port %s: %w", ws.Status.PortID, err)
		}
	}
	return nil
}

func (r *CloudWorkspaceReconciler) markFailed(ctx context.Context, ws *iaasv1alpha1.CloudWorkspace, condType, reason string, err error) {
	ws.Status.Phase = "Degraded"
	setCondition(&ws.Status.Conditions, ws.Generation, condType, metav1.ConditionFalse, reason, err.Error())
	setCondition(&ws.Status.Conditions, ws.Generation, "Ready", metav1.ConditionFalse, reason, err.Error())
	_ = r.Status().Update(ctx, ws)
	opmetrics.ManagedResources.WithLabelValues("CloudWorkspace", ws.Namespace, ws.Name).Set(0)
}

// SetupWithManager registers the CloudWorkspace controller with the manager.
func (r *CloudWorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&iaasv1alpha1.CloudWorkspace{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}
