/*
 * Copyright 2018- The Pixie Authors.
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
 * SPDX-License-Identifier: Apache-2.0
 */

package controllers

import (
	"context"
	"strings"

	"github.com/blang/semver"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"

	"px.dev/pixie/src/shared/status"
)

const (
	// If 25% of the kernel versions are incompatible, then consider Vizier in
	// a degraded state.
	degradedThreshold = .25
)

var (
	kernelMinVersion = semver.Version{Major: 4, Minor: 14, Patch: 0}
)

func nodeIsCompatible(node *v1.Node) bool {
	version := node.Status.NodeInfo.KernelVersion
	// We don't actually care about pre-release tags, so drop them since they sometimes cause parse error.
	sp := strings.Split(version, "-")
	if len(sp) == 0 {
		return true
	}
	version = sp[0]
	version = strings.TrimPrefix(version, "v")
	// Minor version can sometime contain a "+", we remove it so it parses properly with semver.
	version = strings.TrimSuffix(version, "+")

	currentSemVer, err := semver.Make(version)
	if err != nil {
		log.WithError(err).Error("Failed to parse current Node Kernel version")
		return true
	}
	return currentSemVer.GE(kernelMinVersion)
}

type nodeCompatTracker struct {
	incompatibleCount float64
	nodeCompatible    map[string]bool
}

func (n *nodeCompatTracker) addNode(node *v1.Node) {
	com := nodeIsCompatible(node)
	if _, ok := n.nodeCompatible[node.Name]; ok {
		n.updateNode(node)
		return
	}
	n.nodeCompatible[node.Name] = com
	if !com {
		n.incompatibleCount++
	}
}

func (n *nodeCompatTracker) updateNode(node *v1.Node) {
	oldCom, ok := n.nodeCompatible[node.Name]
	if !ok {
		n.addNode(node)
		return
	}
	com := nodeIsCompatible(node)
	if com == oldCom {
		return
	}
	n.nodeCompatible[node.Name] = com
	if com {
		n.incompatibleCount--
	} else {
		n.incompatibleCount++
	}
}

func (n *nodeCompatTracker) removeNode(node *v1.Node) {
	com, ok := n.nodeCompatible[node.Name]
	if !ok {
		return
	}
	delete(n.nodeCompatible, node.Name)
	if !com {
		n.incompatibleCount--
	}
}

func (n *nodeCompatTracker) state() *vizierState {
	if n.incompatibleCount > degradedThreshold*float64(len(n.nodeCompatible)) {
		return &vizierState{Reason: status.KernelVersionsIncompatible}
	}
	return okState()
}

// NodeWatcher is responsible for tracking the nodes from the K8s API and using the NodeInfo to determine
// whether or not Pixie can successfully collect data on the cluster.
type nodeWatcher struct {
	factory informers.SharedInformerFactory

	compatTracker nodeCompatTracker

	state chan<- *vizierState
}

func (nw *nodeWatcher) start(ctx context.Context) {
	nw.compatTracker = nodeCompatTracker{
		incompatibleCount: 0.0,
		nodeCompatible:    make(map[string]bool),
	}

	informer := nw.factory.Core().V1().Nodes().Informer()
	stopper := make(chan struct{})
	defer close(stopper)
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    nw.onAdd,
		UpdateFunc: nw.onUpdate,
		DeleteFunc: nw.onDelete,
	})
	informer.Run(stopper)
}

func (nw *nodeWatcher) onAdd(obj interface{}) {
	node, ok := obj.(*v1.Node)
	if !ok {
		return
	}
	nw.compatTracker.addNode(node)
	nw.state <- nw.compatTracker.state()
}

func (nw *nodeWatcher) onUpdate(oldObj, newObj interface{}) {
	node, ok := newObj.(*v1.Node)
	if !ok {
		return
	}
	nw.compatTracker.updateNode(node)
	nw.state <- nw.compatTracker.state()
}

func (nw *nodeWatcher) onDelete(obj interface{}) {
	node, ok := obj.(*v1.Node)
	if !ok {
		return
	}
	nw.compatTracker.removeNode(node)
	nw.state <- nw.compatTracker.state()
}
