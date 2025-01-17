package test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/google/uuid"
	"github.com/mattn/go-tty"
	"github.com/onsi/ginkgo/v2"
	"github.com/phayes/freeport"
	"github.com/pkg/browser"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rancher/opni-monitoring/pkg/agent"
	"github.com/rancher/opni-monitoring/pkg/auth"
	testauth "github.com/rancher/opni-monitoring/pkg/auth/test"
	"github.com/rancher/opni-monitoring/pkg/bootstrap"
	"github.com/rancher/opni-monitoring/pkg/capabilities/wellknown"
	"github.com/rancher/opni-monitoring/pkg/config"
	"github.com/rancher/opni-monitoring/pkg/config/meta"
	"github.com/rancher/opni-monitoring/pkg/config/v1beta1"
	"github.com/rancher/opni-monitoring/pkg/core"
	"github.com/rancher/opni-monitoring/pkg/gateway"
	"github.com/rancher/opni-monitoring/pkg/ident"
	"github.com/rancher/opni-monitoring/pkg/logger"
	"github.com/rancher/opni-monitoring/pkg/management"
	"github.com/rancher/opni-monitoring/pkg/pkp"
	"github.com/rancher/opni-monitoring/pkg/plugins"
	"github.com/rancher/opni-monitoring/pkg/plugins/apis/apiextensions"
	gatewayext "github.com/rancher/opni-monitoring/pkg/plugins/apis/apiextensions/gateway"
	managementext "github.com/rancher/opni-monitoring/pkg/plugins/apis/apiextensions/management"
	"github.com/rancher/opni-monitoring/pkg/plugins/apis/capability"
	"github.com/rancher/opni-monitoring/pkg/plugins/apis/metrics"
	"github.com/rancher/opni-monitoring/pkg/plugins/apis/system"
	"github.com/rancher/opni-monitoring/pkg/sdk/api"
	"github.com/rancher/opni-monitoring/pkg/test/testutil"
	"github.com/rancher/opni-monitoring/pkg/tokens"
	"github.com/rancher/opni-monitoring/pkg/util"
	"github.com/rancher/opni-monitoring/pkg/util/waitctx"
	"github.com/rancher/opni-monitoring/pkg/webui"
	"github.com/ttacon/chalk"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var Log = logger.New(logger.WithLogLevel(zap.DebugLevel)).Named("test")

type servicePorts struct {
	Etcd            int
	Gateway         int
	ManagementGRPC  int
	ManagementHTTP  int
	ManagementWeb   int
	CortexGRPC      int
	CortexHTTP      int
	TestEnvironment int
}

type RunningAgent struct {
	*agent.Agent
	*sync.Mutex
}

type Environment struct {
	EnvironmentOptions

	TestBin           string
	Logger            *zap.SugaredLogger
	CRDDirectoryPaths []string

	mockCtrl *gomock.Controller

	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once

	tempDir string
	ports   servicePorts

	runningAgents   map[string]RunningAgent
	runningAgentsMu sync.Mutex

	gatewayConfig *v1beta1.GatewayConfig
	k8sEnv        *envtest.Environment

	Processes struct {
		Etcd      *util.Future[*os.Process]
		APIServer *util.Future[*os.Process]
	}
}

type EnvironmentOptions struct {
	enableEtcd    bool
	enableGateway bool
	enableCortex  bool
}

type EnvironmentOption func(*EnvironmentOptions)

func (o *EnvironmentOptions) Apply(opts ...EnvironmentOption) {
	for _, op := range opts {
		op(o)
	}
}

func WithEnableEtcd(enableEtcd bool) EnvironmentOption {
	return func(o *EnvironmentOptions) {
		o.enableEtcd = enableEtcd
	}
}

func WithEnableGateway(enableGateway bool) EnvironmentOption {
	return func(o *EnvironmentOptions) {
		o.enableGateway = enableGateway
	}
}

func WithEnableCortex(enableCortex bool) EnvironmentOption {
	return func(o *EnvironmentOptions) {
		o.enableCortex = enableCortex
	}
}

func (e *Environment) Start(opts ...EnvironmentOption) error {
	options := EnvironmentOptions{
		enableEtcd:    true,
		enableGateway: true,
		enableCortex:  true,
	}
	options.Apply(opts...)

	e.Logger = Log.Named("env")

	e.EnvironmentOptions = options
	e.Processes.Etcd = util.NewFuture[*os.Process]()

	lg := e.Logger
	lg.Info("Starting test environment")

	e.initCtx()
	e.runningAgents = make(map[string]RunningAgent)

	var t gomock.TestReporter
	if strings.HasSuffix(os.Args[0], ".test") {
		t = ginkgo.GinkgoT()
	}
	e.mockCtrl = gomock.NewController(t)

	if _, err := auth.GetMiddleware("test"); err != nil {
		if err := auth.RegisterMiddleware("test", &testauth.TestAuthMiddleware{
			Strategy: testauth.AuthStrategyUserIDInAuthHeader,
		}); err != nil {
			return fmt.Errorf("failed to install test auth middleware: %w", err)
		}
	}
	ports, err := freeport.GetFreePorts(8)
	if err != nil {
		panic(err)
	}
	e.ports = servicePorts{
		Etcd:            ports[0],
		Gateway:         ports[1],
		ManagementGRPC:  ports[2],
		ManagementHTTP:  ports[3],
		ManagementWeb:   ports[4],
		CortexGRPC:      ports[5],
		CortexHTTP:      ports[6],
		TestEnvironment: ports[7],
	}
	if portNum, ok := os.LookupEnv("OPNI_MANAGEMENT_GRPC_PORT"); ok {
		e.ports.ManagementGRPC, err = strconv.Atoi(portNum)
		if err != nil {
			return fmt.Errorf("failed to parse management GRPC port: %w", err)
		}
	}
	if portNum, ok := os.LookupEnv("OPNI_MANAGEMENT_HTTP_PORT"); ok {
		e.ports.ManagementHTTP, err = strconv.Atoi(portNum)
		if err != nil {
			return fmt.Errorf("failed to parse management HTTP port: %w", err)
		}
	}
	if portNum, ok := os.LookupEnv("OPNI_MANAGEMENT_WEB_PORT"); ok {
		e.ports.ManagementWeb, err = strconv.Atoi(portNum)
		if err != nil {
			return fmt.Errorf("failed to parse management web port: %w", err)
		}
	}
	if portNum, ok := os.LookupEnv("OPNI_GATEWAY_PORT"); ok {
		e.ports.Gateway, err = strconv.Atoi(portNum)
		if err != nil {
			return fmt.Errorf("failed to parse gateway port: %w", err)
		}
	}
	if portNum, ok := os.LookupEnv("TEST_ENV_API_PORT"); ok {
		e.ports.TestEnvironment, err = strconv.Atoi(portNum)
		if err != nil {
			panic(err)
		}
	}

	e.tempDir, err = os.MkdirTemp("", "opni-monitoring-test-*")
	if err != nil {
		return err
	}
	if options.enableEtcd {
		if err := os.Mkdir(path.Join(e.tempDir, "etcd"), 0700); err != nil {
			return err
		}
	}
	if options.enableCortex {
		cortexTempDir := path.Join(e.tempDir, "cortex")
		if err := os.MkdirAll(path.Join(cortexTempDir, "rules"), 0700); err != nil {
			return err
		}

		entries, _ := fs.ReadDir(TestDataFS, "testdata/cortex")
		lg.Infof("Copying %d files from embedded testdata/cortex to %s", len(entries), cortexTempDir)
		for _, entry := range entries {
			if err := os.WriteFile(path.Join(cortexTempDir, entry.Name()), TestData("cortex/"+entry.Name()), 0644); err != nil {
				return err
			}
		}
	}
	if options.enableGateway {
		if err := os.Mkdir(path.Join(e.tempDir, "prometheus"), 0700); err != nil {
			return err
		}
	}

	if options.enableEtcd {
		e.startEtcd()
	}
	if options.enableGateway {
		e.startGateway()
	}
	if options.enableCortex {
		e.startCortex()
	}
	return nil
}

func (e *Environment) StartK8s() (*rest.Config, error) {
	e.initCtx()
	e.Processes.APIServer = util.NewFuture[*os.Process]()

	port, err := freeport.GetFreePort()
	if err != nil {
		panic(err)
	}
	scheme := api.NewScheme()

	e.k8sEnv = &envtest.Environment{
		BinaryAssetsDirectory: e.TestBin,
		CRDDirectoryPaths:     e.CRDDirectoryPaths,
		Scheme:                scheme,
		CRDs:                  downloadCertManagerCRDs(scheme),
		ControlPlane: envtest.ControlPlane{
			APIServer: &envtest.APIServer{
				SecureServing: envtest.SecureServing{
					ListenAddr: envtest.ListenAddr{
						Address: "127.0.0.1",
						Port:    fmt.Sprint(port),
					},
				},
			},
		},
	}

	cfg, err := e.k8sEnv.Start()
	if err != nil {
		return nil, err
	}
	pid := os.Getpid()
	threads, err := os.ReadDir(fmt.Sprintf("/proc/%d/task/", pid))
	if err != nil {
		panic(err)
	}
	possiblePIDs := []int{}
	for _, thread := range threads {
		childProcessIDs, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/children", pid, thread.Name()))
		if err != nil {
			continue
		}
		if len(childProcessIDs) > 0 {
			parts := strings.Split(string(childProcessIDs), " ")
			for _, part := range parts {
				if pid, err := strconv.Atoi(part); err == nil {
					possiblePIDs = append(possiblePIDs, pid)
				}
			}
		}
	}
	var apiserverPID int
	for _, pid := range possiblePIDs {
		exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		if err != nil {
			continue
		}
		if filepath.Base(exe) == "kube-apiserver" {
			apiserverPID = pid
			break
		}
	}
	if apiserverPID == 0 {
		panic("could not find kube-apiserver PID")
	}
	proc, err := os.FindProcess(apiserverPID)
	if err != nil {
		panic(err)
	}
	e.Processes.APIServer.Set(proc)
	return cfg, nil
}

type Reconciler interface {
	SetupWithManager(ctrl.Manager) error
}

func (e *Environment) StartManager(restConfig *rest.Config, reconcilers ...Reconciler) ctrl.Manager {
	ports := util.Must(freeport.GetFreePorts(2))

	manager := util.Must(ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 e.k8sEnv.Scheme,
		MetricsBindAddress:     fmt.Sprintf(":%d", ports[0]),
		HealthProbeBindAddress: fmt.Sprintf(":%d", ports[1]),
	}))
	for _, reconciler := range reconcilers {
		util.Must(reconciler.SetupWithManager(manager))
	}
	go func() {
		if err := manager.Start(e.ctx); err != nil {
			panic(err)
		}
	}()
	return manager
}

func (e *Environment) Stop() error {
	if e.cancel != nil {
		e.cancel()
		waitctx.Wait(e.ctx, 20*time.Second)
	}
	if e.k8sEnv != nil {
		e.k8sEnv.Stop()
	}
	if e.mockCtrl != nil {
		e.mockCtrl.Finish()
	}
	if e.tempDir != "" {
		os.RemoveAll(e.tempDir)
	}
	return nil
}

func (e *Environment) initCtx() {
	e.once.Do(func() {
		e.ctx, e.cancel = context.WithCancel(waitctx.Background())
	})
}

func (e *Environment) startEtcd() {
	if !e.enableEtcd {
		e.Logger.Panic("etcd disabled")
	}
	lg := e.Logger
	defaultArgs := []string{
		fmt.Sprintf("--listen-client-urls=http://localhost:%d", e.ports.Etcd),
		fmt.Sprintf("--advertise-client-urls=http://localhost:%d", e.ports.Etcd),
		"--listen-peer-urls=http://localhost:0",
		"--log-level=error",
		fmt.Sprintf("--data-dir=%s", path.Join(e.tempDir, "etcd")),
	}
	etcdBin := path.Join(e.TestBin, "etcd")
	cmd := exec.CommandContext(e.ctx, etcdBin, defaultArgs...)
	cmd.Env = []string{"ALLOW_NONE_AUTHENTICATION=yes"}
	plugins.ConfigureSysProcAttr(cmd)
	session, err := testutil.StartCmd(cmd)
	if err != nil {
		if !errors.Is(e.ctx.Err(), context.Canceled) {
			panic(err)
		} else {
			return
		}
	}
	e.Processes.Etcd.Set(cmd.Process)

	lg.Info("Waiting for etcd to start...")
	for e.ctx.Err() == nil {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/health", e.ports.Etcd))
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(time.Second)
	}
	lg.Info("Etcd started")
	waitctx.Go(e.ctx, func() {
		<-e.ctx.Done()
		session.Wait()
	})
}

type cortexTemplateOptions struct {
	HttpListenPort int
	GrpcListenPort int
	StorageDir     string
}

func (e *Environment) startCortex() {
	if !e.enableCortex {
		e.Logger.Panic("cortex disabled")
	}
	lg := e.Logger
	configTemplate := TestData("cortex/config.yaml")
	t := util.Must(template.New("config").Parse(string(configTemplate)))
	configFile, err := os.Create(path.Join(e.tempDir, "cortex", "config.yaml"))
	if err != nil {
		panic(err)
	}
	if err := t.Execute(configFile, cortexTemplateOptions{
		HttpListenPort: e.ports.CortexHTTP,
		GrpcListenPort: e.ports.CortexGRPC,
		StorageDir:     path.Join(e.tempDir, "cortex"),
	}); err != nil {
		panic(err)
	}
	configFile.Close()
	cortexBin := path.Join(e.TestBin, "cortex")
	defaultArgs := []string{
		fmt.Sprintf("-config.file=%s", path.Join(e.tempDir, "cortex/config.yaml")),
	}
	cmd := exec.CommandContext(e.ctx, cortexBin, defaultArgs...)
	plugins.ConfigureSysProcAttr(cmd)
	session, err := testutil.StartCmd(cmd)
	if err != nil {
		if !errors.Is(e.ctx.Err(), context.Canceled) {
			panic(err)
		}
	}
	lg.Info("Waiting for cortex to start...")
	for e.ctx.Err() == nil {
		req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("https://localhost:%d/ready", e.ports.Gateway), nil)
		client := http.Client{
			Transport: &http.Transport{
				TLSClientConfig: e.GatewayTLSConfig(),
			},
		}
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			break
		}
		if resp != nil {
			lg.With(
				zap.Error(err),
				"status", resp.Status,
			).Info("Waiting for cortex to start...")
		}
		time.Sleep(time.Second)
	}
	lg.Info("Cortex started")
	waitctx.Go(e.ctx, func() {
		<-e.ctx.Done()
		session.Wait()
	})
}

type prometheusTemplateOptions struct {
	ListenPort    int
	OpniAgentPort int
}

func (e *Environment) StartPrometheus(opniAgentPort int) int {
	if !e.enableGateway {
		e.Logger.Panic("gateway disabled")
	}
	lg := e.Logger
	port, err := freeport.GetFreePort()
	if err != nil {
		panic(err)
	}
	configTemplate := TestData("prometheus/config.yaml")
	t := util.Must(template.New("config").Parse(string(configTemplate)))
	configFile, err := os.Create(path.Join(e.tempDir, "prometheus", "config.yaml"))
	if err != nil {
		panic(err)
	}
	if err := t.Execute(configFile, prometheusTemplateOptions{
		ListenPort:    port,
		OpniAgentPort: opniAgentPort,
	}); err != nil {
		panic(err)
	}
	configFile.Close()
	prometheusBin := path.Join(e.TestBin, "prometheus")
	defaultArgs := []string{
		fmt.Sprintf("--config.file=%s", path.Join(e.tempDir, "prometheus/config.yaml")),
		fmt.Sprintf("--storage.agent.path=%s", path.Join(e.tempDir, "prometheus", fmt.Sprint(opniAgentPort))),
		fmt.Sprintf("--web.listen-address=127.0.0.1:%d", port),
		"--log.level=error",
		"--web.enable-lifecycle",
		"--enable-feature=agent",
	}
	cmd := exec.CommandContext(e.ctx, prometheusBin, defaultArgs...)
	plugins.ConfigureSysProcAttr(cmd)
	session, err := testutil.StartCmd(cmd)
	if err != nil {
		if !errors.Is(e.ctx.Err(), context.Canceled) {
			panic(err)
		}
	}
	lg.Info("Waiting for prometheus to start...")
	for e.ctx.Err() == nil {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/-/ready", port))
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(time.Second)
	}
	lg.Info("Prometheus started")
	waitctx.Go(e.ctx, func() {
		<-e.ctx.Done()
		session.Wait()
	})
	return port
}

func (e *Environment) newGatewayConfig() *v1beta1.GatewayConfig {
	caCertData := string(TestData("root_ca.crt"))
	servingCertData := string(TestData("localhost.crt"))
	servingKeyData := string(TestData("localhost.key"))
	return &v1beta1.GatewayConfig{
		TypeMeta: meta.TypeMeta{
			APIVersion: "v1beta1",
			Kind:       "GatewayConfig",
		},
		Spec: v1beta1.GatewayConfigSpec{
			Plugins: v1beta1.PluginsSpec{
				Dirs: []string{ // ¯\_(ツ)_/¯
					"bin",
					"../bin",
					"../../bin",
					"../../../bin",
					"../../../../bin",
					"../../../../../bin",
				},
			},
			ListenAddress: fmt.Sprintf("localhost:%d", e.ports.Gateway),
			EnableMonitor: true,
			Management: v1beta1.ManagementSpec{
				GRPCListenAddress: fmt.Sprintf("tcp://127.0.0.1:%d", e.ports.ManagementGRPC),
				HTTPListenAddress: fmt.Sprintf("127.0.0.1:%d", e.ports.ManagementHTTP),
				WebListenAddress:  fmt.Sprintf("127.0.0.1:%d", e.ports.ManagementWeb),
			},
			AuthProvider: "test",
			Certs: v1beta1.CertsSpec{
				CACertData:      &caCertData,
				ServingCertData: &servingCertData,
				ServingKeyData:  &servingKeyData,
			},
			Cortex: v1beta1.CortexSpec{
				Distributor: v1beta1.DistributorSpec{
					HTTPAddress: fmt.Sprintf("localhost:%d", e.ports.CortexHTTP),
					GRPCAddress: fmt.Sprintf("localhost:%d", e.ports.CortexGRPC),
				},
				Ingester: v1beta1.IngesterSpec{
					HTTPAddress: fmt.Sprintf("localhost:%d", e.ports.CortexHTTP),
					GRPCAddress: fmt.Sprintf("localhost:%d", e.ports.CortexGRPC),
				},
				Alertmanager: v1beta1.AlertmanagerSpec{
					HTTPAddress: fmt.Sprintf("localhost:%d", e.ports.CortexHTTP),
				},
				Ruler: v1beta1.RulerSpec{
					HTTPAddress: fmt.Sprintf("localhost:%d", e.ports.CortexHTTP),
				},
				QueryFrontend: v1beta1.QueryFrontendSpec{
					HTTPAddress: fmt.Sprintf("localhost:%d", e.ports.CortexHTTP),
					GRPCAddress: fmt.Sprintf("localhost:%d", e.ports.CortexGRPC),
				},
				Certs: v1beta1.MTLSSpec{
					ServerCA:   path.Join(e.tempDir, "cortex/root.crt"),
					ClientCA:   path.Join(e.tempDir, "cortex/root.crt"),
					ClientCert: path.Join(e.tempDir, "cortex/client.crt"),
					ClientKey:  path.Join(e.tempDir, "cortex/client.key"),
				},
			},
			Storage: v1beta1.StorageSpec{
				Type: v1beta1.StorageTypeEtcd,
				Etcd: &v1beta1.EtcdStorageSpec{
					Endpoints: []string{fmt.Sprintf("http://localhost:%d", e.ports.Etcd)},
				},
			},
		},
	}
}

func (e *Environment) NewManagementClient() management.ManagementClient {
	if !e.enableGateway {
		e.Logger.Panic("gateway disabled")
	}
	c, err := management.NewClient(e.ctx,
		management.WithListenAddress(fmt.Sprintf("127.0.0.1:%d", e.ports.ManagementGRPC)),
		management.WithDialOptions(grpc.WithDefaultCallOptions(grpc.WaitForReady(true))),
	)
	if err != nil {
		panic(err)
	}
	return c
}

func (e *Environment) PrometheusAPIEndpoint() string {
	if !e.enableGateway {
		e.Logger.Panic("gateway disabled")
	}
	return fmt.Sprintf("https://localhost:%d/prometheus/api/v1", e.ports.Gateway)
}

func (e *Environment) startGateway() {
	if !e.enableGateway {
		e.Logger.Panic("gateway disabled")
	}
	lg := e.Logger
	e.gatewayConfig = e.newGatewayConfig()
	pluginLoader := plugins.NewPluginLoader()
	LoadPlugins(pluginLoader)
	mgmtExtensionPlugins := plugins.DispenseAllAs[apiextensions.ManagementAPIExtensionClient](
		pluginLoader, managementext.ManagementAPIExtensionPluginID)
	gatewayExtensionPlugins := plugins.DispenseAllAs[apiextensions.GatewayAPIExtensionClient](
		pluginLoader, gatewayext.GatewayAPIExtensionPluginID)
	systemPlugins := pluginLoader.DispenseAll(system.SystemPluginID)
	capBackendPlugins := plugins.DispenseAllAs[capability.BackendClient](
		pluginLoader, capability.CapabilityBackendPluginID)
	metricsPlugins := plugins.DispenseAllAs[prometheus.Collector](
		pluginLoader, metrics.MetricsPluginID)

	lifecycler := config.NewLifecycler(meta.ObjectList{e.gatewayConfig, &v1beta1.AuthProvider{
		TypeMeta: meta.TypeMeta{
			APIVersion: "v1beta1",
			Kind:       "AuthProvider",
		},
		ObjectMeta: meta.ObjectMeta{
			Name: "test",
		},
		Spec: v1beta1.AuthProviderSpec{
			Type: "test",
		},
	}})
	g := gateway.NewGateway(e.ctx, e.gatewayConfig,
		gateway.WithSystemPlugins(systemPlugins),
		gateway.WithLifecycler(lifecycler),
		gateway.WithCapabilityBackendPlugins(capBackendPlugins),
		gateway.WithAPIServerOptions(
			gateway.WithAPIExtensions(gatewayExtensionPlugins),
			gateway.WithAuthMiddleware(e.gatewayConfig.Spec.AuthProvider),
			gateway.WithMetricsPlugins(metricsPlugins),
		),
	)
	m := management.NewServer(e.ctx, &e.gatewayConfig.Spec.Management, g,
		management.WithCapabilitiesDataSource(g),
		management.WithSystemPlugins(systemPlugins),
		management.WithLifecycler(lifecycler),
		management.WithAPIExtensions(mgmtExtensionPlugins),
	)
	go func() {
		if err := g.ListenAndServe(); err != nil {
			lg.Errorf("gateway error: %v", err)
		}
	}()
	go func() {
		if err := m.ListenAndServe(); err != nil {
			lg.Errorf("management server error: %v", err)
		}
	}()
	lg.Info("Waiting for gateway to start...")
	for i := 0; i < 10; i++ {
		req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("https://%s/healthz",
			e.gatewayConfig.Spec.ListenAddress), nil)
		client := http.Client{
			Transport: &http.Transport{
				TLSClientConfig: e.GatewayTLSConfig(),
			},
		}
		resp, err := client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
	}
	lg.Info("Gateway started")
	waitctx.Go(e.ctx, func() {
		<-e.ctx.Done()
	})
}

type StartAgentOptions struct {
	ctx context.Context
}

type StartAgentOption func(*StartAgentOptions)

func (o *StartAgentOptions) Apply(opts ...StartAgentOption) {
	for _, op := range opts {
		op(o)
	}
}

func WithContext(ctx context.Context) StartAgentOption {
	return func(o *StartAgentOptions) {
		o.ctx = ctx
	}
}

func (e *Environment) StartAgent(id string, token *core.BootstrapToken, pins []string, opts ...StartAgentOption) (int, <-chan error) {
	if !e.enableGateway {
		e.Logger.Panic("gateway disabled")
	}
	options := &StartAgentOptions{
		ctx: context.Background(),
	}
	options.Apply(opts...)

	errC := make(chan error, 1)
	port, err := freeport.GetFreePort()
	if err != nil {
		panic(err)
	}

	if err := ident.RegisterProvider(id, func() ident.Provider {
		return NewTestIdentProvider(e.mockCtrl, id)
	}); err != nil {
		if !errors.Is(err, ident.ErrProviderAlreadyExists) {
			panic(err)
		}
	}

	agentConfig := &v1beta1.AgentConfig{
		Spec: v1beta1.AgentConfigSpec{
			ListenAddress:    fmt.Sprintf("localhost:%d", port),
			GatewayAddress:   fmt.Sprintf("https://localhost:%d", e.ports.Gateway),
			IdentityProvider: id,
			Storage: v1beta1.StorageSpec{
				Type: v1beta1.StorageTypeEtcd,
				Etcd: &v1beta1.EtcdStorageSpec{
					Endpoints: []string{fmt.Sprintf("http://localhost:%d", e.ports.Etcd)},
				},
			},
		},
	}

	publicKeyPins := []*pkp.PublicKeyPin{}
	for _, pin := range pins {
		d, err := pkp.DecodePin(pin)
		if err != nil {
			errC <- err
			return 0, errC
		}
		publicKeyPins = append(publicKeyPins, d)
	}
	bt, err := tokens.FromBootstrapToken(token)
	if err != nil {
		errC <- err
		return 0, errC
	}
	var a *agent.Agent
	mu := &sync.Mutex{}
	go func() {
		mu.Lock()
		a, err = agent.New(e.ctx, agentConfig,
			agent.WithBootstrapper(&bootstrap.ClientConfig{
				Capability: wellknown.CapabilityMetrics,
				Token:      bt,
				Pins:       publicKeyPins,
				Endpoint:   fmt.Sprintf("http://localhost:%d", e.ports.Gateway),
			}))
		if err != nil {
			errC <- err
			mu.Unlock()
			return
		}
		e.runningAgentsMu.Lock()
		e.runningAgents[id] = RunningAgent{
			Agent: a,
			Mutex: mu,
		}
		e.runningAgentsMu.Unlock()
		mu.Unlock()
		if err := a.ListenAndServe(); err != nil {
			errC <- err
		}
	}()
	waitctx.Go(e.ctx, func() {
		<-e.ctx.Done()
		mu.Lock()
		defer mu.Unlock()
		if a == nil {
			return
		}
		if err := a.Shutdown(); err != nil {
			errC <- err
		}
		e.runningAgentsMu.Lock()
		delete(e.runningAgents, id)
		e.runningAgentsMu.Unlock()
	})
	return port, errC
}

func (e *Environment) GetAgent(id string) RunningAgent {
	e.runningAgentsMu.Lock()
	defer e.runningAgentsMu.Unlock()
	return e.runningAgents[id]
}

func (e *Environment) GatewayTLSConfig() *tls.Config {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(*e.gatewayConfig.Spec.Certs.CACertData))
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    pool,
	}
}

func (e *Environment) GatewayConfig() *v1beta1.GatewayConfig {
	return e.gatewayConfig
}

func (e *Environment) EtcdClient() (*clientv3.Client, error) {
	if !e.enableEtcd {
		e.Logger.Panic("etcd disabled")
	}
	return clientv3.New(clientv3.Config{
		Endpoints: []string{fmt.Sprintf("http://localhost:%d", e.ports.Etcd)},
		Context:   e.ctx,
		Logger:    e.Logger.Desugar(),
	})
}

func (e *Environment) EtcdConfig() *v1beta1.EtcdStorageSpec {
	if !e.enableEtcd {
		e.Logger.Panic("etcd disabled")
	}
	return &v1beta1.EtcdStorageSpec{
		Endpoints: []string{fmt.Sprintf("http://localhost:%d", e.ports.Etcd)},
	}
}

func StartStandaloneTestEnvironment() {
	environment := &Environment{
		TestBin: "testbin/bin",
	}
	addAgent := func(rw http.ResponseWriter, r *http.Request) {
		Log.Infof("%s %s", r.Method, r.URL.Path)
		switch r.Method {
		case http.MethodPost:
			body := struct {
				Token string   `json:"token"`
				Pins  []string `json:"pins"`
			}{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				rw.WriteHeader(http.StatusBadRequest)
				rw.Write([]byte(err.Error()))
				return
			}
			token, err := tokens.ParseHex(body.Token)
			if err != nil {
				rw.WriteHeader(http.StatusBadRequest)
				rw.Write([]byte(err.Error()))
				return
			}
			port, errC := environment.StartAgent(uuid.New().String(), token.ToBootstrapToken(), body.Pins)
			select {
			case err := <-errC:
				rw.WriteHeader(http.StatusInternalServerError)
				rw.Write([]byte(err.Error()))
				return
			case <-time.After(time.Second):
			}
			environment.StartPrometheus(port)
			rw.WriteHeader(http.StatusOK)
			rw.Write([]byte(fmt.Sprintf("%d", port)))
		}
	}
	webui.AddExtraHandler("/opni-test/agents", addAgent)
	http.HandleFunc("/agents", addAgent)
	if err := environment.Start(); err != nil {
		panic(err)
	}
	go func() {
		addr := fmt.Sprintf("127.0.0.1:%d", environment.ports.TestEnvironment)
		Log.Infof(chalk.Green.Color("Test environment API listening on %s"), addr)
		if err := http.ListenAndServe(addr, nil); err != nil {
			panic(err)
		}
	}()
	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt)
	Log.Info(chalk.Blue.Color("Press (ctrl+c) to stop test environment"))
	// listen for spacebar on stdin
	t, err := tty.Open()
	if err == nil {
		Log.Info(chalk.Blue.Color("Press (space) to open the web dashboard"))
		go func() {
			for {
				rn, err := t.ReadRune()
				if err != nil {
					Log.Fatal(err)
				}
				if rn == ' ' {
					if err := browser.OpenURL(fmt.Sprintf("http://localhost:%d", environment.ports.ManagementWeb)); err != nil {
						Log.Error(err)
					}
				}
			}
		}()
	}
	<-c
	Log.Info("\nStopping test environment")
	if err := environment.Stop(); err != nil {
		panic(err)
	}
}
