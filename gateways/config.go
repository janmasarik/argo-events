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

package gateways

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/argoproj/argo-events/common"
	"github.com/argoproj/argo-events/pkg/apis/gateway/v1alpha1"
	gwclientset "github.com/argoproj/argo-events/pkg/client/gateway/clientset/versioned"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// GatewayConfig provides a generic event source for a gateway
type GatewayConfig struct {
	// Log provides fast and simple logger dedicated to JSON output
	Log zerolog.Logger
	// Clientset is client for kubernetes API
	Clientset kubernetes.Interface
	// Name is gateway name
	Name string
	// Namespace is namespace for the gateway to run inside
	Namespace string
	// KubeConfig rest client config
	KubeConfig *rest.Config
	// gateway holds Gateway custom resource
	gw *v1alpha1.Gateway
	// gwClientset is gateway clientset
	gwcs gwclientset.Interface
	// serverPort is gateway server port to listen events from
	serverPort string
	// registeredConfigs stores information about current event sources that are running in the gateway
	registeredConfigs map[string]*EventSourceContext
	// configName is name of configmap that contains run event source/s for the gateway
	configName string
	// controllerInstanceId is instance ID of the gateway controller
	controllerInstanceID string
	// StatusCh is used to communicate the status of an event source
	StatusCh chan EventSourceStatus
}

// EventSourceContext contains information of a event source for gateway to run.
type EventSourceContext struct {
	// Data holds the actual event source
	Data *EventSourceData
	// Ctx contains context for the connection
	Ctx context.Context
	// Cancel upon invocation cancels the connection context
	Cancel context.CancelFunc
	// Client is grpc client
	Client EventingClient
	// Conn is grpc connection
	Conn *grpc.ClientConn
}

// EventSourceData holds the actual event source
type EventSourceData struct {
	// Unique ID for event source
	ID string `json:"id"`
	// Src contains name of the event source
	Src string `json:"src"`
	// Config contains the event source
	Config string `json:"config"`
}

// GatewayEvent is the internal representation of an event.
type GatewayEvent struct {
	// Src is source of event
	Src string `json:"src"`
	// Payload contains event data
	Payload []byte `json:"payload"`
}

// NewGatewayConfiguration returns a new gateway configuration
func NewGatewayConfiguration() *GatewayConfig {
	kubeConfig, _ := os.LookupEnv(common.EnvVarKubeConfig)
	restConfig, err := common.GetClientConfig(kubeConfig)
	if err != nil {
		panic(err)
	}
	name, ok := os.LookupEnv(common.EnvVarGatewayName)
	if !ok {
		panic("gateway name not provided")
	}
	namespace, ok := os.LookupEnv(common.EnvVarGatewayNamespace)
	if !ok {
		panic("no namespace provided")
	}
	configName, ok := os.LookupEnv(common.EnvVarGatewayEventSourceConfigMap)
	if !ok {
		panic("gateway processor configmap is not provided")
	}
	controllerInstanceID, ok := os.LookupEnv(common.EnvVarGatewayControllerInstanceID)
	if !ok {
		panic("gateway controller instance ID is not provided")
	}
	serverPort, ok := os.LookupEnv(common.EnvVarGatewayServerPort)
	if !ok {
		panic("server port is not provided")
	}

	clientset := kubernetes.NewForConfigOrDie(restConfig)
	gwcs := gwclientset.NewForConfigOrDie(restConfig)
	gw, err := gwcs.ArgoprojV1alpha1().Gateways(namespace).Get(name, metav1.GetOptions{})
	if err != nil {
		panic(err)
	}

	return &GatewayConfig{
		Log:                  common.GetLoggerContext(common.LoggerConf()).Str("gateway-name", name).Str("gateway-namespace", namespace).Logger(),
		Clientset:            clientset,
		Namespace:            namespace,
		Name:                 name,
		KubeConfig:           restConfig,
		registeredConfigs:    make(map[string]*EventSourceContext),
		configName:           configName,
		gwcs:                 gwcs,
		gw:                   gw,
		controllerInstanceID: controllerInstanceID,
		serverPort:           serverPort,
		StatusCh:             make(chan EventSourceStatus),
	}
}

// createInternalEventSources creates an internal representation of event source declared in the gateway configmap.
// returned event sources are map of hash of event source and event source itself.
// Creating a hash of event source makes it easy to check equality of two event sources.
func (gc *GatewayConfig) createInternalEventSources(cm *corev1.ConfigMap) (map[string]*EventSourceContext, error) {
	configs := make(map[string]*EventSourceContext)
	for configKey, configValue := range cm.Data {
		hashKey := Hasher(configKey + configValue)
		gc.Log.Info().Str("config-key", configKey).Str("config-value", configValue).Str("hash", string(hashKey)).Msg("event source")

		// create a connection to gateway server
		ctx, cancel := context.WithCancel(context.Background())
		timeoutCtx, _ := context.WithTimeout(context.Background(), common.ServerConnTimeout*time.Second)
		conn, err := grpc.DialContext(timeoutCtx, fmt.Sprintf("localhost:%s", gc.serverPort), grpc.WithBlock(), grpc.WithInsecure())
		if err != nil {
			gc.Log.Panic().Err(err).Str("conn-state", conn.GetState().String()).Msg("failed to connect to gateway server")
			return nil, err
		}
		gc.Log.Info().Str("state", conn.GetState().String()).Msg("state of the connection")

		configs[hashKey] = &EventSourceContext{
			Data: &EventSourceData{
				ID:     hashKey,
				Src:    configKey,
				Config: configValue,
			},
			Cancel: cancel,
			Ctx:    ctx,
			Client: NewEventingClient(conn),
			Conn:   conn,
		}
	}
	return configs, nil
}

// diffConfig diffs currently registered event sources and the event sources in the gateway configmap
// It simply matches the event source strings. So, if event source string differs through some sequence of definition
// and although the event sources are actually same, this method will treat them as different event sources.
// retunrs staleConfig - event sources to be removed from gateway
// newConfig - new event sources to run
func (gc *GatewayConfig) diffEventSources(newConfigs map[string]*EventSourceContext) (staleConfigKeys []string, newConfigKeys []string) {
	var currentConfigKeys []string
	var updatedConfigKeys []string

	for currentConfigKey := range gc.registeredConfigs {
		currentConfigKeys = append(currentConfigKeys, currentConfigKey)
	}
	for updatedConfigKey := range newConfigs {
		updatedConfigKeys = append(updatedConfigKeys, updatedConfigKey)
	}

	gc.Log.Info().Interface("current-event-sources-keys", currentConfigKeys).Msg("event sources hashes")
	gc.Log.Info().Interface("updated-event-sources--keys", updatedConfigKeys).Msg("event sources hashes")

	swapped := false
	// iterates over current event sources and updated event sources
	// and creates two arrays, first one containing event sources that need to removed
	// and second containing new event sources that need to be added and run.
	for i := 0; i < 2; i++ {
		for _, cc := range currentConfigKeys {
			found := false
			for _, uc := range updatedConfigKeys {
				if cc == uc {
					found = true
					break
				}
			}
			if !found {
				if swapped {
					newConfigKeys = append(newConfigKeys, cc)
				} else {
					staleConfigKeys = append(staleConfigKeys, cc)
				}
			}
		}
		if i == 0 {
			currentConfigKeys, updatedConfigKeys = updatedConfigKeys, currentConfigKeys
			swapped = true
		}
	}
	return
}

// startEventSources starts new event sources added to gateway
func (gc *GatewayConfig) startEventSources(eventSources map[string]*EventSourceContext, keys []string) {
	for _, key := range keys {
		eventSource := eventSources[key]
		// register the event source
		gc.registeredConfigs[key] = eventSource
		gc.Log.Info().Str("event-source-name", eventSource.Data.Src).Msg("activating new event source")

		go func() {
			// conn should be in READY state
			if eventSource.Conn.GetState() != connectivity.Ready {
				gc.Log.Error().Msg("connection is not in ready state.")
				gc.StatusCh <- EventSourceStatus{
					Phase:   v1alpha1.NodePhaseError,
					Id:      eventSource.Data.ID,
					Message: fmt.Sprintf("connection is not in ready state"),
				}
				return
			}

			// validate event source
			_, err := eventSource.Client.ValidateEventSource(eventSource.Ctx, &EventSource{
				Data: &eventSource.Data.Config,
				Name: &eventSource.Data.Src,
			})
			if err != nil {
				gc.Log.Error().Str("event-source-name", eventSource.Data.Src).Err(err).Msg("event source is not valid")
				if err := eventSource.Conn.Close(); err != nil {
					gc.Log.Error().Str("event-source-name", eventSource.Data.Src).Err(err).Msg("failed to close client connection")
				}
				gc.StatusCh <- EventSourceStatus{
					Phase:   v1alpha1.NodePhaseError,
					Id:      eventSource.Data.ID,
					Message: fmt.Sprintf("event source is not valid. err: %+v", err),
				}
				return
			}

			// mark event source as running
			gc.StatusCh <- EventSourceStatus{
				Phase:   v1alpha1.NodePhaseRunning,
				Message: "event source is running",
				Id:      eventSource.Data.ID,
				Name:    eventSource.Data.Src,
			}

			// listen to events from gateway server
			eventStream, err := eventSource.Client.StartEventSource(eventSource.Ctx, &EventSource{
				Name: &eventSource.Data.Src,
				Data: &eventSource.Data.Config,
			})
			if err != nil {
				gc.StatusCh <- EventSourceStatus{
					Phase:   v1alpha1.NodePhaseError,
					Message: fmt.Sprintf("failed to receive event stream from event source. err: %+v", err),
					Id:      eventSource.Data.ID,
				}
				return
			}

			gc.Log.Info().Str("event-source-name", eventSource.Data.Src).Msg("started listening to events from gateway server")
			for {
				event, err := eventStream.Recv()
				if err != nil {
					if err == io.EOF {
						gc.Log.Info().Str("event-source-name", eventSource.Data.Src).Msg("event source has stopped")
						gc.StatusCh <- EventSourceStatus{
							Phase:   v1alpha1.NodePhaseCompleted,
							Message: "event source has been stopped",
							Id:      eventSource.Data.ID,
						}
						return
					}

					gc.Log.Error().Err(err).Str("event-source-name", eventSource.Data.Src).Msg("failed to receive event from stream")
					gc.StatusCh <- EventSourceStatus{
						Phase:   v1alpha1.NodePhaseError,
						Message: fmt.Sprintf("failed to receive event from event source stream. err: %v", err),
						Id:      eventSource.Data.ID,
					}
					return
				}
				err = gc.DispatchEvent(event)
				if err != nil {
					// todo: escalate using K8s event
					gc.Log.Error().Err(err).Str("event-source-name", eventSource.Data.Src).Msg("failed to dispatch event to watchers")
				}
			}
		}()
	}
}

// stopEventSources stops an existing event sources
func (gc *GatewayConfig) stopEventSources(configs []string) {
	for _, configKey := range configs {
		eventSource := gc.registeredConfigs[configKey]
		gc.Log.Info().Str("event-source-name", eventSource.Data.Src).Msg("removing the event source")
		gc.StatusCh <- EventSourceStatus{
			Phase: v1alpha1.NodePhaseRemove,
			Id:    eventSource.Data.ID,
		}
		eventSource.Cancel()
		if err := eventSource.Conn.Close(); err != nil {
			gc.Log.Error().Str("event-source-name", eventSource.Data.Src).Err(err).Msg("failed to close client connection")
		}
	}
}

// manageEventSources syncs registered event sources and updated gateway configmap
func (gc *GatewayConfig) manageEventSources(cm *corev1.ConfigMap) error {
	eventSources, err := gc.createInternalEventSources(cm)
	if err != nil {
		return err
	}

	staleEventSources, newEventSources := gc.diffEventSources(eventSources)
	gc.Log.Info().Interface("event-sources", staleEventSources).Msg("stale event sources")
	gc.Log.Info().Interface("event-sources", newEventSources).Msg("new event sources")

	// stop existing event sources
	gc.stopEventSources(staleEventSources)

	// start new event sources
	gc.startEventSources(eventSources, newEventSources)

	return nil
}
