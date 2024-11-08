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
	"strings"
	"sync"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	resourceapi "k8s.io/api/resource/v1alpha3"
	"k8s.io/apimachinery/pkg/runtime"
	drapbv1 "k8s.io/kubelet/pkg/apis/dra/v1alpha4"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"

	configapi "example-numa-aware-cpu-driver/api/nvidia.com/resource/cpu/v1alpha1"

	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
)

type AllocatableDevices map[string]resourceapi.Device
type PreparedDevices []*PreparedDevice
type PreparedClaims map[string]PreparedDevices

type OpaqueDeviceConfig struct {
	Requests []string
	Config   runtime.Object
}

type PreparedDevice struct {
	drapbv1.Device
	ContainerEdits *cdiapi.ContainerEdits
}

func (pds PreparedDevices) GetDevices() []*drapbv1.Device {
	var devices []*drapbv1.Device
	for _, pd := range pds {
		devices = append(devices, &pd.Device)
	}
	return devices
}

type DeviceState struct {
	sync.Mutex
	cdi               *CDIHandler
	allocatable       AllocatableDevices
	checkpointManager checkpointmanager.CheckpointManager
}

func NewDeviceState(config *Config) (*DeviceState, error) {
	allocatable, err := enumerateAllCPUDevices()
	if err != nil {
		return nil, fmt.Errorf("error enumerating all possible devices: %v", err)
	}

	cdi, err := NewCDIHandler(config)
	if err != nil {
		return nil, fmt.Errorf("unable to create CDI handler: %v", err)
	}

	err = cdi.CreateCommonSpecFile()
	if err != nil {
		return nil, fmt.Errorf("unable to create CDI spec file for common edits: %v", err)
	}

	checkpointManager, err := checkpointmanager.NewCheckpointManager(DriverPluginPath)
	if err != nil {
		return nil, fmt.Errorf("unable to create checkpoint manager: %v", err)
	}

	state := &DeviceState{
		cdi:               cdi,
		allocatable:       allocatable,
		checkpointManager: checkpointManager,
	}

	checkpoints, err := state.checkpointManager.ListCheckpoints()
	if err != nil {
		return nil, fmt.Errorf("unable to list checkpoints: %v", err)
	}

	for _, c := range checkpoints {
		if c == DriverPluginCheckpointFile {
			return state, nil
		}
	}

	checkpoint := newCheckpoint()
	if err := state.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return nil, fmt.Errorf("unable to sync to checkpoint: %v", err)
	}

	return state, nil
}

func (s *DeviceState) Prepare(claim *resourceapi.ResourceClaim) ([]*drapbv1.Device, error) {
	s.Lock()
	defer s.Unlock()

	claimUID := string(claim.UID)

	checkpoint := newCheckpoint()
	if err := s.checkpointManager.GetCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return nil, fmt.Errorf("unable to sync from checkpoint: %v", err)
	}
	preparedClaims := checkpoint.V1.PreparedClaims

	if preparedClaims[claimUID] != nil {
		return preparedClaims[claimUID].GetDevices(), nil
	}

	preparedDevices, containerEdit, err := s.prepareDevices(claim)
	if err != nil {
		return nil, fmt.Errorf("prepare failed: %v", err)
	}

	if err = s.cdi.CreateClaimSpecFile(claimUID, preparedDevices, *containerEdit); err != nil {
		return nil, fmt.Errorf("unable to create CDI spec file for claim: %v", err)
	}

	preparedClaims[claimUID] = preparedDevices
	if err := s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return nil, fmt.Errorf("unable to sync to checkpoint: %v", err)
	}

	return preparedClaims[claimUID].GetDevices(), nil
}

func (s *DeviceState) Unprepare(claimUID string) error {
	s.Lock()
	defer s.Unlock()

	checkpoint := newCheckpoint()
	if err := s.checkpointManager.GetCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return fmt.Errorf("unable to sync from checkpoint: %v", err)
	}
	preparedClaims := checkpoint.V1.PreparedClaims

	if preparedClaims[claimUID] == nil {
		return nil
	}

	if err := s.unprepareDevices(claimUID, preparedClaims[claimUID]); err != nil {
		return fmt.Errorf("unprepare failed: %v", err)
	}

	err := s.cdi.DeleteClaimSpecFile(claimUID)
	if err != nil {
		return fmt.Errorf("unable to delete CDI spec file for claim: %v", err)
	}

	delete(preparedClaims, claimUID)
	if err := s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return fmt.Errorf("unable to sync to checkpoint: %v", err)
	}

	return nil
}

func (s *DeviceState) prepareDevices(claim *resourceapi.ResourceClaim) (PreparedDevices, *cdispec.ContainerEdits, error) {
	if claim.Status.Allocation == nil {
		return nil, nil, fmt.Errorf("claim not yet allocated")
	}

	driverResults := s.filterDevicesForDriver(claim.Status.Allocation.Devices.Results, configapi.GroupName)

	containerEdit, err := s.applyConfig(claim, driverResults)
	if err != nil {
		return nil, nil, fmt.Errorf("error applying CPU config: %w", err)
	}

	var preparedDevices PreparedDevices
	for _, result := range driverResults {
		device := &PreparedDevice{
			Device: drapbv1.Device{
				RequestNames: []string{result.Request},
				PoolName:     result.Pool,
				DeviceName:   result.Device,
				CDIDeviceIDs: s.cdi.GetClaimDevices(string(claim.UID), []string{result.Device}),
			},
		}
		preparedDevices = append(preparedDevices, device)
	}

	return preparedDevices, containerEdit, nil
}

func (s *DeviceState) filterDevicesForDriver(statusDevices []resourceapi.DeviceRequestAllocationResult, driverName string) []resourceapi.DeviceRequestAllocationResult {
	driverFilteredDevices := []resourceapi.DeviceRequestAllocationResult{}
	for _, device := range statusDevices {
		if device.Driver == driverName {
			driverFilteredDevices = append(driverFilteredDevices, device)
		}
	}
	return driverFilteredDevices
}

func (s *DeviceState) unprepareDevices(claimUID string, devices PreparedDevices) error {
	return nil
}

// applyConfig applies a configuration to a set of device allocation results.
//
// In this example driver there is no actual configuration applied. We simply
// define a set of environment variables to be injected into the containers
// that include a given device. A real driver would likely need to do some sort
// of hardware configuration as well, based on the config passed in.
func (s *DeviceState) applyConfig(claim *resourceapi.ResourceClaim, results []resourceapi.DeviceRequestAllocationResult) (*cdispec.ContainerEdits, error) {
	threadIDs := []string{}
	for _, result := range results {
		for _, a := range s.allocatable {
			if result.Device == a.Name {
				threadIDs = append(threadIDs, fmt.Sprintf("%d", *a.Basic.Attributes["id"].IntValue))
			}
		}
	}

	envs := []string{
		fmt.Sprintf("CPU_%s=%s", claim.Name, strings.Join(threadIDs, ",")),
	}

	return &cdispec.ContainerEdits{
		Env: envs,
	}, nil
}
