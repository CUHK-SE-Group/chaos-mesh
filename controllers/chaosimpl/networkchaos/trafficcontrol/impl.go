// Copyright 2021 Chaos Mesh Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package trafficcontrol

import (
	"context"
	"strings"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	k8sError "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/chaos-mesh/chaos-mesh/api/v1alpha1"
	"github.com/chaos-mesh/chaos-mesh/controllers/chaosimpl/networkchaos/podnetworkchaosmanager"
	impltypes "github.com/chaos-mesh/chaos-mesh/controllers/chaosimpl/types"
	"github.com/chaos-mesh/chaos-mesh/controllers/chaosimpl/utils"
	"github.com/chaos-mesh/chaos-mesh/controllers/podnetworkchaos/ipset"
	"github.com/chaos-mesh/chaos-mesh/controllers/podnetworkchaos/netutils"
	"github.com/chaos-mesh/chaos-mesh/controllers/utils/controller"
)

var _ impltypes.ChaosImpl = (*Impl)(nil)

const (
	targetIPSetPostFix = "tgt"
	sourceIPSetPostFix = "src"
)

const (
	waitForApplySync   v1alpha1.Phase = "Not Injected/Wait"
	waitForRecoverSync v1alpha1.Phase = "Injected/Wait"
)

type Impl struct {
	client.Client

	builder *podnetworkchaosmanager.Builder

	Log logr.Logger
}

func (impl *Impl) Apply(ctx context.Context, index int, records []*v1alpha1.Record, obj v1alpha1.InnerObject) (v1alpha1.Phase, error) {
	// The only possible phase to get in here is "Not Injected" or "Not Injected/Wait"

	impl.Log.Info("traffic control Apply", "namespace", obj.GetNamespace(), "name", obj.GetName())
	networkchaos := obj.(*v1alpha1.NetworkChaos)
	if networkchaos.Status.Instances == nil {
		networkchaos.Status.Instances = make(map[string]int64)
	}

	record := records[index]
	phase := record.Phase

	if phase == waitForApplySync {
		podnetworkchaos := &v1alpha1.PodNetworkChaos{}
		namespacedName, err := controller.ParseNamespacedName(record.Id)
		if err != nil {
			return waitForApplySync, err
		}
		err = impl.Client.Get(ctx, namespacedName, podnetworkchaos)
		if err != nil {
			if k8sError.IsNotFound(err) {
				return v1alpha1.NotInjected, nil
			}

			if k8sError.IsForbidden(err) {
				if strings.Contains(err.Error(), "because it is being terminated") {
					return v1alpha1.NotInjected, nil
				}
			}

			return waitForApplySync, err
		}

		if podnetworkchaos.Status.FailedMessage != "" {
			return waitForApplySync, errors.New(podnetworkchaos.Status.FailedMessage)
		}

		if podnetworkchaos.Status.ObservedGeneration >= networkchaos.Status.Instances[record.Id] {
			return v1alpha1.Injected, nil
		}

		return waitForApplySync, nil
	}

	var pod v1.Pod
	namespacedName, err := controller.ParseNamespacedName(record.Id)
	if err != nil {
		return v1alpha1.NotInjected, err
	}
	err = impl.Client.Get(ctx, namespacedName, &pod)
	if err != nil {
		// TODO: handle this error
		return v1alpha1.NotInjected, err
	}

	source := networkchaos.Namespace + "/" + networkchaos.Name
	m := impl.builder.WithInit(source, types.NamespacedName{
		Namespace: pod.Namespace,
		Name:      pod.Name,
	})

	if record.SelectorKey == "." {
		if networkchaos.Spec.Direction == v1alpha1.To || networkchaos.Spec.Direction == v1alpha1.Both {
			var targets []*v1alpha1.Record
			for _, record := range records {
				if record.SelectorKey == ".Target" {
					targets = append(targets, record)
				}
			}

			err := impl.ApplyTc(ctx, m, targets, networkchaos, targetIPSetPostFix, networkchaos.Spec.Device)
			if err != nil {
				return v1alpha1.NotInjected, err
			}

			generationNumber, err := m.Commit(ctx, networkchaos)
			if err != nil {
				return v1alpha1.NotInjected, err
			}

			// modify the custom status
			networkchaos.Status.Instances[record.Id] = generationNumber
			return waitForApplySync, nil
		}

		return v1alpha1.Injected, nil
	} else if record.SelectorKey == ".Target" {
		if networkchaos.Spec.Direction == v1alpha1.From || networkchaos.Spec.Direction == v1alpha1.Both {
			var targets []*v1alpha1.Record
			for _, record := range records {
				if record.SelectorKey == "." {
					targets = append(targets, record)
				}
			}

			err := impl.ApplyTc(ctx, m, targets, networkchaos, sourceIPSetPostFix, networkchaos.Spec.TargetDevice)
			if err != nil {
				return v1alpha1.NotInjected, err
			}

			generationNumber, err := m.Commit(ctx, networkchaos)
			if err != nil {
				return v1alpha1.NotInjected, err
			}

			// modify the custom status
			networkchaos.Status.Instances[record.Id] = generationNumber
			return waitForApplySync, nil
		}

		return v1alpha1.Injected, nil
	} else {
		impl.Log.Info("unknown selector key", "record", record)
		return v1alpha1.NotInjected, nil
	}
}

func (impl *Impl) Recover(ctx context.Context, index int, records []*v1alpha1.Record, obj v1alpha1.InnerObject) (v1alpha1.Phase, error) {
	// The only possible phase to get in here is "Injected" or "Injected/Wait"

	networkchaos := obj.(*v1alpha1.NetworkChaos)
	if networkchaos.Status.Instances == nil {
		networkchaos.Status.Instances = make(map[string]int64)
	}

	record := records[index]
	phase := record.Phase

	if phase == waitForRecoverSync {
		podnetworkchaos := &v1alpha1.PodNetworkChaos{}
		namespacedName, err := controller.ParseNamespacedName(record.Id)
		if err != nil {
			// This error is not expected to exist
			return waitForRecoverSync, err
		}
		err = impl.Client.Get(ctx, namespacedName, podnetworkchaos)
		if err != nil {
			// TODO: handle this error
			if k8sError.IsNotFound(err) {
				return v1alpha1.NotInjected, nil
			}
			return waitForRecoverSync, err
		}

		if podnetworkchaos.Status.FailedMessage != "" {
			return waitForRecoverSync, errors.New(podnetworkchaos.Status.FailedMessage)
		}

		if podnetworkchaos.Status.ObservedGeneration >= networkchaos.Status.Instances[record.Id] {
			return v1alpha1.NotInjected, nil
		}

		return waitForRecoverSync, nil
	}

	var pod v1.Pod
	namespacedName, err := controller.ParseNamespacedName(record.Id)
	if err != nil {
		// This error is not expected to exist
		return v1alpha1.Injected, err
	}
	err = impl.Client.Get(ctx, namespacedName, &pod)
	if err != nil {
		// TODO: handle this error
		if k8sError.IsNotFound(err) {
			return v1alpha1.NotInjected, nil
		}

		if k8sError.IsForbidden(err) {
			if strings.Contains(err.Error(), "because it is being terminated") {
				return v1alpha1.NotInjected, nil
			}
		}
		return v1alpha1.Injected, err
	}

	source := networkchaos.Namespace + "/" + networkchaos.Name
	// TODO: use the DI but not construct it manually
	m := impl.builder.WithInit(source, types.NamespacedName{
		Namespace: pod.Namespace,
		Name:      pod.Name,
	})
	generationNumber, err := m.Commit(ctx, networkchaos)
	if err != nil {
		if err == podnetworkchaosmanager.ErrPodNotFound || err == podnetworkchaosmanager.ErrPodNotRunning {
			return v1alpha1.NotInjected, nil
		}
		return v1alpha1.Injected, err
	}

	// Now modify the custom status and phase
	networkchaos.Status.Instances[record.Id] = generationNumber
	return waitForRecoverSync, nil
}

func (impl *Impl) ApplyTc(ctx context.Context, m *podnetworkchaosmanager.PodNetworkManager, targets []*v1alpha1.Record, networkchaos *v1alpha1.NetworkChaos, ipSetPostFix string, device string) error {
	spec := networkchaos.Spec
	tcType := v1alpha1.Bandwidth
	switch spec.Action {
	case v1alpha1.NetemAction, v1alpha1.DelayAction, v1alpha1.DuplicateAction, v1alpha1.CorruptAction, v1alpha1.LossAction:
		tcType = v1alpha1.Netem
	case v1alpha1.BandwidthAction:
		tcType = v1alpha1.Bandwidth
	case v1alpha1.ConnectDelayAction:
		tcType = v1alpha1.ConnectDelay
	default:
		return errors.Wrapf(utils.ErrUnknownAction, "action: %s", spec.Action)
	}

	externalCidrs, err := netutils.ResolveCidrs(networkchaos.Spec.ExternalTargets)
	if err != nil {
		return err
	}

	if len(targets)+len(externalCidrs) == 0 {
		impl.Log.Info("apply traffic control", "sources", m.Source)
		m.T.Append(v1alpha1.RawTrafficControl{
			Type:        tcType,
			TcParameter: spec.TcParameter,
			Source:      m.Source,
			Device:      device,
		})
		return nil
	}

	targetPods := []v1.Pod{}
	for _, record := range targets {
		var pod v1.Pod
		namespacedName, err := controller.ParseNamespacedName(record.Id)
		if err != nil {
			// TODO: handle this error
			return err
		}
		err = impl.Client.Get(ctx, namespacedName, &pod)
		if err != nil {
			// TODO: handle this error
			return err
		}
		targetPods = append(targetPods, pod)
	}
	ipSetWithTcPostFix := string(tcType[0:2]) + ipSetPostFix
	dstIPSets := ipset.BuildIPSets(targetPods, externalCidrs, networkchaos, ipSetWithTcPostFix, m.Source)
	dstSetIPSet := ipset.BuildSetIPSet(dstIPSets, networkchaos, ipSetWithTcPostFix, m.Source)
	impl.Log.Info("apply traffic control with filter", "sources", m.Source, "setIpset", dstSetIPSet, "ipSets", dstIPSets)

	for _, ipSet := range dstIPSets {
		m.T.Append(ipSet)
	}

	m.T.Append(dstSetIPSet)

	m.T.Append(v1alpha1.RawTrafficControl{
		Type:        tcType,
		TcParameter: spec.TcParameter,
		Source:      m.Source,
		IPSet:       dstSetIPSet.Name,
		Device:      device,
	})

	return nil
}

func NewImpl(c client.Client, b *podnetworkchaosmanager.Builder, log logr.Logger) *Impl {
	return &Impl{
		Client:  c,
		builder: b,
		Log:     log.WithName("trafficcontrol"),
	}
}
