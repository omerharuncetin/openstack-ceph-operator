package v1alpha1

import (
	"slices"

	"k8s.io/apimachinery/pkg/runtime"
)

func (in *TenantStoragePoolSpec) DeepCopyInto(out *TenantStoragePoolSpec) {
	*out = *in
}

func (in *TenantStoragePoolStatus) DeepCopyInto(out *TenantStoragePoolStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = slices.Clone(in.Conditions)
	}
}

func (in *TenantStoragePool) DeepCopyInto(out *TenantStoragePool) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *TenantStoragePool) DeepCopy() *TenantStoragePool {
	if in == nil {
		return nil
	}
	out := new(TenantStoragePool)
	in.DeepCopyInto(out)
	return out
}

func (in *TenantStoragePool) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *TenantStoragePoolList) DeepCopyInto(out *TenantStoragePoolList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]TenantStoragePool, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *TenantStoragePoolList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(TenantStoragePoolList)
	in.DeepCopyInto(out)
	return out
}

func (in *VirtualNetworkSpec) DeepCopyInto(out *VirtualNetworkSpec) {
	*out = *in
	if in.EnableDHCP != nil {
		v := *in.EnableDHCP
		out.EnableDHCP = &v
	}
	if in.SecurityRules != nil {
		out.SecurityRules = slices.Clone(in.SecurityRules)
	}
}

func (in *VirtualNetworkStatus) DeepCopyInto(out *VirtualNetworkStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = slices.Clone(in.Conditions)
	}
}

func (in *VirtualNetwork) DeepCopyInto(out *VirtualNetwork) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *VirtualNetwork) DeepCopy() *VirtualNetwork {
	if in == nil {
		return nil
	}
	out := new(VirtualNetwork)
	in.DeepCopyInto(out)
	return out
}

func (in *VirtualNetwork) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *VirtualNetworkList) DeepCopyInto(out *VirtualNetworkList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]VirtualNetwork, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *VirtualNetworkList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(VirtualNetworkList)
	in.DeepCopyInto(out)
	return out
}

func (in *CloudWorkspaceSpec) DeepCopyInto(out *CloudWorkspaceSpec) {
	*out = *in
}

func (in *CloudWorkspaceStatus) DeepCopyInto(out *CloudWorkspaceStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = slices.Clone(in.Conditions)
	}
}

func (in *CloudWorkspace) DeepCopyInto(out *CloudWorkspace) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *CloudWorkspace) DeepCopy() *CloudWorkspace {
	if in == nil {
		return nil
	}
	out := new(CloudWorkspace)
	in.DeepCopyInto(out)
	return out
}

func (in *CloudWorkspace) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *CloudWorkspaceList) DeepCopyInto(out *CloudWorkspaceList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]CloudWorkspace, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *CloudWorkspaceList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(CloudWorkspaceList)
	in.DeepCopyInto(out)
	return out
}
