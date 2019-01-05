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

package artifact

import (
	"context"
	"testing"

	"github.com/argoproj/argo-events/gateways"
	"github.com/stretchr/testify/assert"
)

var (
	configKey   = "testConfig"
	configValue = `
s3EventConfig:
    bucket: input
    endpoint: minio-service.argo-events:9000
    event: s3:ObjectCreated:Put
    filter:
    prefix: ""
    suffix: ""
insecure: true
accessKey:
    key: accesskey
    name: artifacts-minio
secretKey:
    key: secretkey
    name: artifacts-minio
`
)

func TestS3ConfigExecutor_Validate(t *testing.T) {
	s3Config := &S3EventSourceExecutor{}
	ctx := &gateways.EventSource{
		Data: &configValue,
		Name: &configKey,
	}
	_, err := s3Config.ValidateEventSource(context.Background(), ctx)
	assert.Nil(t, err)

	badConfig := `
s3EventConfig:
    bucket: input
    endpoint: minio-service.argo-events:9000
    event: s3:ObjectCreated:Put
    filter:
        prefix: ""
        suffix: ""
insecure: true
`
	ctx.Data = &badConfig

	_, err = s3Config.ValidateEventSource(context.Background(), ctx)
	assert.NotNil(t, err)
}
