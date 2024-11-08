/*
 * Copyright 2023 The Kubernetes Authors.
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
 */

package main

import (
	"fmt"
	resourceapi "k8s.io/api/resource/v1alpha3"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

func enumerateAllCPUDevices() (AllocatableDevices, error) {
	alldevices := make(AllocatableDevices)

	numCores := int64(16)
	numThreads := int64(2)
	for thread := int64(0); thread < numThreads*numCores; thread++ {
		var core, numaNode int64

		if thread < 16 {
			core = thread % 16
		} else {
			core = thread - 16
		}

		if core < 8 {
			numaNode = 0
		} else {
			numaNode = 1
		}

		fmt.Printf("id: %d\nparentID: %d\nnumaNode: %d\n\n", thread, core, numaNode)
		device := resourceapi.Device{
			Name: fmt.Sprintf("cpu-%d", thread),
			Basic: &resourceapi.BasicDevice{
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"architecture": {
						StringValue: ptr.To("amd64"),
					},
					"id": {
						IntValue: ptr.To(thread),
					},
					"parentID": {
						IntValue: ptr.To(core),
					},
					"dra.nvidia.com/numa": {
						IntValue: ptr.To(numaNode),
					},
					"driverVersion": {
						VersionValue: ptr.To("1.0.0"),
					},
				},
				Capacity: map[resourceapi.QualifiedName]resource.Quantity{
					"memory": resource.MustParse("80Gi"),
				},
			},
		}
		alldevices[device.Name] = device
	}
	return alldevices, nil
}
