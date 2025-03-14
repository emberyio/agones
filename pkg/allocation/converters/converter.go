// Copyright 2019 Google LLC All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package converters includes API conversions between GameServerAllocation API and the Allocation proto APIs.
package converters

import (
	pb "agones.dev/agones/pkg/allocation/go"
	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
	"agones.dev/agones/pkg/util/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConvertAllocationRequestToGSA converts AllocationRequest to GameServerAllocation V1 (GSA)
func ConvertAllocationRequestToGSA(in *pb.AllocationRequest) *allocationv1.GameServerAllocation {
	if in == nil {
		return nil
	}

	gsa := &allocationv1.GameServerAllocation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: in.GetNamespace(),
		},
		Spec: allocationv1.GameServerAllocationSpec{
			// nolint:staticcheck
			Preferred:  convertGameServerSelectorsToInternalGameServerSelectors(in.GetPreferredGameServerSelectors()),
			Selectors:  convertGameServerSelectorsToInternalGameServerSelectors(in.GetGameServerSelectors()),
			Scheduling: convertAllocationSchedulingToGSASchedulingStrategy(in.GetScheduling()),
		},
	}

	if in.GetMultiClusterSetting() != nil {
		gsa.Spec.MultiClusterSetting = allocationv1.MultiClusterSetting{
			Enabled: in.GetMultiClusterSetting().GetEnabled(),
		}
		if ls := convertLabelSelectorToInternalLabelSelector(in.GetMultiClusterSetting().GetPolicySelector()); ls != nil {
			gsa.Spec.MultiClusterSetting.PolicySelector = *ls
		}
	}

	// Accept both metadata (preferred) and metapatch until metapatch is fully removed.
	metadata := in.GetMetadata()
	if metadata == nil {
		metadata = in.GetMetaPatch()
	}

	if metadata != nil {
		gsa.Spec.MetaPatch = allocationv1.MetaPatch{
			Labels:      metadata.GetLabels(),
			Annotations: metadata.GetAnnotations(),
		}
	}

	// nolint:staticcheck
	if selector := convertGameServerSelectorToInternalGameServerSelector(in.GetRequiredGameServerSelector()); selector != nil {
		// nolint:staticcheck
		gsa.Spec.Required = *selector
	}

	if runtime.FeatureEnabled(runtime.FeatureCountsAndLists) && in.Priorities != nil {
		gsa.Spec.Priorities = convertAllocationPrioritiesToGSAPriorities(in.GetPriorities())
	}

	return gsa
}

// ConvertGSAToAllocationRequest converts AllocationRequest to GameServerAllocation V1 (GSA)
func ConvertGSAToAllocationRequest(in *allocationv1.GameServerAllocation) *pb.AllocationRequest {
	if in == nil {
		return nil
	}

	out := &pb.AllocationRequest{
		Namespace:           in.GetNamespace(),
		Scheduling:          convertGSASchedulingStrategyToAllocationScheduling(in.Spec.Scheduling),
		GameServerSelectors: convertInternalLabelSelectorsToLabelSelectors(in.Spec.Selectors),
		MultiClusterSetting: &pb.MultiClusterSetting{
			Enabled: in.Spec.MultiClusterSetting.Enabled,
		},
		Metadata: &pb.MetaPatch{
			Labels:      in.Spec.MetaPatch.Labels,
			Annotations: in.Spec.MetaPatch.Annotations,
		},
		// MetaPatch is deprecated, but we do a double write here to both metapatch and metadata
		// to ensure that multi-cluster allocation still works when one cluster has the field
		// and another one does not have the field yet.
		MetaPatch: &pb.MetaPatch{
			Labels:      in.Spec.MetaPatch.Labels,
			Annotations: in.Spec.MetaPatch.Annotations,
		},
	}

	l := len(out.GameServerSelectors)
	if l > 0 {
		// nolint:staticcheck
		// Sets all but the last GameServerSelector as PreferredGameServerSelectors
		out.PreferredGameServerSelectors = out.GameServerSelectors[:l-1]
		// nolint:staticcheck
		// Sets the last GameServerSelector as RequiredGameServerSelector
		out.RequiredGameServerSelector = out.GameServerSelectors[l-1]
	}

	if in.Spec.MultiClusterSetting.Enabled {
		out.MultiClusterSetting.PolicySelector = convertInternalLabelSelectorToLabelSelector(&in.Spec.MultiClusterSetting.PolicySelector)
	}

	if runtime.FeatureEnabled(runtime.FeatureCountsAndLists) && in.Spec.Priorities != nil {
		out.Priorities = convertGSAPrioritiesToAllocationPriorities(in.Spec.Priorities)
	}

	return out
}

// convertAllocationSchedulingToGSASchedulingStrategy converts AllocationRequest_SchedulingStrategy to apis.SchedulingStrategy
func convertAllocationSchedulingToGSASchedulingStrategy(in pb.AllocationRequest_SchedulingStrategy) apis.SchedulingStrategy {
	switch in {
	case pb.AllocationRequest_Packed:
		return apis.Packed
	case pb.AllocationRequest_Distributed:
		return apis.Distributed
	}
	return apis.Packed
}

// convertGSASchedulingStrategyToAllocationScheduling converts  apis.SchedulingStrategy to pb.AllocationRequest_SchedulingStrategy
func convertGSASchedulingStrategyToAllocationScheduling(in apis.SchedulingStrategy) pb.AllocationRequest_SchedulingStrategy {
	switch in {
	case apis.Packed:
		return pb.AllocationRequest_Packed
	case apis.Distributed:
		return pb.AllocationRequest_Distributed
	}
	return pb.AllocationRequest_Packed
}

func convertLabelSelectorToInternalLabelSelector(in *pb.LabelSelector) *metav1.LabelSelector {
	if in == nil {
		return nil
	}
	return &metav1.LabelSelector{MatchLabels: in.GetMatchLabels()}
}

func convertGameServerSelectorToInternalGameServerSelector(in *pb.GameServerSelector) *allocationv1.GameServerSelector {
	if in == nil {
		return nil
	}
	result := &allocationv1.GameServerSelector{
		LabelSelector: metav1.LabelSelector{MatchLabels: in.GetMatchLabels()},
	}

	if runtime.FeatureEnabled(runtime.FeatureStateAllocationFilter) {
		switch in.GameServerState {
		case pb.GameServerSelector_ALLOCATED:
			allocated := agonesv1.GameServerStateAllocated
			result.GameServerState = &allocated
		case pb.GameServerSelector_READY:
			ready := agonesv1.GameServerStateReady
			result.GameServerState = &ready
		}
	}

	if runtime.FeatureEnabled(runtime.FeaturePlayerAllocationFilter) && in.Players != nil {
		result.Players = &allocationv1.PlayerSelector{
			MinAvailable: int64(in.Players.MinAvailable),
			MaxAvailable: int64(in.Players.MaxAvailable),
		}
	}

	if runtime.FeatureEnabled(runtime.FeatureCountsAndLists) {
		if in.Counters != nil {
			result.Counters = map[string]allocationv1.CounterSelector{}
			for k, v := range in.GetCounters() {
				result.Counters[k] = allocationv1.CounterSelector{
					MinCount:     v.MinCount,
					MaxCount:     v.MaxCount,
					MinAvailable: v.MinAvailable,
					MaxAvailable: v.MaxAvailable,
				}
			}
		}
		if in.Lists != nil {
			result.Lists = map[string]allocationv1.ListSelector{}
			for k, v := range in.GetLists() {
				result.Lists[k] = allocationv1.ListSelector{
					ContainsValue: v.ContainsValue,
					MinAvailable:  v.MinAvailable,
					MaxAvailable:  v.MaxAvailable,
				}
			}
		}
	}

	return result
}

func convertInternalGameServerSelectorToGameServer(in *allocationv1.GameServerSelector) *pb.GameServerSelector {
	if in == nil {
		return nil
	}
	result := &pb.GameServerSelector{
		MatchLabels: in.MatchLabels,
	}

	if runtime.FeatureEnabled(runtime.FeatureStateAllocationFilter) && in.GameServerState != nil {
		switch *in.GameServerState {
		case agonesv1.GameServerStateReady:
			result.GameServerState = pb.GameServerSelector_READY
		case agonesv1.GameServerStateAllocated:
			result.GameServerState = pb.GameServerSelector_ALLOCATED
		}
	}

	if runtime.FeatureEnabled(runtime.FeaturePlayerAllocationFilter) && in.Players != nil {
		result.Players = &pb.PlayerSelector{
			MinAvailable: uint64(in.Players.MinAvailable),
			MaxAvailable: uint64(in.Players.MaxAvailable),
		}
	}

	if runtime.FeatureEnabled(runtime.FeatureCountsAndLists) {
		if in.Counters != nil {
			result.Counters = map[string]*pb.CounterSelector{}
			for k, v := range in.Counters {
				result.Counters[k] = &pb.CounterSelector{
					MinCount:     v.MinCount,
					MaxCount:     v.MaxCount,
					MinAvailable: v.MinAvailable,
					MaxAvailable: v.MaxAvailable,
				}
			}
		}
		if in.Lists != nil {
			result.Lists = map[string]*pb.ListSelector{}
			for k, v := range in.Lists {
				result.Lists[k] = &pb.ListSelector{
					ContainsValue: v.ContainsValue,
					MinAvailable:  v.MinAvailable,
					MaxAvailable:  v.MaxAvailable,
				}
			}
		}
	}

	return result
}

func convertInternalLabelSelectorToLabelSelector(in *metav1.LabelSelector) *pb.LabelSelector {
	if in == nil {
		return nil
	}
	return &pb.LabelSelector{MatchLabels: in.MatchLabels}
}

func convertInternalLabelSelectorsToLabelSelectors(in []allocationv1.GameServerSelector) []*pb.GameServerSelector {
	var result []*pb.GameServerSelector
	for _, l := range in {
		l := l
		c := convertInternalGameServerSelectorToGameServer(&l)
		result = append(result, c)
	}
	return result
}

func convertGameServerSelectorsToInternalGameServerSelectors(in []*pb.GameServerSelector) []allocationv1.GameServerSelector {
	var result []allocationv1.GameServerSelector
	for _, l := range in {
		if selector := convertGameServerSelectorToInternalGameServerSelector(l); selector != nil {
			result = append(result, *selector)
		}
	}
	return result
}

// ConvertGSAToAllocationResponse converts GameServerAllocation V1 (GSA) to AllocationResponse
func ConvertGSAToAllocationResponse(in *allocationv1.GameServerAllocation) (*pb.AllocationResponse, error) {
	if in == nil {
		return nil, nil
	}

	if err := convertStateV1ToError(in.Status.State); err != nil {
		return nil, err
	}

	return &pb.AllocationResponse{
		GameServerName: in.Status.GameServerName,
		Address:        in.Status.Address,
		NodeName:       in.Status.NodeName,
		Ports:          convertGSAAgonesPortsToAllocationPorts(in.Status.Ports),
		Source:         in.Status.Source,
	}, nil
}

// ConvertAllocationResponseToGSA converts AllocationResponse to GameServerAllocation V1 (GSA)
func ConvertAllocationResponseToGSA(in *pb.AllocationResponse, rs string) *allocationv1.GameServerAllocation {
	if in == nil {
		return nil
	}

	out := &allocationv1.GameServerAllocation{
		Status: allocationv1.GameServerAllocationStatus{
			State:          allocationv1.GameServerAllocationAllocated,
			GameServerName: in.GameServerName,
			Address:        in.Address,
			NodeName:       in.NodeName,
			Ports:          convertAllocationPortsToGSAAgonesPorts(in.Ports),
			Source:         rs,
		},
	}
	out.SetGroupVersionKind(allocationv1.SchemeGroupVersion.WithKind("GameServerAllocation"))

	return out
}

// convertGSAAgonesPortsToAllocationPorts converts GameServerStatusPort V1 (GSA) to AllocationResponse_GameServerStatusPort
func convertGSAAgonesPortsToAllocationPorts(in []agonesv1.GameServerStatusPort) []*pb.AllocationResponse_GameServerStatusPort {
	var pbPorts []*pb.AllocationResponse_GameServerStatusPort
	for _, port := range in {
		pbPort := &pb.AllocationResponse_GameServerStatusPort{
			Name: port.Name,
			Port: port.Port,
		}
		pbPorts = append(pbPorts, pbPort)
	}
	return pbPorts
}

// convertAllocationPortsToGSAAgonesPorts converts AllocationResponse_GameServerStatusPort to GameServerStatusPort V1 (GSA)
func convertAllocationPortsToGSAAgonesPorts(in []*pb.AllocationResponse_GameServerStatusPort) []agonesv1.GameServerStatusPort {
	var out []agonesv1.GameServerStatusPort
	for _, port := range in {
		p := &agonesv1.GameServerStatusPort{
			Name: port.Name,
			Port: port.Port,
		}
		out = append(out, *p)
	}
	return out
}

// convertStateV1ToError converts GameServerAllocationState V1 (GSA) to AllocationResponse_GameServerAllocationState
func convertStateV1ToError(in allocationv1.GameServerAllocationState) error {
	switch in {
	case allocationv1.GameServerAllocationAllocated:
		return nil
	case allocationv1.GameServerAllocationUnAllocated:
		return status.Error(codes.ResourceExhausted, "there is no available GameServer to allocate")
	case allocationv1.GameServerAllocationContention:
		return status.Error(codes.Aborted, "too many concurrent requests have overwhelmed the system")
	}
	return status.Error(codes.Unknown, "unknown issue")
}

// convertAllocationPrioritiesToGSAPriorities converts a list of AllocationRequest_Priorities to a
// list of GameServerAllocationSpec (GSA.Spec) Priorities
func convertAllocationPrioritiesToGSAPriorities(in []*pb.Priority) []allocationv1.Priority {
	var out []allocationv1.Priority
	for _, p := range in {
		priority := allocationv1.Priority{
			PriorityType: p.PriorityType,
			Key:          p.Key,
			Order:        p.Order,
		}
		out = append(out, priority)
	}
	return out
}

// convertAllocationPrioritiesToGSAPriorities converts a list of GameServerAllocationSpec (GSA.Spec)
// Priorities to a list of AllocationRequest_Priorities
func convertGSAPrioritiesToAllocationPriorities(in []allocationv1.Priority) []*pb.Priority {
	var out []*pb.Priority
	for _, p := range in {
		priority := pb.Priority{
			PriorityType: p.PriorityType,
			Key:          p.Key,
			Order:        p.Order,
		}
		out = append(out, &priority)
	}
	return out
}
