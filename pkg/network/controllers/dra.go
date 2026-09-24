/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright The KubeVirt Authors.
 *
 */

package controllers

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	resourceclient "k8s.io/client-go/kubernetes/typed/resource/v1"
	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/kubevirt/pkg/network/vmispec"
)

type networkBindingConfig interface {
	GetNetworkBindings() map[string]v1.InterfaceBindingPlugin
}

// NewStatusUpdater resolves managed TAP DRA interfaces before handing the VMI to
// virt-handler. Other DRA bindings remain responsible for their own discovery.
func NewStatusUpdater(client resourceclient.ResourceV1Interface, config networkBindingConfig) func(*v1.VirtualMachineInstance, *corev1.Pod) error {
	return func(vmi *v1.VirtualMachineInstance, pod *corev1.Pod) error {
		if err := UpdateVMIStatus(vmi, pod); err != nil {
			return err
		}
		if vmi.IsRunning() {
			return nil
		}
		bindings := config.GetNetworkBindings()
		claims := map[string]*resourcev1.ResourceClaim{}
		for _, network := range vmi.Spec.Networks {
			iface := vmispec.LookupInterfaceByName(vmi.Spec.Domain.Devices.Interfaces, network.Name)
			if network.ResourceClaim == nil || iface == nil || iface.State == v1.InterfaceStateAbsent || iface.Binding == nil || bindings[iface.Binding.Name].DomainAttachmentType != v1.ManagedTap {
				continue
			}
			claimName := podClaimName(pod, network.ResourceClaim.ClaimName)
			if claimName == "" {
				return fmt.Errorf("DRA network %q: waiting for resource claim name", network.Name)
			}
			claim := claims[claimName]
			if claim == nil {
				var err error
				claim, err = client.ResourceClaims(pod.Namespace).Get(context.Background(), claimName, metav1.GetOptions{})
				if err != nil {
					return fmt.Errorf("DRA network %q: %w", network.Name, err)
				}
				claims[claimName] = claim
			}
			name, err := allocatedInterfaceName(claim, network.ResourceClaim.RequestName)
			if err != nil {
				return fmt.Errorf("DRA network %q: %w", network.Name, err)
			}
			for _, status := range vmi.Status.Interfaces {
				if status.Name != network.Name && status.PodInterfaceName == name {
					return fmt.Errorf("DRA network %q: pod interface %q is already used by network %q", network.Name, name, status.Name)
				}
			}
			status := vmispec.LookupInterfaceStatusByName(vmi.Status.Interfaces, network.Name)
			if status == nil {
				vmi.Status.Interfaces = append(vmi.Status.Interfaces, v1.VirtualMachineInstanceNetworkInterface{Name: network.Name, PodInterfaceName: name})
			} else {
				status.PodInterfaceName = name
			}
		}
		return nil
	}
}

func podClaimName(pod *corev1.Pod, name string) string {
	for _, claim := range pod.Spec.ResourceClaims {
		if claim.Name != name {
			continue
		}
		if claim.ResourceClaimName != nil {
			return *claim.ResourceClaimName
		}
		if claim.ResourceClaimTemplateName != nil {
			for _, status := range pod.Status.ResourceClaimStatuses {
				if status.Name == name && status.ResourceClaimName != nil {
					return *status.ResourceClaimName
				}
			}
		}
	}
	return ""
}

func allocatedInterfaceName(claim *resourcev1.ResourceClaim, request string) (string, error) {
	if claim.Status.Allocation == nil {
		return "", fmt.Errorf("waiting for claim %q allocation", claim.Name)
	}
	var device *resourcev1.DeviceRequestAllocationResult
	for i := range claim.Status.Allocation.Devices.Results {
		result := &claim.Status.Allocation.Devices.Results[i]
		if result.Request != request && !strings.HasPrefix(result.Request, request+"/") {
			continue
		}
		if device != nil {
			return "", fmt.Errorf("request %q must allocate exactly one device for managedTap", request)
		}
		device = result
	}
	if device == nil {
		return "", fmt.Errorf("waiting for request %q allocation", request)
	}
	for _, status := range claim.Status.Devices {
		if status.Driver != device.Driver || status.Pool != device.Pool || status.Device != device.Device {
			continue
		}
		if status.NetworkData == nil || status.NetworkData.InterfaceName == "" {
			break
		}
		name := status.NetworkData.InterfaceName
		// managedTap appends "-nic" to the original link name (Linux limit: 15 bytes).
		if len(name) > 11 || name == "." || name == ".." || name == "lo" || strings.ContainsAny(name, "/: \t\n") {
			return "", fmt.Errorf("interface name %q cannot be used with managedTap", name)
		}
		return name, nil
	}
	return "", fmt.Errorf("waiting for request %q networkData.interfaceName", request)
}
