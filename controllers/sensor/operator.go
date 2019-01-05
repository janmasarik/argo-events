/*
Copyright 2018 BlackRock, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sensor

import (
	"runtime/debug"
	"time"

	"github.com/argoproj/argo-events/common"
	"github.com/argoproj/argo-events/pkg/apis/sensor/v1alpha1"
	client "github.com/argoproj/argo-events/pkg/client/sensor/clientset/versioned/typed/sensor/v1alpha1"
	"github.com/rs/zerolog"
	appv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
)

// the context of an operation on a sensor.
// the sensor-controller creates this context each time it picks a Sensor off its queue.
type sOperationCtx struct {
	// s is the sensor object
	s *v1alpha1.Sensor
	// updated indicates whether the sensor object was updated and needs to be persisted back to k8
	updated bool
	// log is the logrus logging context to correlate logs with a sensor
	log zerolog.Logger
	// reference to the sensor-controller
	controller *SensorController
}

// newSensorOperationCtx creates and initializes a new sOperationCtx object
func newSensorOperationCtx(s *v1alpha1.Sensor, controller *SensorController) *sOperationCtx {
	return &sOperationCtx{
		s:          s.DeepCopy(),
		updated:    false,
		log:        common.GetLoggerContext(common.LoggerConf()).Str("sensor-name", s.Name).Str("sensor-namespace", s.Namespace).Logger(),
		controller: controller,
	}
}

func (soc *sOperationCtx) operate() error {
	defer soc.persistUpdates()
	defer func() {
		if r := recover(); r != nil {
			if rerr, ok := r.(error); ok {
				soc.markSensorPhase(v1alpha1.NodePhaseError, true, rerr.Error())
				soc.log.Error().Err(rerr)
			}
			soc.log.Error().Interface("recover", r).Str("stack", string(debug.Stack())).Msg("recovered from panic")
		}
	}()

	switch soc.s.Status.Phase {
	case v1alpha1.NodePhaseNew:
		// perform one-time sensor validation
		// non nil err indicates failed validation
		// we do not want to requeue a sensor in this case
		// since validation will fail every time
		err := ValidateSensor(soc.s)
		if err != nil {
			soc.log.Error().Err(err).Msg("failed to validate sensor")
			soc.markSensorPhase(v1alpha1.NodePhaseError, true, err.Error())
			return nil
		}

		// Initialize all signal nodes
		for _, signal := range soc.s.Spec.EventDependencies {
			soc.initializeNode(signal.Name, v1alpha1.NodeTypeEventDependency, v1alpha1.NodePhaseNew)
		}

		// Initialize all trigger nodes
		for _, trigger := range soc.s.Spec.Triggers {
			soc.initializeNode(trigger.Name, v1alpha1.NodeTypeTrigger, v1alpha1.NodePhaseNew)
		}

		// add default env variables
		soc.s.Spec.DeploySpec.Containers[0].Env = append(soc.s.Spec.DeploySpec.Containers[0].Env, []corev1.EnvVar{
			{
				Name:  common.SensorName,
				Value: soc.s.Name,
			},
			{
				Name:  common.SensorNamespace,
				Value: soc.s.Namespace,
			},
			{
				Name:  common.EnvVarSensorControllerInstanceID,
				Value: soc.controller.Config.InstanceID,
			},
		}...,
		)

		// create a sensor pod
		if _, err := soc.controller.kubeClientset.AppsV1().Deployments(soc.s.Namespace).Get(soc.s.Name, metav1.GetOptions{}); err != nil && apierr.IsNotFound(err) {
			// default label on sensor
			if soc.s.ObjectMeta.Labels == nil {
				soc.s.ObjectMeta.Labels = make(map[string]string)
			}
			soc.s.ObjectMeta.Labels[common.LabelSensorName] = soc.s.Name
			sensorDeployment := &appv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      soc.s.Name,
					Namespace: soc.s.Namespace,
					Labels:    soc.s.Labels,
					OwnerReferences: []metav1.OwnerReference{
						*metav1.NewControllerRef(soc.s, v1alpha1.SchemaGroupVersionKind),
					},
				},
				Spec: appv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: soc.s.ObjectMeta.Labels,
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							GenerateName: soc.s.Name,
							Namespace:    soc.s.Name,
							Annotations:  soc.s.Annotations,
							Labels:       soc.s.Labels,
						},
						Spec: *soc.s.Spec.DeploySpec,
					},
				},
			}
			_, err = soc.controller.kubeClientset.AppsV1().Deployments(soc.s.Namespace).Create(sensorDeployment)
			if err != nil {
				soc.log.Error().Err(err).Msg("failed to create deployment")
				return err
			}
			soc.log.Info().Msg("sensor deployment created")
		}

		// Create a ClusterIP service to expose sensor in cluster
		// For now, sensor will receive event notifications through http server.
		_, err = soc.controller.kubeClientset.CoreV1().Services(soc.s.Namespace).Get(common.DefaultServiceName(soc.s.Name), metav1.GetOptions{})
		if err != nil && apierr.IsNotFound(err) {
			// Create sensor service
			sensorSvc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      common.DefaultServiceName(soc.s.Name),
					Namespace: soc.s.Namespace,
					OwnerReferences: []metav1.OwnerReference{
						*metav1.NewControllerRef(soc.s, v1alpha1.SchemaGroupVersionKind),
					},
				},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Port:       intstr.Parse(common.SensorServicePort).IntVal,
							TargetPort: intstr.FromInt(int(intstr.Parse(common.SensorServicePort).IntVal)),
						},
					},
					Type:     corev1.ServiceTypeClusterIP,
					Selector: soc.s.Labels,
				},
			}
			_, err = soc.controller.kubeClientset.CoreV1().Services(soc.s.Namespace).Create(sensorSvc)
			if err != nil {
				soc.log.Error().Err(err).Msg("failed to create sensor service")
				return err
			}
		}

		// Mark all signal nodes as active
		for _, signal := range soc.s.Spec.EventDependencies {
			soc.markNodePhase(signal.Name, v1alpha1.NodePhaseActive, "node is active")
		}

		// if we get here - we know the signals are running
		soc.log.Info().Msg("marking sensor as active")
		soc.markSensorPhase(v1alpha1.NodePhaseActive, false, "listening for signal events")

	case v1alpha1.NodePhaseActive:
		if soc.s.AreAllNodesSuccess(v1alpha1.NodeTypeEventDependency) {
			if soc.s.AreAllNodesSuccess(v1alpha1.NodeTypeTrigger) {
				soc.markSensorPhase(v1alpha1.NodePhaseComplete, true)
			}
		}

	case v1alpha1.NodePhaseError:
		soc.log.Info().Msg("sensor is in error state. Check escalated K8 event for the error")

	case v1alpha1.NodePhaseComplete:
		soc.log.Info().Msg("sensor is in complete state")
		soc.s.Status.CompletionCount = soc.s.Status.CompletionCount + 1
		soc.log.Info().Msg("re-run sensor")
		soc.reRunSensor()
	}
	return nil
}

func (soc *sOperationCtx) reRunSensor() {
	soc.log.Info().Msg("resetting nodes and re-running sensor")
	soc.s.Status.Nodes = make(map[string]v1alpha1.NodeStatus)
	soc.markSensorPhase(v1alpha1.NodePhaseNew, false)
	soc.updated = true
}

// persist the updates to the Sensor resource
func (soc *sOperationCtx) persistUpdates() {
	var err error
	if !soc.updated {
		return
	}
	sensorClient := soc.controller.sensorClientset.ArgoprojV1alpha1().Sensors(soc.s.ObjectMeta.Namespace)
	soc.s, err = sensorClient.Update(soc.s)
	if err != nil {
		soc.log.Warn().Err(err).Msg("error updating sensor")
		if errors.IsConflict(err) {
			return
		}
		soc.log.Info().Msg("re-applying updates on latest version and retrying update")
		err = soc.reapplyUpdate(sensorClient)
		if err != nil {
			soc.log.Error().Err(err).Msg("failed to re-apply update")
			return
		}
	}
	soc.log.Info().Str("phase", string(soc.s.Status.Phase)).Msg("sensor state updated successfully")

	time.Sleep(1 * time.Second)
}

// reapplyUpdate by fetching a new version of the sensor and updating the status
func (soc *sOperationCtx) reapplyUpdate(sensorClient client.SensorInterface) error {
	return wait.ExponentialBackoff(common.DefaultRetry, func() (bool, error) {
		s, err := sensorClient.Get(soc.s.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		s.Status = soc.s.Status
		soc.s, err = sensorClient.Update(s)
		if err != nil {
			if !common.IsRetryableKubeAPIError(err) {
				return false, err
			}
			return false, nil
		}
		return true, nil
	})
}

// create a new node
func (soc *sOperationCtx) initializeNode(nodeName string, nodeType v1alpha1.NodeType, phase v1alpha1.NodePhase, messages ...string) *v1alpha1.NodeStatus {
	if soc.s.Status.Nodes == nil {
		soc.s.Status.Nodes = make(map[string]v1alpha1.NodeStatus)
	}
	nodeID := soc.s.NodeID(nodeName)
	oldNode, ok := soc.s.Status.Nodes[nodeID]
	if ok {
		soc.log.Info().Str("node-name", nodeName).Msg("node already initialized")
		return &oldNode
	}
	node := v1alpha1.NodeStatus{
		ID:          nodeID,
		Name:        nodeName,
		DisplayName: nodeName,
		Type:        nodeType,
		Phase:       phase,
		StartedAt:   metav1.MicroTime{Time: time.Now().UTC()},
	}
	if len(messages) > 0 {
		node.Message = messages[0]
	}
	soc.s.Status.Nodes[nodeID] = node
	soc.log.Info().Str("node-type", string(node.Type)).Str("node-name", node.DisplayName).Str("node-message", node.Message).Msg("node is initialized")
	soc.updated = true
	return &node
}

// mark the overall sensor phase
func (soc *sOperationCtx) markSensorPhase(phase v1alpha1.NodePhase, markComplete bool, message ...string) {
	justCompleted := soc.s.Status.Phase != phase
	if justCompleted {
		soc.log.Info().Str("old-phase", string(soc.s.Status.Phase)).Str("new-phase", string(phase)).Msg("sensor phase updated")
		soc.s.Status.Phase = phase
		soc.updated = true
		if soc.s.ObjectMeta.Labels == nil {
			soc.s.ObjectMeta.Labels = make(map[string]string)
		}
		if soc.s.ObjectMeta.Annotations == nil {
			soc.s.ObjectMeta.Annotations = make(map[string]string)
		}
		soc.s.ObjectMeta.Labels[common.LabelSensorKeyPhase] = string(phase)
		// add annotations so a resource sensor can watch this sensor.
		soc.s.ObjectMeta.Annotations[common.LabelSensorKeyPhase] = string(phase)
	}
	if soc.s.Status.StartedAt.IsZero() {
		soc.s.Status.StartedAt = metav1.Time{Time: time.Now().UTC()}
		soc.updated = true
	}
	if len(message) > 0 && soc.s.Status.Message != message[0] {
		soc.log.Info().Str("old-message", soc.s.Status.Message).Str("new-message", message[0]).Msg("sensor message updated")
		soc.s.Status.Message = message[0]
		soc.updated = true
	}

	switch phase {
	case v1alpha1.NodePhaseComplete, v1alpha1.NodePhaseError:
		if markComplete && justCompleted {
			soc.log.Info().Msg("marking sensor complete")
			soc.s.Status.CompletedAt = metav1.Time{Time: time.Now().UTC()}
			if soc.s.ObjectMeta.Labels == nil {
				soc.s.ObjectMeta.Labels = make(map[string]string)
			}
			soc.s.ObjectMeta.Labels[common.LabelSensorKeyComplete] = "true"
			soc.s.ObjectMeta.Annotations[common.LabelSensorKeyComplete] = string(phase)
			soc.updated = true
		}
	}
}

// markNodePhase marks the node with a phase, returns the node
func (soc *sOperationCtx) markNodePhase(nodeName string, phase v1alpha1.NodePhase, message ...string) *v1alpha1.NodeStatus {
	node := GetNodeByName(soc.s, nodeName)
	if node == nil {
		soc.log.Panic().Str("node-name", nodeName).Msg("node is uninitialized")
	}
	if node.Phase != phase {
		soc.log.Info().Str("type", string(node.Type)).Str("node-name", node.Name).Str("phase", string(node.Phase))
		node.Phase = phase
	}
	if len(message) > 0 && node.Message != message[0] {
		soc.log.Info().Str("type", string(node.Type)).Str("node-name", node.Name).Str("phase", string(node.Phase)).Str("message", message[0])
		node.Message = message[0]
	}
	if node.IsComplete() && node.CompletedAt.IsZero() {
		node.CompletedAt = metav1.MicroTime{Time: time.Now().UTC()}
		soc.log.Info().Str("type", string(node.Type)).Str("node-name", node.Name).Msg("completed")
	}
	soc.s.Status.Nodes[node.ID] = *node
	return node
}
