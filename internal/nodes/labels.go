/*
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
 * Copyright 2026 Red Hat, Inc.
 */

package nodes

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	"sigs.k8s.io/controller-runtime/pkg/client"

	mcov1 "github.com/openshift/api/machineconfiguration/v1"

	"github.com/k8stopologyawareschedwg/deployer/pkg/deployer/platform"

	nodegroupv1 "github.com/openshift-kni/numaresources-operator/api/v1/helper/nodegroup"
	nrolabels "github.com/openshift-kni/numaresources-operator/internal/api/labels"
	mcpools "github.com/openshift-kni/numaresources-operator/pkg/machineconfigpools"
)

// LabelForTree labels each node matching the given tree's MCPs with
// its primary pool name and removes stale labels from nodes that
// matched this tree but no longer belong to any of its managed pools.
// The primary pool is determined via MCO priority logic.
// On HyperShift this is a no-op because the nodePool label is already
// exclusive (each node belongs to exactly one node-pool).
// allPools and allNodes must be pre-fetched by the caller and shared
// across per-tree calls to avoid redundant API requests.
func LabelForTree(ctx context.Context, cli client.Client, plat platform.Platform, tree nodegroupv1.Tree, allPools []mcov1.MachineConfigPool, allNodes []corev1.Node) error {
	if plat != platform.OpenShift {
		return nil
	}

	managedPools := managedPoolNames(tree)

	nodesByName := map[string]*corev1.Node{}
	for _, mcp := range tree.MachineConfigPools {
		sel, err := metav1.LabelSelectorAsSelector(mcp.Spec.NodeSelector)
		if err != nil {
			klog.V(4).InfoS("Invalid MCP node selector, skipping", "pool", mcp.Name, "error", err)
			continue
		}
		for i := range allNodes {
			if sel.Matches(labels.Set(allNodes[i].Labels)) {
				nodesByName[allNodes[i].Name] = &allNodes[i]
			}
		}
	}

	// single pass: determine primary pool for each node and patch the label
	desiredLabels := map[string]string{} // node name -> desired pool name
	for nodeName, node := range nodesByName {
		primaryPool, err := mcpools.GetPrimaryPoolForNode(allPools, node)
		if err != nil {
			klog.V(4).InfoS("Cannot determine primary pool for node, skipping", "node", nodeName, "error", err)
			continue
		}
		if primaryPool == nil {
			continue
		}
		if !managedPools.Has(primaryPool.Name) {
			continue
		}
		desiredLabels[nodeName] = primaryPool.Name
	}

	for name, poolName := range desiredLabels {
		node := nodesByName[name]
		if current, ok := node.Labels[nrolabels.NodePrimaryPool]; ok && current == poolName {
			continue
		}
		if err := patchLabel(ctx, cli, node, poolName); err != nil {
			return fmt.Errorf("failed to label node %q with pool %q: %w", name, poolName, err)
		}
		klog.V(3).InfoS("Labeled node with primary pool", "node", name, "pool", poolName)
	}

	return removeStaleLabelsForNodes(ctx, cli, nodesByName, desiredLabels)
}

// removeStaleLabelsForNodes removes the NRO primary-pool label from nodes
// in nodesByName that should no longer have it (e.g. pool changed or node
// is no longer in any managed pool for this tree).
func removeStaleLabelsForNodes(ctx context.Context, cli client.Client, nodesByName map[string]*corev1.Node, desiredLabels map[string]string) error {
	for name, node := range nodesByName {
		currentPool, hasLabel := node.Labels[nrolabels.NodePrimaryPool]
		if !hasLabel {
			continue
		}
		desiredPool, wantLabel := desiredLabels[name]
		if wantLabel && currentPool == desiredPool {
			continue
		}
		if err := removeLabel(ctx, cli, node); err != nil {
			return fmt.Errorf("failed to remove stale label from node %q: %w", name, err)
		}
		klog.V(3).InfoS("Removed stale primary pool label from node", "node", name, "oldPool", currentPool)
	}
	return nil
}

// RemoveAllLabels removes the NRO primary-pool label from all nodes.
// Used during finalizer cleanup when the NRO CR is deleted.
func RemoveAllLabels(ctx context.Context, cli client.Client) error {
	labeledNodes := &corev1.NodeList{}
	sel, _ := labels.Parse(nrolabels.NodePrimaryPool)
	if err := cli.List(ctx, labeledNodes, &client.ListOptions{LabelSelector: sel}); err != nil {
		return fmt.Errorf("failed to list nodes with NRO label: %w", err)
	}

	for i := range labeledNodes.Items {
		if err := removeLabel(ctx, cli, &labeledNodes.Items[i]); err != nil {
			return fmt.Errorf("failed to remove label from node %q: %w", labeledNodes.Items[i].Name, err)
		}
		klog.V(3).InfoS("Removed primary pool label from node (finalizer cleanup)", "node", labeledNodes.Items[i].Name)
	}
	return nil
}

func patchLabel(ctx context.Context, cli client.Client, node *corev1.Node, poolName string) error {
	base := node.DeepCopy()
	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}
	node.Labels[nrolabels.NodePrimaryPool] = poolName
	return cli.Patch(ctx, node, client.MergeFrom(base))
}

func removeLabel(ctx context.Context, cli client.Client, node *corev1.Node) error {
	if _, ok := node.Labels[nrolabels.NodePrimaryPool]; !ok {
		return nil
	}
	base := node.DeepCopy()
	delete(node.Labels, nrolabels.NodePrimaryPool)
	return cli.Patch(ctx, node, client.MergeFrom(base))
}

func managedPoolNames(tree nodegroupv1.Tree) sets.Set[string] {
	names := sets.New[string]()
	for _, mcp := range tree.MachineConfigPools {
		names.Insert(mcp.Name)
	}
	if tree.NodeGroup.PoolName != nil {
		names.Insert(*tree.NodeGroup.PoolName)
	}
	return names
}
