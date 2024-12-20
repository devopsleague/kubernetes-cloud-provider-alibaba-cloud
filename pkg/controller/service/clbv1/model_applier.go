package clbv1

import (
	"context"
	"fmt"
	v1 "k8s.io/api/core/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrlCfg "k8s.io/cloud-provider-alibaba-cloud/pkg/config"
	"k8s.io/cloud-provider-alibaba-cloud/pkg/controller/helper"
	"k8s.io/cloud-provider-alibaba-cloud/pkg/controller/service/reconcile/annotation"
	svcCtx "k8s.io/cloud-provider-alibaba-cloud/pkg/controller/service/reconcile/context"
	"k8s.io/cloud-provider-alibaba-cloud/pkg/model"
	"k8s.io/cloud-provider-alibaba-cloud/pkg/model/tag"
	"k8s.io/cloud-provider-alibaba-cloud/pkg/provider/dryrun"
	"k8s.io/cloud-provider-alibaba-cloud/pkg/util"
	"k8s.io/klog/v2"
	"sort"
)

func NewModelApplier(slbMgr *LoadBalancerManager, lisMgr *ListenerManager, vGroupMgr *VGroupManager) *ModelApplier {
	return &ModelApplier{
		slbMgr:    slbMgr,
		lisMgr:    lisMgr,
		vGroupMgr: vGroupMgr,
	}
}

type ModelApplier struct {
	slbMgr    *LoadBalancerManager
	lisMgr    *ListenerManager
	vGroupMgr *VGroupManager
}

func (m *ModelApplier) Apply(reqCtx *svcCtx.RequestContext, local *model.LoadBalancer) (*model.LoadBalancer, error) {
	remote := &model.LoadBalancer{
		NamespacedName:                  util.NamespacedName(reqCtx.Service),
		ContainsPotentialReadyEndpoints: local.ContainsPotentialReadyEndpoints,
	}

	err := m.slbMgr.BuildRemoteModel(reqCtx, remote)
	if err != nil {
		return remote, fmt.Errorf("get load balancer attribute from cloud, error: %s", err.Error())
	}
	klog.Infof("%s find clb with result, reconcileID: %s\n%+v", util.Key(reqCtx.Service), reqCtx.ReconcileID, util.PrettyJson(remote))

	if remote.LoadBalancerAttribute.LoadBalancerId != "" && local.LoadBalancerAttribute.PreserveOnDelete {
		reqCtx.Recorder.Eventf(reqCtx.Service, v1.EventTypeWarning, helper.PreservedOnDelete,
			"The lb [%s] will be preserved after the service is deleted.", remote.LoadBalancerAttribute.LoadBalancerId)
	}

	serviceHashChanged := helper.IsServiceHashChanged(reqCtx.Service)
	errs := []error{}
	// apply sequence can not change, apply lb first, then vgroup, listener at last
	if serviceHashChanged || ctrlCfg.ControllerCFG.DryRun {
		if err := m.applyLoadBalancerAttribute(reqCtx, local, remote); err != nil {
			_, ok := err.(utilerrors.Aggregate)
			if ok {
				// if lb attr update failed, continue to sync vgroup & listener
				errs = append(errs, fmt.Errorf("update lb attribute error: %s", err.Error()))
			} else {
				return nil, err
			}
		}
	}

	if remote.LoadBalancerAttribute.LoadBalancerId == "" {
		// delete loadbalancer: return nil
		if helper.NeedDeleteLoadBalancer(reqCtx.Service) {
			return remote, nil
		}
		// update loadbalancer: return error
		errs = append(errs, fmt.Errorf("alicloud: can not find loadbalancer by tag [%s:%s]",
			helper.TAGKEY, reqCtx.Anno.GetDefaultLoadBalancerName()))
		return remote, utilerrors.NewAggregate(errs)
	}
	reqCtx.Ctx = context.WithValue(reqCtx.Ctx, dryrun.ContextSLB, remote.LoadBalancerAttribute.LoadBalancerId)

	if err := m.vGroupMgr.BuildRemoteModel(reqCtx, remote); err != nil {
		errs = append(errs, fmt.Errorf("get lb backend from remote error: %s", err.Error()))
		return remote, utilerrors.NewAggregate(errs)
	}
	if err := m.applyVGroups(reqCtx, local, remote); err != nil {
		errs = append(errs, fmt.Errorf("update lb backends error: %s", err.Error()))
		return remote, utilerrors.NewAggregate(errs)
	}

	if serviceHashChanged || ctrlCfg.ControllerCFG.DryRun {
		if err := m.lisMgr.BuildRemoteModel(reqCtx, remote); err != nil {
			errs = append(errs, fmt.Errorf("get lb listeners from cloud, error: %s", err.Error()))
			return remote, utilerrors.NewAggregate(errs)
		}
		if err := m.applyListeners(reqCtx, local, remote); err != nil {
			errs = append(errs, fmt.Errorf("update lb listeners error: %s", err.Error()))
			return remote, utilerrors.NewAggregate(errs)
		}
	}

	if err := m.cleanup(reqCtx, local, remote); err != nil {
		errs = append(errs, fmt.Errorf("update lb listeners error: %s", err.Error()))
		return remote, utilerrors.NewAggregate(errs)
	}

	return remote, utilerrors.NewAggregate(errs)
}

func (m *ModelApplier) applyLoadBalancerAttribute(reqCtx *svcCtx.RequestContext, local *model.LoadBalancer, remote *model.LoadBalancer) error {
	if local == nil {
		return fmt.Errorf("local model is nil")
	}
	if remote == nil {
		return fmt.Errorf("remote model is nil")
	}
	if local.NamespacedName.String() != remote.NamespacedName.String() {
		return fmt.Errorf("models for different svc, local [%s], remote [%s]",
			local.NamespacedName, remote.NamespacedName)
	}

	// delete slb
	if helper.NeedDeleteLoadBalancer(reqCtx.Service) {
		if !local.LoadBalancerAttribute.IsUserManaged {
			if local.LoadBalancerAttribute.PreserveOnDelete {
				err := m.slbMgr.SetProtectionsOff(reqCtx, remote)
				if err != nil {
					return fmt.Errorf("set loadbalancer [%s] protections off error: %s",
						remote.LoadBalancerAttribute.LoadBalancerId, err.Error())
				}

				err = m.slbMgr.CleanupLoadBalancerTags(reqCtx, remote)
				if err != nil {
					return fmt.Errorf("cleanup loadbalancer [%s] tags error: %s",
						remote.LoadBalancerAttribute.LoadBalancerId, err.Error())
				}
				reqCtx.Log.Info(fmt.Sprintf("successfully cleanup preserved slb %s", remote.LoadBalancerAttribute.LoadBalancerId))
			} else {
				err := m.slbMgr.Delete(reqCtx, remote)
				if err != nil {
					return fmt.Errorf("delete loadbalancer [%s] error: %s",
						remote.LoadBalancerAttribute.LoadBalancerId, err.Error())
				}
				reqCtx.Log.Info(fmt.Sprintf("successfully delete slb %s", remote.LoadBalancerAttribute.LoadBalancerId))
			}
			remote.LoadBalancerAttribute.LoadBalancerId = ""
			remote.LoadBalancerAttribute.Address = ""
			return nil
		}

		reqCtx.Log.Info(fmt.Sprintf("slb %s is reused, skip delete it", remote.LoadBalancerAttribute.LoadBalancerId))
		return nil
	}

	// create slb
	if remote.LoadBalancerAttribute.LoadBalancerId == "" {
		if helper.IsServiceOwnIngress(reqCtx.Service) {
			return fmt.Errorf("alicloud: can not find loadbalancer, but it's defined in service [%v] "+
				"this may happen when you delete the loadbalancer", reqCtx.Service.Status.LoadBalancer.Ingress[0].IP)
		}

		if err := m.slbMgr.Create(reqCtx, local); err != nil {
			return fmt.Errorf("create lb error: %s", err.Error())
		}
		reqCtx.Log.Info(fmt.Sprintf("successfully create lb %s", local.LoadBalancerAttribute.LoadBalancerId))
		// update remote model
		remote.LoadBalancerAttribute.LoadBalancerId = local.LoadBalancerAttribute.LoadBalancerId
		if err := m.slbMgr.Find(reqCtx, remote); err != nil {
			return fmt.Errorf("update remote model for lbId %s, error: %s",
				remote.LoadBalancerAttribute.LoadBalancerId, err.Error())
		}
		return nil
	}

	tags, err := m.slbMgr.cloud.ListCLBTagResources(reqCtx.Ctx, remote.LoadBalancerAttribute.LoadBalancerId)
	if err != nil {
		return fmt.Errorf("DescribeTags: %s", err.Error())
	}
	remote.LoadBalancerAttribute.Tags = tags

	// check whether slb can be reused
	if !helper.NeedDeleteLoadBalancer(reqCtx.Service) && local.LoadBalancerAttribute.IsUserManaged {
		if ok, reason := isLoadBalancerReusable(reqCtx, tags, remote.LoadBalancerAttribute.Address); !ok {
			return fmt.Errorf("alicloud: the loadbalancer %s can not be reused, %s",
				remote.LoadBalancerAttribute.LoadBalancerId, reason)
		}
	}

	return m.slbMgr.Update(reqCtx, local, remote)

}

func (m *ModelApplier) applyVGroups(reqCtx *svcCtx.RequestContext, local *model.LoadBalancer, remote *model.LoadBalancer) error {
	var errs []error
	updatedVGroups := map[string]bool{}

	for i := range local.VServerGroups {
		found := false
		var old model.VServerGroup

		for _, rv := range remote.VServerGroups {
			// for reuse vgroup case, find by vgroup id first
			if local.VServerGroups[i].VGroupId != "" &&
				local.VServerGroups[i].VGroupId == rv.VGroupId {
				found = true
				old = rv
				break
			}
			// find by vgroup name
			if local.VServerGroups[i].VGroupId == "" &&
				local.VServerGroups[i].VGroupName == rv.VGroupName {
				found = true
				local.VServerGroups[i].VGroupId = rv.VGroupId
				old = rv
				break
			}
		}

		if updatedVGroups[local.VServerGroups[i].VGroupId] {
			reqCtx.Log.Info("already updated vgroup, skip",
				"vgroupID", local.VServerGroups[i].VGroupId, "vgroupName", local.VServerGroups[i].VGroupName)
			continue
		}

		// update
		if found {
			if err := m.vGroupMgr.UpdateVServerGroup(reqCtx, local.VServerGroups[i], old); err != nil {
				errs = append(errs, fmt.Errorf("EnsureVGroupUpdated error: %s", err.Error()))
				continue
			}
			updatedVGroups[local.VServerGroups[i].VGroupId] = true
		}

		// create
		if !found {
			reqCtx.Log.Info(fmt.Sprintf("try to create vgroup %s", local.VServerGroups[i].VGroupName))
			// to avoid add too many backends in one action, create vserver group with empty backends,
			// then use AddVServerGroupBackendServers to add backends
			err := m.vGroupMgr.CreateVServerGroup(reqCtx, &local.VServerGroups[i], remote.LoadBalancerAttribute.LoadBalancerId)
			if err != nil {
				errs = append(errs, fmt.Errorf("CreateVServerGroup error: %s", err.Error()))
				continue
			}
			if err := m.vGroupMgr.BatchAddVServerGroupBackendServers(reqCtx, local.VServerGroups[i],
				local.VServerGroups[i].Backends); err != nil {
				errs = append(errs, fmt.Errorf("BatchAddVServerGroupBackendServers error: %s", err.Error()))
				continue
			}
			remote.VServerGroups = append(remote.VServerGroups, local.VServerGroups[i])
			updatedVGroups[local.VServerGroups[i].VGroupId] = true
		}
	}

	return utilerrors.NewAggregate(errs)
}

func (m *ModelApplier) applyListeners(reqCtx *svcCtx.RequestContext, local *model.LoadBalancer, remote *model.LoadBalancer) error {
	if local.LoadBalancerAttribute.IsUserManaged {
		if !reqCtx.Anno.IsForceOverride() {
			reqCtx.Log.Info("listener override is false, skip reconcile listeners")
			return nil
		}
	}
	createActions, updateActions, deleteActions, err := buildActionsForListeners(reqCtx, local, remote)
	if err != nil {
		return fmt.Errorf("merge listener: %s", err.Error())
	}
	// make https come first.
	// ensure https listeners to be created first for http forward
	sort.SliceStable(
		createActions,
		func(i, j int) bool {
			return createActions[i].listener.Protocol == model.HTTPS
		},
	)
	// make https at last.
	// ensure https listeners to be deleted late for http forward
	sort.SliceStable(
		deleteActions,
		func(i, j int) bool {
			return deleteActions[i].listener.Protocol != model.HTTPS
		},
	)

	// Pls be careful of the sequence. deletion first, then addition, last update
	for _, action := range deleteActions {
		err := m.lisMgr.Delete(reqCtx, action)
		if err != nil {
			return fmt.Errorf("delete listener [%d] error: %s", action.listener.ListenerPort, err.Error())
		}
	}

	for _, action := range createActions {
		err := m.lisMgr.Create(reqCtx, action)
		if err != nil {
			return fmt.Errorf("create listener [%d] error: %s", action.listener.ListenerPort, err.Error())
		}
	}

	for _, action := range updateActions {
		err := m.lisMgr.Update(reqCtx, action)
		if err != nil {
			return fmt.Errorf("update listener [%d] error: %s", action.local.ListenerPort, err.Error())
		}
	}

	return nil
}

func (m *ModelApplier) cleanup(reqCtx *svcCtx.RequestContext, local *model.LoadBalancer, remote *model.LoadBalancer) error {
	// clean up vServerGroup
	vgs := remote.VServerGroups
	for _, vg := range vgs {
		found := false
		for _, l := range local.VServerGroups {
			if vg.VGroupId == l.VGroupId {
				found = true
				break
			}
		}

		// delete unused vgroup
		if !found {
			// do not delete user managed vgroup, but need to clean the backends in the vgroup
			if !isVGroupManagedByMyService(vg, reqCtx.Service) {
				reqCtx.Log.Info(fmt.Sprintf("try to delete vgroup: [%s] description [%s] is managed by user, skip delete",
					vg.VGroupName, vg.VGroupId))
				var del []model.BackendAttribute
				for _, remote := range vg.Backends {
					if !remote.IsUserManaged {
						del = append(del, remote)
					}
				}
				if len(del) > 0 {
					if err := m.vGroupMgr.BatchRemoveVServerGroupBackendServers(reqCtx, vg, del); err != nil {
						return err
					}
				}
				continue
			}

			reqCtx.Log.Info(fmt.Sprintf("delete vgroup: [%s], [%s]", vg.NamedKey.Key(), vg.VGroupId))
			err := m.vGroupMgr.DeleteVServerGroup(reqCtx, vg.VGroupId)
			if err != nil {
				return fmt.Errorf("lb [%s] delete vgroup %s failed, error: %s",
					remote.LoadBalancerAttribute.LoadBalancerId, vg.VGroupId, err.Error())
			}
		}
	}
	return nil
}

func isLoadBalancerReusable(reqCtx *svcCtx.RequestContext, tags []tag.Tag, lbIp string) (bool, string) {
	for _, tag := range tags {
		// the tag of the apiserver slb is "ack.aliyun.com": "${clusterid}",
		// so can not reuse slbs which have ack.aliyun.com tag key.
		if tag.Key == helper.TAGKEY || tag.Key == util.ClusterTagKey {
			return false, "can not reuse loadbalancer created by kubernetes."
		}
	}

	// if use eip as externalIPType, ingress IP is eip, skip to check
	if reqCtx.Anno.Get(annotation.ExternalIPType) == "eip" {
		return true, ""
	}

	service := reqCtx.Service
	if len(service.Status.LoadBalancer.Ingress) > 0 {
		found := false
		for _, ingress := range service.Status.LoadBalancer.Ingress {
			if ingress.IP == lbIp || (ingress.Hostname != "" && ingress.IP == "") {
				found = true
			}
		}
		if !found {
			return false, fmt.Sprintf("service has been associated with ip [%v], cannot be bound to ip [%s]",
				service.Status.LoadBalancer.Ingress[0].IP, lbIp)
		}
	}

	return true, ""
}
