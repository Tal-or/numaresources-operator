/*
 * Copyright 2022 Red Hat, Inc.
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

package wait

import (
	"context"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

func ForDeploymentComplete(cli client.Client, dp *appsv1.Deployment, pollInterval, pollTimeout time.Duration) (*appsv1.Deployment, error) {
	key := ObjectKeyFromObject(dp)
	updatedDp := &appsv1.Deployment{}
	err := wait.PollImmediate(pollInterval, pollTimeout, func() (bool, error) {
		err := cli.Get(context.TODO(), key.AsKey(), updatedDp)
		if err != nil {
			klog.Warningf("failed to get the deployment %s: %v", key.String(), err)
			return false, err
		}

		if !IsDeploymentComplete(dp, &updatedDp.Status) {
			klog.Warningf("deployment %s not yet complete", key.String())
			return false, nil
		}

		klog.Infof("deployment %s complete", key.String())
		return true, nil
	})
	return updatedDp, err
}

func areDeploymentReplicasAvailable(newStatus *appsv1.DeploymentStatus, replicas int32) bool {
	return newStatus.UpdatedReplicas == replicas &&
		newStatus.Replicas == replicas &&
		newStatus.AvailableReplicas == replicas
}

func IsDeploymentComplete(dp *appsv1.Deployment, newStatus *appsv1.DeploymentStatus) bool {
	return areDeploymentReplicasAvailable(newStatus, *(dp.Spec.Replicas)) &&
		newStatus.ObservedGeneration >= dp.Generation
}
