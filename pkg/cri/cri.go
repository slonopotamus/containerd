/*
   Copyright The containerd Authors.

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

package cri

import (
	"context"
	"fmt"
	"io"

	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	containerd "github.com/containerd/containerd/v2/client"
	srvconfig "github.com/containerd/containerd/v2/cmd/containerd/server/config"
	"github.com/containerd/containerd/v2/core/sandbox"
	criconfig "github.com/containerd/containerd/v2/pkg/cri/config"
	"github.com/containerd/containerd/v2/pkg/cri/constants"
	"github.com/containerd/containerd/v2/pkg/cri/instrument"
	"github.com/containerd/containerd/v2/pkg/cri/nri"
	"github.com/containerd/containerd/v2/pkg/cri/server"
	nriservice "github.com/containerd/containerd/v2/pkg/nri"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/platforms"

	"google.golang.org/grpc"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// Register CRI service plugin
func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.GRPCPlugin,
		ID:   "cri",
		Requires: []plugin.Type{
			plugins.CRIServicePlugin,
			plugins.SandboxControllerPlugin,
			plugins.NRIApiPlugin,
			plugins.EventPlugin,
			plugins.ServicePlugin,
			plugins.LeasePlugin,
			plugins.SandboxStorePlugin,
			plugins.TransferPlugin,
		},
		Config: &criconfig.ServiceConfig{
			DisableTCPService: true,
		},
		ConfigMigration: func(ctx context.Context, version int, pluginConfigs map[string]interface{}) error {
			if version >= srvconfig.CurrentConfigVersion {
				return nil
			}
			const pluginName = string(plugins.GRPCPlugin) + ".cri"
			original, ok := pluginConfigs[pluginName]
			if !ok {
				return nil
			}
			src := original.(map[string]interface{})

			// Currently only a single key migrated
			if val, ok := src["disable_tcp_service"]; ok {
				pluginConfigs[pluginName] = map[string]interface{}{
					"disable_tcp_service": val,
				}
			} else {
				delete(pluginConfigs, pluginName)
			}
			return nil
		},
		InitFn: initCRIService,
	})
}

func initCRIService(ic *plugin.InitContext) (interface{}, error) {
	ctx := ic.Context
	config := ic.Config.(*criconfig.ServiceConfig)

	// Get runtime service.
	criRuntimePlugin, err := ic.GetByID(plugins.CRIServicePlugin, "runtime")
	if err != nil {
		return nil, fmt.Errorf("unable to load CRI runtime service plugin dependency: %w", err)
	}

	// Get image service.
	criImagePlugin, err := ic.GetByID(plugins.CRIServicePlugin, "images")
	if err != nil {
		return nil, fmt.Errorf("unable to load CRI image service plugin dependency: %w", err)
	}

	log.G(ctx).Info("Connect containerd service")
	client, err := containerd.New(
		"",
		containerd.WithDefaultNamespace(constants.K8sContainerdNamespace),
		containerd.WithDefaultPlatform(platforms.Default()),
		containerd.WithInMemoryServices(ic),
		containerd.WithInMemorySandboxControllers(ic),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create containerd client: %w", err)
	}

	// TODO(dmcgowan): Get the full list directly from configured plugins
	sbControllers := map[string]sandbox.Controller{
		string(criconfig.ModePodSandbox): client.SandboxController(string(criconfig.ModePodSandbox)),
		string(criconfig.ModeShim):       client.SandboxController(string(criconfig.ModeShim)),
	}

	options := &server.CRIServiceOptions{
		RuntimeService:     criRuntimePlugin.(server.RuntimeService),
		ImageService:       criImagePlugin.(server.ImageService),
		NRI:                getNRIAPI(ic),
		Client:             client,
		SandboxControllers: sbControllers,
	}
	is := criImagePlugin.(imageService).GRPCService()

	// TODO: More options specifically for grpc service?
	s, rs, err := server.NewCRIService(options)
	if err != nil {
		return nil, fmt.Errorf("failed to create CRI service: %w", err)
	}

	// RegisterReadiness() must be called after NewCRIService(): https://github.com/containerd/containerd/issues/9163
	ready := ic.RegisterReadiness()
	go func() {
		if err := s.Run(ready); err != nil {
			log.G(ctx).WithError(err).Fatal("Failed to run CRI service")
		}
		// TODO(random-liu): Whether and how we can stop containerd.
	}()

	service := &criGRPCServer{
		RuntimeServiceServer: rs,
		ImageServiceServer:   is,
		Closer:               s, // TODO: Where is close run?
		initializer:          s,
	}

	if config.DisableTCPService {
		return service, nil
	}

	return criGRPCServerWithTCP{service}, nil
}

type imageService interface {
	GRPCService() runtime.ImageServiceServer
}

type initializer interface {
	IsInitialized() bool
}

type criGRPCServer struct {
	runtime.RuntimeServiceServer
	runtime.ImageServiceServer
	io.Closer
	initializer
}

func (c *criGRPCServer) register(s *grpc.Server) error {
	instrumented := instrument.NewService(c)
	runtime.RegisterRuntimeServiceServer(s, instrumented)
	runtime.RegisterImageServiceServer(s, instrumented)
	return nil
}

// Register registers all required services onto a specific grpc server.
// This is used by containerd cri plugin.
func (c *criGRPCServer) Register(s *grpc.Server) error {
	return c.register(s)
}

type criGRPCServerWithTCP struct {
	*criGRPCServer
}

// RegisterTCP register all required services onto a GRPC server on TCP.
// This is used by containerd CRI plugin.
func (c criGRPCServerWithTCP) RegisterTCP(s *grpc.Server) error {
	return c.register(s)
}

// Get the NRI plugin, and set up our NRI API for it.
func getNRIAPI(ic *plugin.InitContext) *nri.API {
	const (
		pluginType = plugins.NRIApiPlugin
		pluginName = "nri"
	)

	ctx := ic.Context

	p, err := ic.GetByID(pluginType, pluginName)
	if err != nil {
		log.G(ctx).Info("NRI service not found, NRI support disabled")
		return nil
	}

	api, ok := p.(nriservice.API)
	if !ok {
		log.G(ctx).Infof("NRI plugin (%s, %q) has incorrect type %T, NRI support disabled",
			pluginType, pluginName, api)
		return nil
	}

	log.G(ctx).Info("using experimental NRI integration - disable nri plugin to prevent this")

	return nri.NewAPI(api)
}
