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
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	resourceclient "k8s.io/client-go/kubernetes/typed/resource/v1"
	v1 "kubevirt.io/api/core/v1"
	"strings"
	"testing"
)

func testClaim() *resourcev1.ResourceClaim {
	return &resourcev1.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Name: "generated"}, Status: resourcev1.ResourceClaimStatus{
		Allocation: &resourcev1.AllocationResult{Devices: resourcev1.DeviceAllocationResult{Results: []resourcev1.DeviceRequestAllocationResult{
			{Request: "net", Driver: "example.com", Pool: "node", Device: "dev"},
		}}},
		Devices: []resourcev1.AllocatedDeviceStatus{
			{Driver: "other.com", Pool: "node", Device: "dev", NetworkData: &resourcev1.NetworkDeviceData{InterfaceName: "wrong"}},
			{Driver: "example.com", Pool: "node", Device: "dev", NetworkData: &resourcev1.NetworkDeviceData{InterfaceName: "net1"}},
		},
	}}
}

func TestAllocatedInterfaceName(t *testing.T) {
	for _, tc := range []struct {
		name            string
		change          func(*resourcev1.ResourceClaim)
		want, errorText string
	}{
		{name: "matches allocation tuple", want: "net1"},
		{name: "subrequest", change: func(c *resourcev1.ResourceClaim) { c.Status.Allocation.Devices.Results[0].Request = "net/first" }, want: "net1"},
		{name: "unallocated", change: func(c *resourcev1.ResourceClaim) { c.Status.Allocation = nil }, errorText: "waiting"},
		{name: "missing status", change: func(c *resourcev1.ResourceClaim) { c.Status.Devices = nil }, errorText: "networkData.interfaceName"},
		{name: "wrong request", change: func(c *resourcev1.ResourceClaim) { c.Status.Allocation.Devices.Results[0].Request = "network" }, errorText: "waiting"},
		{name: "multiple allocations", change: func(c *resourcev1.ResourceClaim) {
			c.Status.Allocation.Devices.Results = append(c.Status.Allocation.Devices.Results, c.Status.Allocation.Devices.Results[0])
		}, errorText: "exactly one"},
		{name: "long interface", change: func(c *resourcev1.ResourceClaim) { c.Status.Devices[1].NetworkData.InterfaceName = "abcdefghijkl" }, errorText: "cannot be used"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClaim()
			if tc.change != nil {
				tc.change(c)
			}
			got, err := allocatedInterfaceName(c, "net")
			if tc.errorText != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errorText) {
					t.Fatalf("got %q, %v", got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
}

type claimClient struct {
	resourceclient.ResourceV1Interface
	claim     *resourcev1.ResourceClaim
	requested string
}

func (c *claimClient) ResourceClaims(namespace string) resourceclient.ResourceClaimInterface {
	return &claimGetter{client: c}
}

type claimGetter struct {
	resourceclient.ResourceClaimInterface
	client *claimClient
}

func (c *claimGetter) Get(_ context.Context, name string, _ metav1.GetOptions) (*resourcev1.ResourceClaim, error) {
	c.client.requested = name
	return c.client.claim, nil
}

type bindingConfig struct{}

func (bindingConfig) GetNetworkBindings() map[string]v1.InterfaceBindingPlugin {
	return map[string]v1.InterfaceBindingPlugin{"managed": {DomainAttachmentType: v1.ManagedTap}}
}

func TestDRAStatusUpdater(t *testing.T) {
	template, generated := "template", "generated"
	pod := &corev1.Pod{Spec: corev1.PodSpec{ResourceClaims: []corev1.PodResourceClaim{{Name: "network", ResourceClaimTemplateName: &template}}}, Status: corev1.PodStatus{ResourceClaimStatuses: []corev1.PodResourceClaimStatus{{Name: "network", ResourceClaimName: &generated}}}}
	vmi := &v1.VirtualMachineInstance{Spec: v1.VirtualMachineInstanceSpec{
		Networks: []v1.Network{{Name: "secondary", NetworkSource: v1.NetworkSource{ResourceClaim: &v1.ClaimRequest{ClaimName: "network", RequestName: "net"}}}},
		Domain:   v1.DomainSpec{Devices: v1.Devices{Interfaces: []v1.Interface{{Name: "secondary", Binding: &v1.PluginBinding{Name: "managed"}}}}},
	}}
	client := &claimClient{claim: testClaim()}
	update := NewStatusUpdater(client, bindingConfig{})
	client.claim.Status.Devices = nil
	if err := update(vmi, pod); err == nil {
		t.Fatal("must wait for device status")
	}
	if len(vmi.Status.Interfaces) != 0 {
		t.Fatal("published unresolved interface")
	}
	client.claim = testClaim()
	if err := update(vmi, pod); err != nil {
		t.Fatal(err)
	}
	if client.requested != generated || len(vmi.Status.Interfaces) != 1 || vmi.Status.Interfaces[0].PodInterfaceName != "net1" {
		t.Fatalf("unexpected resolution: %+v", vmi.Status.Interfaces)
	}
	if err := update(vmi, pod); err != nil {
		t.Fatal(err)
	}
	if len(vmi.Status.Interfaces) != 1 {
		t.Fatal("duplicate interface status")
	}
	// Fixed claims do not require generated claim status.
	pod.Spec.ResourceClaims[0] = corev1.PodResourceClaim{Name: "network", ResourceClaimName: &generated}
	pod.Status.ResourceClaimStatuses = nil
	if err := update(vmi, pod); err != nil {
		t.Fatal(err)
	}
	// After handoff, preserve the identity even if driver status changes.
	vmi.Status.Phase = v1.Running
	client.claim.Status.Devices = nil
	if err := update(vmi, pod); err != nil {
		t.Fatal(err)
	}
}
