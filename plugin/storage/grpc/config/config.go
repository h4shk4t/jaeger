// Copyright (c) 2019 The Jaeger Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file ex	cept in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/jaegertracing/jaeger/pkg/config/tlscfg"
	"github.com/jaegertracing/jaeger/pkg/tenancy"
	"github.com/jaegertracing/jaeger/plugin/storage/grpc/shared"
)

// Configuration describes the options to customize the storage behavior.
type Configuration struct {
	PluginLogLevel       string `yaml:"log-level" mapstructure:"log_level"`
	RemoteServerAddr     string `yaml:"server" mapstructure:"server"`
	RemoteTLS            tlscfg.Options
	RemoteConnectTimeout time.Duration `yaml:"connection-timeout" mapstructure:"connection-timeout"`
	TenancyOpts          tenancy.Options

	pluginHealthCheck     *time.Ticker
	pluginHealthCheckDone chan bool
	remoteConn            *grpc.ClientConn
}

// ClientPluginServices defines services plugin can expose and its capabilities
type ClientPluginServices struct {
	shared.PluginServices
	Capabilities     shared.PluginCapabilities
	killPluginClient func()
}

func (c *ClientPluginServices) Close() error {
	if c.killPluginClient != nil {
		c.killPluginClient()
	}
	return nil
}

// PluginBuilder is used to create storage plugins. Implemented by Configuration.
type PluginBuilder interface {
	Build(logger *zap.Logger, tracerProvider trace.TracerProvider) (*ClientPluginServices, error)
	Close() error
}

// Build instantiates a PluginServices
func (c *Configuration) Build(logger *zap.Logger, tracerProvider trace.TracerProvider) (*ClientPluginServices, error) {
	return c.buildRemote(logger, tracerProvider)
}

func (c *Configuration) Close() error {
	if c.pluginHealthCheck != nil {
		c.pluginHealthCheck.Stop()
		c.pluginHealthCheckDone <- true
	}
	if c.remoteConn != nil {
		c.remoteConn.Close()
	}

	return c.RemoteTLS.Close()
}

func (c *Configuration) buildRemote(logger *zap.Logger, tracerProvider trace.TracerProvider) (*ClientPluginServices, error) {
	opts := []grpc.DialOption{
		grpc.WithStatsHandler(otelgrpc.NewClientHandler(otelgrpc.WithTracerProvider(tracerProvider))),
		grpc.WithBlock(),
	}
	if c.RemoteTLS.Enabled {
		tlsCfg, err := c.RemoteTLS.Config(logger)
		if err != nil {
			return nil, err
		}
		creds := credentials.NewTLS(tlsCfg)
		opts = append(opts, grpc.WithTransportCredentials(creds))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.RemoteConnectTimeout)
	defer cancel()

	tenancyMgr := tenancy.NewManager(&c.TenancyOpts)
	if tenancyMgr.Enabled {
		opts = append(opts, grpc.WithUnaryInterceptor(tenancy.NewClientUnaryInterceptor(tenancyMgr)))
		opts = append(opts, grpc.WithStreamInterceptor(tenancy.NewClientStreamInterceptor(tenancyMgr)))
	}
	var err error
	// TODO: Need to replace grpc.DialContext with grpc.NewClient and pass test
	c.remoteConn, err = grpc.DialContext(ctx, c.RemoteServerAddr, opts...)
	if err != nil {
		return nil, fmt.Errorf("error connecting to remote storage: %w", err)
	}

	grpcClient := shared.NewGRPCClient(c.remoteConn)
	return &ClientPluginServices{
		PluginServices: shared.PluginServices{
			Store:               grpcClient,
			ArchiveStore:        grpcClient,
			StreamingSpanWriter: grpcClient,
		},
		Capabilities: grpcClient,
	}, nil
}
