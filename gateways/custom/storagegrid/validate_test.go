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

package storagegrid

import (
	"context"
	"testing"

	"github.com/argoproj/argo-events/gateways"
	"github.com/stretchr/testify/assert"
)

var (
	configKey   = "testConfig"
	configValue = `
endpoint: "/"
port: "8080"
events:
    - "ObjectCreated:Put"
filter:
    suffix: ".txt"
    prefix: "hello-"
`
)

func TestStorageGridConfigExecutor_Validate(t *testing.T) {
	s3Config := &StorageGridEventSourceExecutor{}
	es := &gateways.EventSource{
		Data: &configValue,
	}
	ctx := context.Background()
	_, err := s3Config.ValidateEventSource(ctx, es)
	assert.Nil(t, err)

	badConfig := `
events:
    - "ObjectCreated:Put"
filter:
    suffix: ".txt"
    prefix: "hello-"
`

	es.Data = &badConfig

	_, err = s3Config.ValidateEventSource(ctx, es)
	assert.NotNil(t, err)
}
