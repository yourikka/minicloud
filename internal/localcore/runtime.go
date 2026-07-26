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
	DefaultSyncInterval = time.Second
	MinSyncInterval     = 10 * time.Millisecond
	MaxSyncInterval     = workerregistry.DefaultHeartbeatInterval
	defaultWorkerID     = "local-worker"
	defaultEndpoint     = "local-worker.internal:7443"
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
	SyncInterval     time.Duration
	WorkerID         string
	WorkerMemoryMiB  uint64
	WorkerSlots      uint64
	OnError          func(error)
}

// Runtime owns every resource in the Local Core data plane. The Controller is
// exposed for an outer management API while all mutable stores remain private.
type Runtime struct {
	controller   *localcontroller.Controller
	reconciler   *localworker.Reconciler
	synchronizer *localserving.Synchronizer
	handler      *gatewayhttp.Handler
	server       *gatewayhttp.Server
	registry     *workerregistry.Registry
	agent        *workeragent.Agent
	engine       *wasmexec.Engine
	artifacts    *artifact.Store
	syncInterval time.Duration
	address      string
	connection   servingauth.ControlConnection
	session      servingauth.WorkerSession
	shutdown     time.Duration
	onError      func(error)

	runMu      sync.Mutex
	running    bool
	convergeMu sync.Mutex
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
	connection := servingauth.ControlConnection{
		ConnectionID: connectionID, SessionEpoch: session.SessionEpoch, DiscoveryEpoch: 1,
	}
	reconciler, err := localworker.NewReconciler(localworker.ReconcilerConfig{
		Assignments: controller,
		States:      controller,
		Workers:     registryWorkerSource{registry: registry, session: session},
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

	agentOwned = false
	return &Runtime{
		controller: controller, reconciler: reconciler, synchronizer: synchronizer,
		handler: handler, server: server, registry: registry, agent: agent, engine: engine, artifacts: artifacts,
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

// Converge refreshes the local Worker lease, reconciles committed intent, and
// publishes one complete serving view even when reconciliation reports errors.
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
	reconcileErr := r.reconciler.Reconcile(ctx)
	syncErr := r.synchronizer.FullSync(ctx)
	return errors.Join(heartbeatErr, evaluateErr, reconcileErr, syncErr)
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

	loopContext, cancel := context.WithCancel(ctx)
	defer cancel()
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		r.runConvergenceLoop(loopContext)
	}()
	serveDone := make(chan error, 1)
	go func() { serveDone <- r.server.Serve(listener) }()

	select {
	case serveErr := <-serveDone:
		cancel()
		<-loopDone
		return serveErr
	case <-ctx.Done():
		cancel()
		shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), r.shutdown)
		shutdownErr := r.server.Shutdown(shutdownContext)
		shutdownCancel()
		serveErr := <-serveDone
		<-loopDone
		return errors.Join(shutdownErr, serveErr)
	}
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
