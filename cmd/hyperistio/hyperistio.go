package main

import (
	"flag"
	"fmt"
	"github.com/costinm/istio-vm/pkg/istiostart"
	"istio.io/istio/security/pkg/nodeagent/cache"
	"istio.io/istio/security/pkg/nodeagent/sds"
	"istio.io/istio/security/pkg/nodeagent/secretfetcher"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gogo/protobuf/types"

	meshv1 "istio.io/api/mesh/v1alpha1"

	"istio.io/istio/galley/pkg/server"
	"istio.io/istio/galley/pkg/server/settings"
	"istio.io/istio/pilot/pkg/bootstrap"
	"istio.io/istio/pilot/pkg/proxy/envoy"
	"istio.io/istio/pilot/pkg/serviceregistry"
	agent "istio.io/istio/pkg/bootstrap"
	"istio.io/istio/pkg/config/mesh"
	"istio.io/istio/pkg/keepalive"
)

// hyperistio runs istio control plane components in one binary, using a directory based config by
// default or MCP sources. It is intended for testing/debugging/prototyping, as well as for running on VMs.
//
// Directory structure has a base directory, which can be mounted in a docker container or as a config map:
//
// Binaries: /usr/local/bin
// Base dir is the current working dir.
//
// conf/... - config files.
// run/... - created envoy config, run files
// certs/ - certificate directory. If found, an envoy sidecar is started for control plane using the certs
// conf/ca - root CA directory.
//
// This will start an envoy sidecar, using SDS for certificates. There is no restart capability, just drain (currently
// off, debugging /dev/shm issues)
//
//
func main() {
	flag.Parse()

	err := startAll()
	if err != nil {
		log.Fatal("Failed to start ", err)
	}
}

// Start all components of istio, using local config files.
//
// A minimal set of Istio Env variables are also used.
// This is expected to run in a Docker or K8S environment, with a volume with user configs mounted.
//
//
func startAll() error {
	baseDir := "./"
	//meshConfigFile := baseDir + "/conf/pilot/mesh.yaml"

	mcfg := mesh.DefaultMeshConfig()

	mcfg.AuthPolicy = meshv1.MeshConfig_NONE

	mcfg.DisablePolicyChecks = true
	mcfg.ProxyHttpPort = 12080
	mcfg.ProxyListenPort = 12001

	// TODO: 15006 can't be configured currently
	// TODO: 15090 (prometheus) can't be configured. It's in the bootstrap file, so easy to replace

	mcfg.ProxyHttpPort = 12002
	mcfg.DefaultConfig = &meshv1.ProxyConfig{
		DiscoveryAddress:       "localhost:12010",
		ControlPlaneAuthPolicy: meshv1.AuthenticationPolicy_NONE,

		ProxyAdminPort: 12000,

		ConfigPath: baseDir + "/run",
		// BinaryPath:       "/usr/local/bin/envoy", - default
		CustomConfigFile:       baseDir + "/conf/sidecar/envoy_bootstrap_v2.json",
		ConnectTimeout:         types.DurationProto(5 * time.Second),  // crash if not set
		DrainDuration:          types.DurationProto(30 * time.Second), // crash if 0
		StatNameLength:         189,
		ParentShutdownDuration: types.DurationProto(5 * time.Second),

		ServiceCluster: "istio",
	}

	// Load config from the in-process Galley.
	// We can also configure Envoy to listen on 9901 and galley on different port, and LB
	mcfg.ConfigSources = []*meshv1.ConfigSource{
		&meshv1.ConfigSource{
			Address: "localhost:12901",
		},
	}

	err := startGalley(baseDir)
	if err != nil {
		return err
	}

	err = startPilot(baseDir, &mcfg)
	if err != nil {
		return err
	}

	// Start the SDS server for TLS certs
	err = StartSDS(baseDir, &mcfg)
	if err != nil {
		return err
	}

	// TODO: start envoy only if TLS certs exist (or bootstrap token and SDS server address is configured)
	err = startEnvoy(baseDir, &mcfg)
	if err != nil {
		return err
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
	// Will gradually terminate connections to Pilot
	istiostart.DrainEnvoy(baseDir, mcfg.DefaultConfig)

	return nil
}

// Start the SDS service. Uses the main Istio address.
//
func StartSDS(baseDir string, config *meshv1.MeshConfig) error {

	return nil
}

func StartSDSK8S(baseDir string, config *meshv1.MeshConfig) error {

	// This won't work on VM - only on K8S.
	var workloadSdsCacheOptions cache.Options
	var serverOptions sds.Options

	// Compat with Istio env
	caProvider := os.Getenv("CA_PROVIDER")
	if caProvider == "" {
		caProvider = "Citadel"
	}

	wSecretFetcher, err := secretfetcher.NewSecretFetcher(false,
		serverOptions.CAEndpoint, caProvider, true,
		[]byte(""), "", "", "", "")
	if err != nil {
		log.Fatal("failed to create secretFetcher for workload proxy", err)
	}
	workloadSdsCacheOptions.TrustDomain = serverOptions.TrustDomain
	workloadSdsCacheOptions.Plugins = sds.NewPlugins(serverOptions.PluginNames)
	workloadSecretCache := cache.NewSecretCache(wSecretFetcher, sds.NotifyProxy, workloadSdsCacheOptions)

	// GatewaySecretCache loads secrets from K8S
	_, err = sds.NewServer(serverOptions, workloadSecretCache, nil)

	if err != nil {
		log.Fatal("Failed to start SDS server", err)
	}

	return nil
}

var trustDomain = "cluster.local"

// TODO: use pilot-agent code, and refactor it to extract the core functionality.

// TODO: better implementation for 'drainFile' config - used by agent.terminate()

// startEnvoy starts the envoy sidecar for Istio control plane, for TLS and load balancing.
// Not used otherwise.
func startEnvoy(baseDir string, mcfg *meshv1.MeshConfig) error {
	os.Mkdir(baseDir+"/run", 0700)
	cfg := mcfg.DefaultConfig

	nodeId := "sidecar~127.0.0.2~istio-control.fortio~fortio.svc.cluster.local"
	env := os.Environ()
	env = append(env, "ISTIO_META_ISTIO_VERSION=1.4")

	cfgF, err := agent.WriteBootstrap(cfg, nodeId, 1, []string{
		"istio-pilot.istio-system",
		fmt.Sprintf("spiffe://%s/ns/%s/sa/%s", trustDomain, "istio-system", "istio-pilot-service-account"),
	},
		map[string]interface{}{},
		env,
		[]string{"127.0.0.2"}, // node IPs
		"60s")
	if err != nil {
		return err
	}

	// Start Envoy, using the pre-generated config. No restarts: if it crashes, we exit.
	stop := make(chan error)
	//features.EnvoyBaseId.DefaultValue = "1"
	process, err := agent.RunProxy(cfg, nodeId, 1, cfgF, stop,
		os.Stdout, os.Stderr, []string{
			"--disable-hot-restart",
			// "-l", "trace",
		})
	go func() {
		// Should not happen.
		process.Wait()
		log.Fatal("Envoy terminated, restart.")
	}()
	return err
}

// startPilot with defaults:
// - http port 15007
// - grpc on 15010
//- config from $ISTIO_CONFIG dir (defaults to in-source tests/testdata/config)
func startPilot(baseDir string, mcfg *meshv1.MeshConfig) error {
	stop := make(chan struct{})

	// Create a test pilot discovery service configured to watch the tempDir.
	args := bootstrap.PilotArgs{
		Namespace: "testing",
		DiscoveryOptions: envoy.DiscoveryServiceOptions{
			HTTPAddr:        ":12007",
			GrpcAddr:        ":12010",
			SecureGrpcAddr:  ":12011",
			EnableCaching:   true,
			EnableProfiling: true,
		},

		Mesh: bootstrap.MeshArgs{

			MixerAddress:    "localhost:9091",
			RdsRefreshDelay: types.DurationProto(10 * time.Millisecond),
		},
		Config: bootstrap.ConfigArgs{},
		Service: bootstrap.ServiceArgs{
			// Using the Mock service registry, which provides the hello and world services.
			Registries: []string{
				string(serviceregistry.MCPRegistry)},
		},

		// MCP is messing up with the grpc settings...
		MCPMaxMessageSize:        1024 * 1024 * 64,
		MCPInitialWindowSize:     1024 * 1024 * 64,
		MCPInitialConnWindowSize: 1024 * 1024 * 64,

		MeshConfig:       mcfg,
		KeepaliveOptions: keepalive.DefaultOption(),
	}

	bootstrap.FilepathWalkInterval = 5 * time.Second

	log.Println("Using mock configs: ")

	// Create and setup the controller.
	s, err := bootstrap.NewServer(args)
	if err != nil {
		return err
	}

	// Start the server.
	if err := s.Start(stop); err != nil {
		return err
	}
	return nil
}

// Start the galley component, with its args.

func startGalley(baseDir string) error {
	args := settings.DefaultArgs()

	// Default dir.
	// If not set, will attempt to use K8S.
	args.ConfigPath = baseDir + "/conf/istio/simple"
	// TODO: load a json file to override defaults (for all components)

	args.ValidationArgs.EnableValidation = false
	args.ValidationArgs.EnableReconcileWebhookConfiguration = false
	args.APIAddress = "tcp://0.0.0.0:12901"
	args.Insecure = true
	args.EnableServer = true
	args.DisableResourceReadyCheck = true
	// Use Galley Ctrlz for all services.
	args.IntrospectionOptions.Port = 12876

	// The file is loaded and watched by Galley using galley/pkg/meshconfig watcher/reader
	// Current code in galley doesn't expose it - we'll use 2 Caches instead.

	// Defaults are from pkg/config/mesh

	// Actual files are loaded by galley/pkg/src/fs, which recursively loads .yaml and .yml files
	// The files are suing YAMLToJSON, but interpret Kind, APIVersion

	args.MeshConfigFile = baseDir + "/conf/pilot/mesh.yaml"
	args.MonitoringPort = 12015

	gs := server.New(args)
	err := gs.Start()
	if err != nil {
		log.Fatalln("Galley startup error", err)
	}

	return nil
}
