// Copyright 2019 Tetrate
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controlplane

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"istio.io/istio/pilot/pkg/bootstrap"
	"istio.io/istio/pilot/pkg/networking/plugin"
	"istio.io/istio/pkg/config/mesh"
	"istio.io/istio/pkg/keepalive"

	"github.com/tetratelabs/getenvoy/pkg/binary/envoy"
	"github.com/tetratelabs/getenvoy/pkg/binary/envoy/debug"
	"github.com/tetratelabs/getenvoy/pkg/binary/envoytest"
	"github.com/tetratelabs/getenvoy/pkg/test/morerequire"
)

func TestMain(m *testing.M) {
	envoytest.FetchEnvoyAndRun(m)
}

// TestEnableIstioBootstrap ensures Istio bootstraps Envoy configuration, and that configuration in
// "networking.istio.io/v1alpha3" rules end up pushed back to Envoy via XDS.
//
// Technically what happens is bootstrap results in a "xds-grpc" cluster pointed at Pilot's gRPC address. During
// bootstrap, Envoy invokes "/envoy.service.discovery.v3.AggregatedDiscoveryService/StreamAggregatedResources", which
// reads configuration in pilot (from testdata/configs.yaml). This test passes when that configuration merges with the
// bootstrap configuration generated by Istio.
func TestEnableIstioBootstrap(t *testing.T) {
	// only includes configs.yaml, which includes egress rules for googleapis
	pilotServer, stopPilot := requireMockPilot(t, "testdata")
	defer stopPilot()

	tracingConfig = nil // prevent Envoy from hanging on unavailable Zipkin
	defer func() {
		tracingConfig = mesh.DefaultProxyConfig().Tracing
	}()

	pilotGrpc := pilotServer.GRPCListener.Addr().String()
	runtime, err := envoy.NewRuntime(
		func(r *envoy.Runtime) {
			r.Config.Mode = envoy.ParseMode("loadbalancer")
			// Choosing a random port doesn't help because envoy_bootstrap.json hard-codes 15020 15021 and 15090
			// See https://github.com/istio/istio/issues/32184 for follow-up
			r.Config.AdminPort = 15000
			r.Config.XDSAddress = pilotGrpc
			r.Config.IPAddresses = []string{"127.0.0.1"} // prevent calling controlplane.retrieveIPs() on CI hosts
		},
		EnableIstioBootstrap,                 // this integrates and ends up reading pilotGrpcAddr
		debug.EnableEnvoyAdminDataCollection, // this allows us to read clusters.txt
	)
	require.NoError(t, err, "error creating envoy runtime")
	defer os.RemoveAll(runtime.DebugStore())

	envoytest.RequireRunTerminate(t, runtime, envoytest.RunKillOptions{
		DisableAutoAdminPort: true,
		RetainDebugStore:     true, // Assertions below inspect files in the debug store
	})

	serverInfo, err := os.ReadFile(filepath.Join(runtime.DebugStore(), "server_info.json"))
	require.NoError(t, err, "error getting server_info.json")
	clusters, err := os.ReadFile(filepath.Join(runtime.DebugStore(), "clusters.txt"))
	require.NoError(t, err, "error getting clusters.txt")

	// First, ensure r.Config.XDSAddress ended up written to config as the discoveryAddress
	require.Contains(t, string(serverInfo), fmt.Sprintf(`"discoveryAddress": "%s"`, pilotGrpc))

	// Next, ensure a connection discoveryAddress was made (Envoy -> Pilot's XDS)
	require.Contains(t, string(clusters), fmt.Sprintf(`xds-grpc::%s::health_flags::healthy`, pilotGrpc))

	// Finally, verify Envoy read and applied configuration from Pilot's XDS.
	// This means not only did bootstrapping work (templating the startup of Envoy), but also dynamic configuration.
	require.Contains(t, string(clusters), `outbound|443||www.googleapis.com::added_via_api::true`)
}

// requireMockPilot will ensure a pilot server and returns it and a function to stop it.
func requireMockPilot(t *testing.T, configFileDir string) (*bootstrap.Server, func()) {
	configFileDir = morerequire.RequireAbsDir(t, configFileDir)
	cleanups := make([]func(), 0)

	cleanup := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}

	// This ensures on any panic the pilot process is stopped, which can prevent test hangs.
	deferredCleanup := cleanup
	defer func() {
		if deferredCleanup != nil {
			deferredCleanup()
		}
	}()

	// In case pilot tries to open any relative paths, make sure they are writeable
	workDir, removeWorkDir := morerequire.RequireNewTempDir(t)
	cleanups = append(cleanups, removeWorkDir)

	_, revertChDir := morerequire.RequireChDir(t, workDir)
	cleanups = append(cleanups, revertChDir)

	args := bootstrap.PilotArgs{
		KeepaliveOptions: keepalive.DefaultOption(),
		Namespace:        "testing",
		MCPOptions:       bootstrap.MCPOptions{MaxMessageSize: 1024 * 1024 * 4},
		Plugins:          []string{plugin.Health},
		RegistryOptions:  bootstrap.RegistryOptions{FileDir: configFileDir},
		ServerOptions:    bootstrap.DiscoveryServerOptions{HTTPAddr: "127.0.0.1:0"}, // gRPC multiplexes over this
		ShutdownDuration: 1 * time.Millisecond,
	}

	s, err := bootstrap.NewServer(&args)
	require.NoError(t, err, "failed to bootstrap mock pilot server with args: %s", args)

	// Start the mock pilot server, passing a stop channel used for cleanup later
	stop := make(chan struct{})
	err = s.Start(stop)
	require.NoError(t, err, "failed to start mock pilot server: %s", s)
	cleanups = append(cleanups, func() { close(stop) })

	// Await /ready endpoint
	client := &http.Client{Timeout: 1 * time.Second}
	checkURL := fmt.Sprintf("http://%s/ready", s.HTTPListener.Addr())
	require.Eventually(t, func() bool {
		resp, err := client.Get(checkURL)
		if err != nil {
			return false
		}
		defer resp.Body.Close()

		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 100*time.Millisecond, "error waiting for pilot to be /ready")

	deferredCleanup = nil // We succeeded, so don't need to tear down before returning

	return s, cleanup
}
