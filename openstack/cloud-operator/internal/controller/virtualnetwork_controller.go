package controller

import (
	"cmp"
	"context"
	"fmt"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/routers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/groups"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/rules"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"
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

	iaasv1alpha1 "github.com/omerharuncetin/openstack-ceph-operator/openstack/cloud-operator/api/v1alpha1"
	"github.com/omerharuncetin/openstack-ceph-operator/openstack/cloud-operator/internal/cloud"
	opmetrics "github.com/omerharuncetin/openstack-ceph-operator/openstack/cloud-operator/internal/metrics"
)

const virtualNetworkFinalizer = "iaas.cloud.io/virtual-network-finalizer"

// VirtualNetworkReconciler reconciles VirtualNetwork custom resources into
// OpenStack Neutron networks, subnets, routers, and drift-healed security groups.
type VirtualNetworkReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	Cloud                *cloud.Clients
	Recorder             EventEmitter
	DriftRequeueInterval time.Duration
}

func (r *VirtualNetworkReconciler) requeueInterval() time.Duration {
	if r.DriftRequeueInterval > 0 {
		return r.DriftRequeueInterval
	}
	return 60 * time.Second
}

type ruleKey struct {
	Direction  string
	Protocol   string
	PortMin    int
	PortMax    int
	RemoteCIDR string
}

// Reconcile converges Neutron L2/L3 resources and heals out-of-band security
// group drift (restoring deleted rules and removing unauthorized ingress rules).
func (r *VirtualNetworkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	start := time.Now()
	logger := log.FromContext(ctx)
	outcome := "success"
	defer func() {
		opmetrics.ReconcileDurationSeconds.WithLabelValues("virtualnetwork").Observe(time.Since(start).Seconds())
		opmetrics.ReconcileTotal.WithLabelValues("virtualnetwork", outcome).Inc()
	}()

	var vn iaasv1alpha1.VirtualNetwork
	if err := r.Get(ctx, req.NamespacedName, &vn); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		outcome = "error"
		return ctrl.Result{}, err
	}

	// Delete in reverse order: router interface -> router -> security group -> subnet -> network.
	if !vn.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&vn, virtualNetworkFinalizer) {
			return ctrl.Result{}, nil
		}
		if !shouldForceDelete(&vn) {
			if err := r.cleanupNeutron(ctx, &vn); err != nil {
				outcome = "error"
				emitEvent(r.Recorder, &vn, corev1.EventTypeWarning, "CleanupFailed", "Neutron teardown failed: %v", err)
				return ctrl.Result{RequeueAfter: 10 * time.Second}, err
			}
			emitEvent(r.Recorder, &vn, corev1.EventTypeNormal, "NetworkDeleted", "Deleted Neutron network stack for %q", vn.Name)
		}
		controllerutil.RemoveFinalizer(&vn, virtualNetworkFinalizer)
		if err := r.Update(ctx, &vn); err != nil {
			outcome = "error"
			return ctrl.Result{}, err
		}
		opmetrics.ManagedResources.DeleteLabelValues("VirtualNetwork", vn.Namespace, vn.Name)
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&vn, virtualNetworkFinalizer) {
		controllerutil.AddFinalizer(&vn, virtualNetworkFinalizer)
		if err := r.Update(ctx, &vn); err != nil {
			outcome = "error"
			return ctrl.Result{}, err
		}
	}

	alreadyProvisioned := vn.Status.Phase == "Ready" && vn.Status.SecurityGroupID != ""

	// 1. Ensure Neutron Network.
	netID, err := r.ensureNetwork(ctx, &vn)
	if err != nil {
		outcome = "error"
		r.markFailed(ctx, &vn, "NetworkError", err)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}
	vn.Status.NetworkID = netID

	// 2. Ensure Neutron Subnet.
	subID, err := r.ensureSubnet(ctx, &vn, netID)
	if err != nil {
		outcome = "error"
		r.markFailed(ctx, &vn, "SubnetError", err)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}
	vn.Status.SubnetID = subID

	// 3. Ensure optional Neutron L3 Router + Subnet Interface.
	if vn.Spec.EnableRouter {
		rtrID, err := r.ensureRouter(ctx, &vn, subID)
		if err != nil {
			outcome = "error"
			r.markFailed(ctx, &vn, "RouterError", err)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, err
		}
		vn.Status.RouterID = rtrID
	}

	// 4. Ensure Neutron Security Group.
	sgID, err := r.ensureSecurityGroup(ctx, &vn)
	if err != nil {
		outcome = "error"
		r.markFailed(ctx, &vn, "SecurityGroupError", err)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}
	vn.Status.SecurityGroupID = sgID

	// 5. Reconcile declarative Security Group rules & self-heal out-of-band drift.
	added, removed, activeCount, err := r.reconcileSecurityRules(ctx, &vn, sgID)
	if err != nil {
		outcome = "error"
		r.markFailed(ctx, &vn, "SecurityRulesError", err)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}

	healed := int64(removed)
	if alreadyProvisioned {
		healed += int64(added)
	}
	if healed > 0 {
		vn.Status.DriftHealsTotal += healed
		if added > 0 && alreadyProvisioned {
			opmetrics.DriftHealsTotal.WithLabelValues("virtualnetwork", "recreate_rule").Add(float64(added))
		}
		if removed > 0 {
			opmetrics.DriftHealsTotal.WithLabelValues("virtualnetwork", "remove_unauthorized_rule").Add(float64(removed))
		}
		logger.Info("healed out-of-band Neutron security group drift",
			"virtualnetwork", vn.Name, "sg_id", sgID, "recreated", added, "removed_unauthorized", removed)
		emitEvent(r.Recorder, &vn, corev1.EventTypeWarning, "DriftHealed",
			"Self-healed Neutron security group %s drift (restored=%d, removed_unauthorized=%d)", sgID, added, removed)
		setCondition(&vn.Status.Conditions, vn.Generation, "DriftHealed", metav1.ConditionTrue, "DriftCorrected",
			fmt.Sprintf("Healed %d drifted security group rule(s) (cumulative=%d)", healed, vn.Status.DriftHealsTotal))
	} else {
		setCondition(&vn.Status.Conditions, vn.Generation, "DriftHealed", metav1.ConditionFalse, "InSync", "No out-of-band security group drift detected")
	}

	vn.Status.ObservedGeneration = vn.Generation
	vn.Status.Phase = "Ready"
	vn.Status.ActiveRuleCount = activeCount
	setCondition(&vn.Status.Conditions, vn.Generation, "Ready", metav1.ConditionTrue, "Reconciled",
		fmt.Sprintf("Neutron network %s (CIDR %s) and security group %s are ready", netID, vn.Spec.CIDR, sgID))

	if err := r.Status().Update(ctx, &vn); err != nil {
		outcome = "error"
		return ctrl.Result{}, err
	}

	opmetrics.ManagedResources.WithLabelValues("VirtualNetwork", vn.Namespace, vn.Name).Set(1)
	return ctrl.Result{RequeueAfter: r.requeueInterval()}, nil
}

func (r *VirtualNetworkReconciler) ensureNetwork(ctx context.Context, vn *iaasv1alpha1.VirtualNetwork) (string, error) {
	if vn.Status.NetworkID != "" {
		if _, err := networks.Get(ctx, r.Cloud.Neutron, vn.Status.NetworkID).Extract(); err == nil {
			return vn.Status.NetworkID, nil
		} else if !isOpenStackNotFound(err) {
			return "", err
		}
	}

	netName := vn.Name + "-net"
	pages, err := networks.List(r.Cloud.Neutron, networks.ListOpts{Name: netName}).AllPages(ctx)
	if err != nil {
		return "", err
	}
	existing, err := networks.ExtractNetworks(pages)
	if err != nil {
		return "", err
	}
	if len(existing) > 0 {
		return existing[0].ID, nil
	}

	created, err := networks.Create(ctx, r.Cloud.Neutron, networks.CreateOpts{
		Name:         netName,
		AdminStateUp: new(true),
	}).Extract()
	if err != nil {
		return "", err
	}
	emitEvent(r.Recorder, vn, corev1.EventTypeNormal, "NetworkCreated", "Created Neutron network %q (%s)", created.Name, created.ID)
	return created.ID, nil
}

func (r *VirtualNetworkReconciler) ensureSubnet(ctx context.Context, vn *iaasv1alpha1.VirtualNetwork, netID string) (string, error) {
	if vn.Status.SubnetID != "" {
		if _, err := subnets.Get(ctx, r.Cloud.Neutron, vn.Status.SubnetID).Extract(); err == nil {
			return vn.Status.SubnetID, nil
		} else if !isOpenStackNotFound(err) {
			return "", err
		}
	}

	subName := vn.Name + "-subnet"
	pages, err := subnets.List(r.Cloud.Neutron, subnets.ListOpts{NetworkID: netID, Name: subName}).AllPages(ctx)
	if err != nil {
		return "", err
	}
	existing, err := subnets.ExtractSubnets(pages)
	if err != nil {
		return "", err
	}
	if len(existing) > 0 {
		return existing[0].ID, nil
	}

	enableDHCP := true
	if vn.Spec.EnableDHCP != nil {
		enableDHCP = *vn.Spec.EnableDHCP
	}
	opts := subnets.CreateOpts{
		Name:       subName,
		NetworkID:  netID,
		IPVersion:  gophercloud.IPv4,
		CIDR:       vn.Spec.CIDR,
		EnableDHCP: new(enableDHCP),
	}
	if vn.Spec.GatewayIP != "" {
		opts.GatewayIP = new(vn.Spec.GatewayIP)
	}
	created, err := subnets.Create(ctx, r.Cloud.Neutron, opts).Extract()
	if err != nil {
		return "", err
	}
	emitEvent(r.Recorder, vn, corev1.EventTypeNormal, "SubnetCreated", "Created Neutron subnet %q (%s)", created.Name, created.CIDR)
	return created.ID, nil
}

func (r *VirtualNetworkReconciler) ensureRouter(ctx context.Context, vn *iaasv1alpha1.VirtualNetwork, subID string) (string, error) {
	rtrID := vn.Status.RouterID
	if rtrID != "" {
		if _, err := routers.Get(ctx, r.Cloud.Neutron, rtrID).Extract(); err != nil {
			if !isOpenStackNotFound(err) {
				return "", err
			}
			rtrID = ""
		}
	}

	if rtrID == "" {
		rtrName := vn.Name + "-router"
		pages, err := routers.List(r.Cloud.Neutron, routers.ListOpts{Name: rtrName}).AllPages(ctx)
		if err != nil {
			return "", err
		}
		existing, err := routers.ExtractRouters(pages)
		if err != nil {
			return "", err
		}
		if len(existing) > 0 {
			rtrID = existing[0].ID
		} else {
			created, err := routers.Create(ctx, r.Cloud.Neutron, routers.CreateOpts{
				Name:         rtrName,
				AdminStateUp: new(true),
			}).Extract()
			if err != nil {
				return "", err
			}
			rtrID = created.ID
			emitEvent(r.Recorder, vn, corev1.EventTypeNormal, "RouterCreated", "Created Neutron L3 router %q (%s)", created.Name, created.ID)
		}
	}

	// Ensure subnet is attached (409 Conflict indicates interface already exists).
	if _, err := routers.AddInterface(ctx, r.Cloud.Neutron, rtrID, routers.AddInterfaceOpts{SubnetID: subID}).Extract(); err != nil {
		if !gophercloud.ResponseCodeIs(err, 409) {
			return "", err
		}
	}
	return rtrID, nil
}

func (r *VirtualNetworkReconciler) ensureSecurityGroup(ctx context.Context, vn *iaasv1alpha1.VirtualNetwork) (string, error) {
	if vn.Status.SecurityGroupID != "" {
		if _, err := groups.Get(ctx, r.Cloud.Neutron, vn.Status.SecurityGroupID).Extract(); err == nil {
			return vn.Status.SecurityGroupID, nil
		} else if !isOpenStackNotFound(err) {
			return "", err
		}
	}

	sgName := vn.Name + "-sg"
	pages, err := groups.List(r.Cloud.Neutron, groups.ListOpts{Name: sgName}).AllPages(ctx)
	if err != nil {
		return "", err
	}
	existing, err := groups.ExtractGroups(pages)
	if err != nil {
		return "", err
	}
	if len(existing) > 0 {
		return existing[0].ID, nil
	}

	created, err := groups.Create(ctx, r.Cloud.Neutron, groups.CreateOpts{
		Name:        sgName,
		Description: "Managed by cloud-operator for VirtualNetwork " + vn.Name,
	}).Extract()
	if err != nil {
		return "", err
	}
	return created.ID, nil
}

func (r *VirtualNetworkReconciler) reconcileSecurityRules(ctx context.Context, vn *iaasv1alpha1.VirtualNetwork, sgID string) (added, removed, total int, err error) {
	pages, err := rules.List(r.Cloud.Neutron, rules.ListOpts{SecGroupID: sgID}).AllPages(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	liveRules, err := rules.ExtractRules(pages)
	if err != nil {
		return 0, 0, 0, err
	}

	desired := make(map[ruleKey]iaasv1alpha1.SecurityRuleSpec, len(vn.Spec.SecurityRules))
	for _, spec := range vn.Spec.SecurityRules {
		k := ruleKey{
			Direction:  cmp.Or(spec.Direction, "ingress"),
			Protocol:   spec.Protocol,
			PortMin:    spec.PortMin,
			PortMax:    spec.PortMax,
			RemoteCIDR: spec.RemoteCIDR,
		}
		desired[k] = spec
	}

	liveByKey := make(map[ruleKey]rules.SecGroupRule, len(liveRules))
	for _, lr := range liveRules {
		k := ruleKey{
			Direction:  lr.Direction,
			Protocol:   lr.Protocol,
			PortMin:    lr.PortRangeMin,
			PortMax:    lr.PortRangeMax,
			RemoteCIDR: lr.RemoteIPPrefix,
		}
		// Leave Neutron's default broad egress allow rule intact unless restricted explicitly.
		if lr.Direction == "egress" && lr.Protocol == "" && lr.PortRangeMin == 0 && lr.PortRangeMax == 0 && lr.RemoteIPPrefix == "" {
			continue
		}
		if _, wanted := desired[k]; !wanted {
			// Remove unauthorized rule.
			if err := rules.Delete(ctx, r.Cloud.Neutron, lr.ID).ExtractErr(); err != nil && !isOpenStackNotFound(err) {
				return added, removed, 0, err
			}
			removed++
			continue
		}
		liveByKey[k] = lr
	}

	// Restore any desired rules missing from Neutron.
	for k, spec := range desired {
		if _, exists := liveByKey[k]; exists {
			continue
		}
		_, err := rules.Create(ctx, r.Cloud.Neutron, rules.CreateOpts{
			Direction:      rules.RuleDirection(k.Direction),
			EtherType:      rules.EtherType4,
			SecGroupID:     sgID,
			Protocol:       rules.RuleProtocol(k.Protocol),
			PortRangeMin:   k.PortMin,
			PortRangeMax:   k.PortMax,
			RemoteIPPrefix: k.RemoteCIDR,
			Description:    spec.Description,
		}).Extract()
		if err != nil {
			return added, removed, 0, err
		}
		added++
	}

	return added, removed, len(desired), nil
}

func (r *VirtualNetworkReconciler) cleanupNeutron(ctx context.Context, vn *iaasv1alpha1.VirtualNetwork) error {
	if vn.Status.RouterID != "" {
		if vn.Status.SubnetID != "" {
			if _, err := routers.RemoveInterface(ctx, r.Cloud.Neutron, vn.Status.RouterID, routers.RemoveInterfaceOpts{SubnetID: vn.Status.SubnetID}).Extract(); err != nil && !isOpenStackNotFound(err) {
				return fmt.Errorf("remove router interface: %w", err)
			}
		}
		if err := routers.Delete(ctx, r.Cloud.Neutron, vn.Status.RouterID).ExtractErr(); err != nil && !isOpenStackNotFound(err) {
			return fmt.Errorf("delete router: %w", err)
		}
	}
	if vn.Status.SecurityGroupID != "" {
		if err := groups.Delete(ctx, r.Cloud.Neutron, vn.Status.SecurityGroupID).ExtractErr(); err != nil && !isOpenStackNotFound(err) {
			return fmt.Errorf("delete security group: %w", err)
		}
	}
	if vn.Status.SubnetID != "" {
		if err := subnets.Delete(ctx, r.Cloud.Neutron, vn.Status.SubnetID).ExtractErr(); err != nil && !isOpenStackNotFound(err) {
			return fmt.Errorf("delete subnet: %w", err)
		}
	}
	if vn.Status.NetworkID != "" {
		if err := networks.Delete(ctx, r.Cloud.Neutron, vn.Status.NetworkID).ExtractErr(); err != nil && !isOpenStackNotFound(err) {
			return fmt.Errorf("delete network: %w", err)
		}
	}
	return nil
}

func (r *VirtualNetworkReconciler) markFailed(ctx context.Context, vn *iaasv1alpha1.VirtualNetwork, reason string, err error) {
	vn.Status.Phase = "Degraded"
	setCondition(&vn.Status.Conditions, vn.Generation, "Ready", metav1.ConditionFalse, reason, err.Error())
	_ = r.Status().Update(ctx, vn)
	opmetrics.ManagedResources.WithLabelValues("VirtualNetwork", vn.Namespace, vn.Name).Set(0)
}

// SetupWithManager registers the VirtualNetwork controller with the manager.
func (r *VirtualNetworkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&iaasv1alpha1.VirtualNetwork{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}
