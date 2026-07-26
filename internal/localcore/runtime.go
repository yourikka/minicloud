// Package localcore composes the single-process MiniCloud development profile.
package localcore

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/yourikka/minicloud/internal/artifact"
	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/discovery"
	"github.com/yourikka/minicloud/internal/gatewaydiscovery"
	"github.com/yourikka/minicloud/internal/gatewayhttp"
	"github.com/yourikka/minicloud/internal/gatewayinvoke"
	"github.com/yourikka/minicloud/internal/localcontroller"
	"github.com/yourikka/minicloud/internal/localserving"
	"github.com/yourikka/minicloud/internal/localworker"
	"github.com/yourikka/minicloud/internal/managementhttp"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/scheduler"
	"github.com/yourikka/minicloud/internal/servingauth"
	"github.com/yourikka/minicloud/internal/validator"
	"github.com/yourikka/minicloud/internal/wasmexec"
	"github.com/yourikka/minicloud/internal/wasmprofile"
	"github.com/yourikka/minicloud/internal/workeragent"
	"github.com/yourikka/minicloud/internal/workercache"
	"github.com/yourikka/minicloud/internal/workerregistry"
)

const (
	DefaultSyncInterval      = time.Second
	MinSyncInterval          = 10 * time.Millisecond
	MaxSyncInterval          = workerregistry.DefaultHeartbeatInterval
	DefaultManagementAddress = "127.0.0.1:8081"
	defaultWorkerID          = "local-worker"
	defaultEndpoint          = "local-worker.internal:7443"
	resumeValidationLimit    = 2
)

// Config contains process-owned paths, listener settings, and bounded runtime
// resources. Zero-valued resource configs use their package defaults.
type Config struct {
	DataRoot         string
	ValidatorCommand string
	Validator        validator.Config
	Wasm             wasmexec.Config
	Cache            workercache.Config
	Agent            workeragent.Config
	Registry         workerregistry.Config
	HTTP             gatewayhttp.ServerConfig
	Management       ManagementConfig
	SyncInterval     time.Duration
	WorkerID         string
	WorkerMemoryMiB  uint64
	WorkerSlots      uint64
	OnError          func(error)
}

// ManagementConfig binds the authenticated management API to its own hardened
// listener, separate from the invocation listener. Management stays disabled
// until a token is configured.
type ManagementConfig struct {
	HTTP    gatewayhttp.ServerConfig
	Token   string
	Subject string
}

// Enabled reports whether this process exposes the management API.
func (c ManagementConfig) Enabled() bool {
	return c.Token != ""
}

// Runtime owns every resource in the Local Core data plane, including the
// optional authenticated management listener. All mutable stores stay private.
type Runtime struct {
	controller       *localcontroller.Controller
	reconciler       *localworker.Reconciler
	synchronizer     *localserving.Synchronizer
	handler          *gatewayhttp.Handler
	server           *gatewayhttp.Server
	managementServer *gatewayhttp.Server
	registry         *workerregistry.Registry
	agent            *workeragent.Agent
	engine           *wasmexec.Engine
	artifacts        *artifact.Store
	planner          *scheduler.Planner
	ids              *localcontroller.RandomIDSource
	workers          registryWorkerSource
	syncInterval     time.Duration
	address          string
	connection       servingauth.ControlConnection
	session          servingauth.WorkerSession
	shutdown         time.Duration
	onError          func(error)

	runMu      sync.Mutex
	running    bool
	convergeMu sync.Mutex

	managementMu      sync.Mutex
	managementAddress string
}

// New creates an empty Local Core without opening a network listener.
func New(ctx context.Context, config Config) (_ *Runtime, err error) {
	if ctx == nil {
		return nil, errors.New("local core context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	config, err = normalizeConfig(config)
	if err != nil {
		return nil, err
	}

	artifacts, err := artifact.Open(artifact.Config{
		Root:             filepath.Join(config.DataRoot, "artifacts"),
		MaxArtifactBytes: config.Validator.MaxArtifactBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("opening local artifact store: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, artifacts.Close())
		}
	}()

	validationClient, err := validator.New(config.Validator)
	if err != nil {
		return nil, fmt.Errorf("creating local validator client: %w", err)
	}
	engine, err := wasmexec.New(ctx, config.Wasm)
	if err != nil {
		return nil, fmt.Errorf("creating local wasm engine: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, engine.Close(context.WithoutCancel(ctx)))
		}
	}()

	config.Cache.Artifacts = artifacts
	config.Cache.Compiler = engine
	cache, err := workercache.New(config.Cache)
	if err != nil {
		return nil, fmt.Errorf("creating local worker cache: %w", err)
	}

	idSource := localcontroller.NewRandomIDSource(nil)
	bootID, err := idSource.NewID("boot")
	if err != nil {
		_ = cache.Close(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("creating local worker boot id: %w", err)
	}
	connectionID, err := idSource.NewID("connection")
	if err != nil {
		_ = cache.Close(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("creating local control connection id: %w", err)
	}
	session := servingauth.WorkerSession{
		WorkerID: config.WorkerID, BootID: bootID, SessionEpoch: 1,
	}
	config.Agent.Cache = cache
	config.Agent.Authorization.Worker = servingauth.WorkerProcess{
		WorkerID: session.WorkerID, BootID: session.BootID,
	}
	agent, err := workeragent.New(config.Agent)
	if err != nil {
		_ = cache.Close(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("creating local worker agent: %w", err)
	}
	agentOwned := true
	defer func() {
		if err != nil && agentOwned {
			err = errors.Join(err, agent.Close(context.WithoutCancel(ctx)))
		}
	}()

	controller, err := localcontroller.New(localcontroller.Config{
		Artifacts: artifacts,
		Validator: validationClient,
	})
	if err != nil {
		return nil, fmt.Errorf("creating local controller: %w", err)
	}
	worker, err := localWorkerSnapshot(session, engine.Profile(), config)
	if err != nil {
		return nil, err
	}
	registry, err := bootstrapRegistry(config.Registry, worker)
	if err != nil {
		return nil, err
	}
	workers := registryWorkerSource{registry: registry, session: session}
	planner, err := scheduler.New(scheduler.Config{})
	if err != nil {
		return nil, fmt.Errorf("creating local placement planner: %w", err)
	}
	if err := planner.InstallBarrier(localCoreBarrier); err != nil {
		return nil, fmt.Errorf("installing local placement barrier: %w", err)
	}
	connection := servingauth.ControlConnection{
		ConnectionID: connectionID, SessionEpoch: session.SessionEpoch, DiscoveryEpoch: 1,
	}
	reconciler, err := localworker.NewReconciler(localworker.ReconcilerConfig{
		Assignments: controller,
		States:      controller,
		Workers:     workers,
		Agent:       agent,
		Connection:  connection,
		Address:     defaultEndpoint,
	})
	if err != nil {
		return nil, fmt.Errorf("creating local worker reconciler: %w", err)
	}
	discoveryBuilder, err := discovery.New(discovery.Config{})
	if err != nil {
		return nil, fmt.Errorf("creating local discovery builder: %w", err)
	}
	publisher, err := discovery.NewPublisher(connection.DiscoveryEpoch, discoveryBuilder)
	if err != nil {
		return nil, fmt.Errorf("creating local discovery publisher: %w", err)
	}
	discoveryStore, err := gatewaydiscovery.New(gatewaydiscovery.Config{})
	if err != nil {
		return nil, fmt.Errorf("creating local gateway discovery store: %w", err)
	}
	synchronizer, err := localserving.New(localserving.Config{
		States: controller, Candidates: reconciler, Publisher: publisher, Store: discoveryStore,
	})
	if err != nil {
		return nil, fmt.Errorf("creating local serving synchronizer: %w", err)
	}
	resolver, err := localworker.NewResolver(defaultEndpoint, agent)
	if err != nil {
		return nil, fmt.Errorf("creating local worker resolver: %w", err)
	}
	gateway, err := gatewayinvoke.New(gatewayinvoke.Config{Discovery: discoveryStore, Resolver: resolver})
	if err != nil {
		return nil, fmt.Errorf("creating local invocation gateway: %w", err)
	}
	handler, err := gatewayhttp.New(gatewayhttp.Config{Discovery: discoveryStore, Gateway: gateway})
	if err != nil {
		return nil, fmt.Errorf("creating local HTTP gateway: %w", err)
	}
	config.HTTP.Handler = handler
	server, err := gatewayhttp.NewServer(config.HTTP)
	if err != nil {
		return nil, fmt.Errorf("creating local HTTP server: %w", err)
	}
	var managementServer *gatewayhttp.Server
	if config.Management.Enabled() {
		managementHandler, err := managementhttp.New(managementhttp.Config{
			Controller:       controller,
			Token:            config.Management.Token,
			Subject:          config.Management.Subject,
			MaxArtifactBytes: config.Validator.MaxArtifactBytes,
		})
		if err != nil {
			return nil, fmt.Errorf("creating local management handler: %w", err)
		}
		config.Management.HTTP.Handler = managementHandler
		managementServer, err = gatewayhttp.NewServer(config.Management.HTTP)
		if err != nil {
			return nil, fmt.Errorf("creating local management server: %w", err)
		}
	}

	agentOwned = false
	return &Runtime{
		controller: controller, reconciler: reconciler, synchronizer: synchronizer,
		handler: handler, server: server, managementServer: managementServer,
		registry: registry, agent: agent, engine: engine, artifacts: artifacts,
		planner: planner, ids: idSource, workers: workers,
		syncInterval: config.SyncInterval, address: server.Address(), connection: connection, session: session,
		shutdown: config.HTTP.ShutdownTimeout, onError: config.OnError,
	}, nil
}

// Controller returns the single write coordinator for a future management API.
func (r *Runtime) Controller() *localcontroller.Controller {
	if r == nil {
		return nil
	}
	return r.controller
}

// Handler returns the invocation boundary for in-process tests and embedding.
func (r *Runtime) Handler() http.Handler {
	if r == nil {
		return nil
	}
	return r.handler
}

// Converge refreshes the local Worker lease, commits missing placement intent,
// reconciles committed intent, and publishes one complete serving view even
// when reconciliation reports errors.
func (r *Runtime) Converge(ctx context.Context) error {
	if ctx == nil {
		return errors.New("local core converge context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r == nil {
		return errors.New("local core converge dependencies are required")
	}
	r.convergeMu.Lock()
	defer r.convergeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.registry == nil || r.reconciler == nil || r.synchronizer == nil ||
		r.agent == nil || r.engine == nil || r.artifacts == nil {
		return errors.New("local core converge dependencies are required")
	}
	heartbeatErr := r.registry.Heartbeat(r.session)
	evaluateErr := r.registry.Evaluate()
	placeErr := r.placeLocalAssignments(ctx)
	reconcileErr := r.reconciler.Reconcile(ctx)
	syncErr := r.synchronizer.FullSync(ctx)
	return errors.Join(heartbeatErr, evaluateErr, placeErr, reconcileErr, syncErr)
}

// Run listens on the configured address and serves until ctx is cancelled.
func (r *Runtime) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("local core run context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r == nil || r.server == nil {
		return errors.New("local core runtime dependencies are required")
	}
	listener, err := net.Listen("tcp", r.address)
	if err != nil {
		return fmt.Errorf("listening for local core requests: %w", err)
	}
	defer listener.Close()
	return r.Serve(ctx, listener)
}

// Serve runs on a supplied listener, primarily for embedding and process tests.
// A configured management server opens its own listener for the same lifetime.
func (r *Runtime) Serve(ctx context.Context, listener net.Listener) error {
	if ctx == nil {
		return errors.New("local core serve context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if listener == nil {
		return errors.New("local core listener is required")
	}
	if err := r.beginRun(); err != nil {
		return err
	}
	defer r.endRun()
	if err := r.Converge(ctx); err != nil {
		return fmt.Errorf("performing initial local core convergence: %w", err)
	}

	var managementListener net.Listener
	if r.managementServer != nil {
		var err error
		managementListener, err = net.Listen("tcp", r.managementServer.Address())
		if err != nil {
			return fmt.Errorf("listening for local management requests: %w", err)
		}
		defer managementListener.Close()
		r.setManagementAddress(managementListener.Addr().String())
		defer r.setManagementAddress("")
	}

	loopContext, cancel := context.WithCancel(ctx)
	defer cancel()
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		r.runConvergenceLoop(loopContext)
	}()
	resumeDone := make(chan struct{})
	go func() {
		defer close(resumeDone)
		r.runValidationResumeLoop(loopContext)
	}()
	serveDone := make(chan error, 1)
	go func() { serveDone <- r.server.Serve(listener) }()
	var managementDone chan error
	if managementListener != nil {
		managementDone = make(chan error, 1)
		go func() { managementDone <- r.managementServer.Serve(managementListener) }()
	}

	select {
	case serveErr := <-serveDone:
		cancel()
		managementErr := r.stopManagement(managementDone)
		<-loopDone
		<-resumeDone
		return errors.Join(serveErr, managementErr)
	case managementErr := <-managementDone:
		cancel()
		serveErr := r.stopInvocation(serveDone)
		<-loopDone
		<-resumeDone
		return errors.Join(managementErr, serveErr)
	case <-ctx.Done():
		cancel()
		serveErr := r.stopInvocation(serveDone)
		managementErr := r.stopManagement(managementDone)
		<-loopDone
		<-resumeDone
		return errors.Join(serveErr, managementErr)
	}
}

// ManagementAddress returns the bound management listener address while the
// runtime serves, or an empty string when management is disabled or stopped.
func (r *Runtime) ManagementAddress() string {
	if r == nil {
		return ""
	}
	r.managementMu.Lock()
	defer r.managementMu.Unlock()
	return r.managementAddress
}

func (r *Runtime) setManagementAddress(address string) {
	r.managementMu.Lock()
	r.managementAddress = address
	r.managementMu.Unlock()
}

func (r *Runtime) stopInvocation(done chan error) error {
	shutdownContext, cancel := context.WithTimeout(context.Background(), r.shutdown)
	defer cancel()
	shutdownErr := r.server.Shutdown(shutdownContext)
	return errors.Join(shutdownErr, <-done)
}

// stopManagement gracefully stops a configured management server and drains
// its serve result. It is a no-op when management is disabled.
func (r *Runtime) stopManagement(done chan error) error {
	if r.managementServer == nil || done == nil {
		return nil
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), r.shutdown)
	defer cancel()
	shutdownErr := r.managementServer.Shutdown(shutdownContext)
	return errors.Join(shutdownErr, <-done)
}

// Close releases process resources after Run or Serve has returned. Failed
// stages remain owned so a later call can retry with a fresh context.
func (r *Runtime) Close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("local core close context is required")
	}
	if r == nil {
		return nil
	}
	r.runMu.Lock()
	defer r.runMu.Unlock()
	if r.running {
		return errors.New("local core runtime cannot close while serving")
	}
	r.convergeMu.Lock()
	defer r.convergeMu.Unlock()
	if r.agent != nil {
		r.agent.DisconnectControl(r.connection)
		if err := r.agent.Close(ctx); err != nil {
			return fmt.Errorf("closing local worker agent: %w", err)
		}
		r.agent = nil
	}
	if r.engine != nil {
		if err := r.engine.Close(ctx); err != nil {
			return fmt.Errorf("closing local wasm engine: %w", err)
		}
		r.engine = nil
	}
	if r.artifacts != nil {
		if err := r.artifacts.Close(); err != nil {
			return fmt.Errorf("closing local artifact store: %w", err)
		}
		r.artifacts = nil
	}
	return nil
}

func (r *Runtime) beginRun() error {
	if r == nil || r.server == nil || r.registry == nil || r.reconciler == nil || r.synchronizer == nil {
		return errors.New("local core runtime dependencies are required")
	}
	r.runMu.Lock()
	defer r.runMu.Unlock()
	if r.running {
		return errors.New("local core runtime is already serving")
	}
	if r.agent == nil || r.engine == nil || r.artifacts == nil {
		return errors.New("local core runtime is closed")
	}
	r.running = true
	return nil
}

func (r *Runtime) endRun() {
	r.runMu.Lock()
	r.running = false
	r.runMu.Unlock()
}

func (r *Runtime) runConvergenceLoop(ctx context.Context) {
	ticker := time.NewTicker(r.syncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.Converge(ctx); err != nil && !errors.Is(err, context.Canceled) && r.onError != nil {
				r.onError(err)
			}
		}
	}
}

// runValidationResumeLoop retries persisted Validating fences left by
// transient validator failures. It runs outside the convergence critical
// section so one slow validation cannot delay heartbeats or serving sync.
func (r *Runtime) runValidationResumeLoop(ctx context.Context) {
	interval := r.syncInterval
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err := r.controller.ResumePendingValidation(ctx, resumeValidationLimit)
			if err != nil && !errors.Is(err, context.Canceled) && r.onError != nil {
				r.onError(err)
			}
		}
	}
}

func normalizeConfig(config Config) (Config, error) {
	if config.DataRoot == "" {
		return Config{}, errors.New("local core data root is required")
	}
	if config.Validator.Command == "" {
		config.Validator.Command = config.ValidatorCommand
	}
	if config.Validator.Command == "" {
		return Config{}, errors.New("local core validator command is required")
	}
	if config.Validator.TempRoot == "" {
		config.Validator.TempRoot = filepath.Join(config.DataRoot, "validator")
	}
	if config.Validator.MaxArtifactBytes == 0 {
		config.Validator.MaxArtifactBytes = model.MaxArtifactBytes
	}
	if config.SyncInterval == 0 {
		config.SyncInterval = DefaultSyncInterval
	}
	if config.SyncInterval < MinSyncInterval || config.SyncInterval > MaxSyncInterval {
		return Config{}, errors.New("local core sync interval is outside bounds")
	}
	if config.WorkerID == "" {
		config.WorkerID = defaultWorkerID
	}
	if config.WorkerMemoryMiB == 0 {
		config.WorkerMemoryMiB = 512
	}
	if config.WorkerSlots == 0 {
		config.WorkerSlots = uint64(wasmexec.DefaultMaxConcurrent)
	}
	if config.HTTP.ShutdownTimeout == 0 {
		config.HTTP.ShutdownTimeout = gatewayhttp.DefaultShutdownTimeout
	}
	if config.Management.Enabled() && config.Management.HTTP.Address == "" {
		config.Management.HTTP.Address = DefaultManagementAddress
	}
	if !config.Management.Enabled() &&
		(config.Management.HTTP.Address != "" || config.Management.Subject != "") {
		return Config{}, errors.New("local core management configuration requires a management token")
	}
	return config, nil
}

func localWorkerSnapshot(
	session servingauth.WorkerSession,
	profile wasmprofile.Profile,
	config Config,
) (scheduler.WorkerSnapshot, error) {
	snapshot := scheduler.WorkerSnapshot{
		Session: session,
		Runtime: scheduler.RuntimeProfile{
			Name: wasmprofile.RuntimeName, Version: wasmprofile.RuntimeVersion,
			Engine: profile.Engine, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
			ABI: model.ABIWASICommandV1, HostAPI: model.HostAPIProfileNone,
			FeatureProfile: wasmprofile.FeatureProfile, MemoryMiB: profile.MemoryLimitMiB,
		},
		Intent: scheduler.IntentSchedulable, State: scheduler.SessionReady,
		Drain:    scheduler.DrainNotDraining,
		Capacity: scheduler.Capacity{MemoryMiB: config.WorkerMemoryMiB, Slots: config.WorkerSlots},
		Labels:   map[string]string{"profile": "local"},
		Cache: scheduler.CacheHints{
			Artifacts: map[digest.SHA256]struct{}{},
			Compiled:  map[workercache.Key]struct{}{},
		},
	}
	if err := snapshot.Validate(); err != nil {
		return scheduler.WorkerSnapshot{}, fmt.Errorf("validating local worker snapshot: %w", err)
	}
	return snapshot, nil
}

func bootstrapRegistry(
	config workerregistry.Config,
	worker scheduler.WorkerSnapshot,
) (*workerregistry.Registry, error) {
	registry, err := workerregistry.New(config)
	if err != nil {
		return nil, fmt.Errorf("creating local worker registry: %w", err)
	}
	registration := workerregistry.Registration{Session: worker.Session}
	if err := registry.CommitSession(registration); err != nil {
		return nil, fmt.Errorf("committing local worker session: %w", err)
	}
	if err := registry.Register(registration); err != nil {
		return nil, fmt.Errorf("registering local worker session: %w", err)
	}
	if err := registry.ReportInventory(workerregistry.Inventory{
		Revision: 1, Session: worker.Session, Runtime: worker.Runtime,
		Capacity: worker.Capacity, Labels: worker.Labels, Cache: worker.Cache,
	}); err != nil {
		return nil, fmt.Errorf("reporting local worker inventory: %w", err)
	}
	return registry, nil
}

type registryWorkerSource struct {
	registry *workerregistry.Registry
	session  servingauth.WorkerSession
}

func (s registryWorkerSource) WorkerSnapshot(ctx context.Context) (scheduler.WorkerSnapshot, error) {
	if ctx == nil {
		return scheduler.WorkerSnapshot{}, errors.New("local worker snapshot context is required")
	}
	if err := ctx.Err(); err != nil {
		return scheduler.WorkerSnapshot{}, err
	}
	if s.registry == nil {
		return scheduler.WorkerSnapshot{}, errors.New("local worker registry is required")
	}
	for _, worker := range s.registry.Snapshot().Workers {
		if worker.Registered && worker.Snapshot.Session == s.session {
			return worker.Snapshot, nil
		}
	}
	return scheduler.WorkerSnapshot{}, errors.New("local worker session is not registered")
}
