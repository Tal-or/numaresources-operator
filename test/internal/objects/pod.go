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

package objects

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/openshift-kni/numaresources-operator/internal/resourcelist"
	"github.com/openshift-kni/numaresources-operator/test/internal/images"
)

func NewTestPodPause(namespace, name string) *corev1.Pod {
	return NewTestPodPauseMultiContainer(namespace, name, 1)
}

func NewTestPodPauseMultiContainer(namespace, name string, cntCount int) *corev1.Pod {
	var zero int64
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			TerminationGracePeriodSeconds: &zero,
			Containers:                    make([]corev1.Container, 0, cntCount),
		},
	}
	for idx := 0; idx < cntCount; idx++ {
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
			Name:    fmt.Sprintf("%s-cnt-%d", name, idx),
			Image:   images.GetPauseImage(),
			Command: []string{images.PauseCommand},
		})
	}
	return &pod
}

const (
	envVarDumpEvents = "E2E_NROP_DUMP_EVENTS"
)

func LogEventsForPod(k8sCli kubernetes.Interface, podNamespace, podName string) error {
	events, err := GetEventsForPod(k8sCli, podNamespace, podName)
	if err != nil {
		return err
	}
	klog.Infof("begin events for %s/%s", podNamespace, podName)
	for _, item := range events {
		klog.Infof("+- event: %s %s: %s %s", item.Type, item.ReportingController, item.Reason, item.Message)
	}
	klog.Infof("end events for %s/%s", podNamespace, podName)

	if _, ok := os.LookupEnv(envVarDumpEvents); ok {
		fmt.Println(DumpEventsForPod(events, podNamespace, podName))
	}
	return nil
}

func DumpEventsForPod(events []corev1.Event, podNamespace, podName string) string {
	var buf strings.Builder
	fmt.Fprintf(&buf, "begin event dump for %s/%s\n", podNamespace, podName)
	for _, item := range events {
		fmt.Fprintf(&buf, "+- event: %s %s: %s %s", item.Type, item.ReportingController, item.Reason, item.Message)
	}
	fmt.Fprintf(&buf, "end event dump for %s/%s", podNamespace, podName)
	return buf.String()
}

func GetEventsForPod(k8sCli kubernetes.Interface, podNamespace, podName string) ([]corev1.Event, error) {
	klog.Infof("checking events for pod %s/%s", podNamespace, podName)
	opts := metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s", podName),
		TypeMeta:      metav1.TypeMeta{Kind: "Pod"},
	}
	events, err := k8sCli.CoreV1().Events(podNamespace).List(context.TODO(), opts)
	if err != nil {
		klog.ErrorS(err, "cannot get events for pod %s/%s", podNamespace, podName)
		return nil, err
	}
	return events.Items, nil
}

func GetLogsForPod(k8sCli *kubernetes.Clientset, podNamespace, podName, containerName string) (string, error) {
	previous := false
	request := k8sCli.CoreV1().RESTClient().Get().Resource("pods").Namespace(podNamespace).Name(podName).SubResource("log").Param("container", containerName).Param("previous", strconv.FormatBool(previous))
	logs, err := request.Do(context.TODO()).Raw()
	if err != nil {
		return "", err
	}
	if strings.Contains(string(logs), "Internal Error") {
		return "", fmt.Errorf("Fetched log contains \"Internal Error\": %q", string(logs))
	}
	return string(logs), err
}

func DumpPODResourceRequirements(pod *corev1.Pod) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "resource requirements for pod %s/%s:\n", pod.Namespace, pod.Name)
	allContainers := []corev1.Container{}
	allContainers = append(allContainers, pod.Spec.Containers...)
	allContainers = append(allContainers, pod.Spec.InitContainers...)
	for _, container := range allContainers {
		fmt.Fprintf(&sb, "+- container %q: %s\n", container.Name, resourcelist.ToString(container.Resources.Limits))
	}
	fmt.Fprintf(&sb, "---\n")
	return sb.String()
}
