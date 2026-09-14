// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package main provides the fga-sync service entry point and supporting types.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/utils"
	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	slogotel "github.com/remychantenay/slog-otel"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/codes"
)

const (
	// The slog key for errors.
	errKey            = "error"
	defaultListenPort = "8080"
	// gracefulShutdownSeconds is the total shared budget for every shutdown
	// phase after the stop signal is received (access-mutation consumer
	// stop, subscription drain/wait, and waiting for the NATS connection to
	// close -- see the shutdownDeadline comment in run()), not a per-phase
	// timeout. It should be higher than NATS client request timeout, and low
	// enough that this budget plus the deferred OpenTelemetry shutdown
	// timeout still fits under the pod or liveness probe's
	// terminationGracePeriodSeconds.
	gracefulShutdownSeconds = 25
	// subscriptionConcurrency bounds how many access-check/read-tuples
	// handler invocations may run concurrently. QueueSubscribe otherwise
	// dispatches each subject on a single goroutine, so a handler that waits
	// on OpenFGA serializes every request behind it; these two subjects have
	// no ordering requirement between distinct messages, unlike the
	// access-mutation JetStream consumer (see access_mutation.go), so
	// bounded concurrency is safe here. Sized to match fgaHTTPMaxConnsPerHost
	// in fga.go so handler concurrency and the OpenFGA connection pool scale
	// together.
	subscriptionConcurrency = fgaHTTPMaxConnsPerHost
)

// Build-time variables set via ldflags
var (
	Version   = "dev"
	BuildTime = "unknown"
	GitCommit = "unknown"
)

var logger *slog.Logger

// natsReadyChecker is the subset of *nats.Conn that /readyz needs. Keeping it
// as an interface allows tests to inject a stub without a real NATS server.
type natsReadyChecker interface {
	IsConnected() bool
	IsDraining() bool
}

// server holds the lifecycle state for a single run of the service.
//
// Concurrency contract: natsConn, natsChecker, jsConn, httpServer,
// subscriptionSem, and plainSubscriptions are written once during sequential
// startup in run(). The HTTP listener starts early (so /livez is available
// during initialization), meaning those fields are assigned after the serving
// goroutine is already running. Safety for HTTP handlers that read them (e.g.
// /readyz) comes from the ready atomic gate below, not from startup ordering.
// ready is the sole synchronization point: HTTP handlers that touch lifecycle
// fields must observe ready.Load() == true first. It is cleared on shutdown so
// /readyz returns 503 while the service is draining.
type server struct {
	natsConn    *nats.Conn
	natsChecker natsReadyChecker // same value as natsConn; separate for testability
	jsConn      jetstream.JetStream
	httpServer  *http.Server

	// ready is the atomic publish point for HTTP handlers. It is set true
	// after all NATS subscriptions and the JetStream consumer are registered,
	// and cleared at the start of shutdown. /readyz gates on this so
	// Kubernetes only routes traffic when the service can process messages.
	ready atomic.Bool

	// subscriptionSem bounds concurrent handler invocations across all plain
	// (non-JetStream) NATS subscriptions to subscriptionConcurrency.
	// subscriptionWG tracks in-flight handler goroutines so shutdown can wait
	// for them; NATS considers a message "processed" as soon as the
	// QueueSubscribe callback returns, which happens immediately once work is
	// handed off to a goroutine, so natsConn.Drain() alone would not wait for
	// them.
	subscriptionSem chan struct{}
	subscriptionWG  sync.WaitGroup

	// plainSubscriptions collects every subscription created by
	// subscribeToSubject so shutdown can drain them individually (see
	// drainPlainSubscriptions). Populated once, sequentially, during startup
	// before any shutdown code runs, so it needs no synchronization.
	plainSubscriptions []*nats.Subscription

	// Shutdown coordination fields. All are written once during sequential
	// startup before the signal wait, so no synchronization with shutdown is
	// needed beyond the ready atomic gate.

	// closeWG is incremented once before the NATS connection is created and
	// is signaled by the NATS ClosedHandler when the connection finishes
	// draining. shutdown waits on it as the final phase of the drain sequence.
	closeWG       sync.WaitGroup
	natsCloseOnce sync.Once

	// consumer is the JetStream access-mutation consume loop; shutdown calls
	// its Stop method to drain the consumer before touching the connection.
	consumer accessMutationConsumeContext
	// cancel is the cancellation function for the consumer's context; shutdown
	// uses it after Stop to force-abort any still-running delivery attempt.
	cancel context.CancelFunc
}

// main parses optional flags and starts the NATS subscribers.
func main() {
	// Allow overriding the port by environmental variable as well as command
	// line argument.
	defaultPort := os.Getenv("PORT")
	if defaultPort == "" {
		defaultPort = defaultListenPort
	}
	var debug = flag.Bool("d", false, "enable debug logging")
	var port = flag.String("p", defaultPort, "health checks port")
	var bind = flag.String("bind", "*", "interface to bind on")

	flag.Usage = func() {
		flag.PrintDefaults()
		os.Exit(2)
	}
	flag.Parse()

	logOptions := &slog.HandlerOptions{}

	// Optional debug logging.
	if os.Getenv("DEBUG") != "" || *debug {
		logOptions.Level = slog.LevelDebug
		logOptions.AddSource = true
	}

	// Create JSON handler and wrap with slog-otel to add trace_id and span_id from context
	jsonHandler := slog.NewJSONHandler(os.Stdout, logOptions)
	otelHandler := slogotel.OtelHandler{Next: jsonHandler}
	logger = slog.New(otelHandler)
	slog.SetDefault(logger)

	if err := run(*bind, *port); err != nil {
		logger.With(errKey, err).Error("fatal error")
		os.Exit(1)
	}
}

// envOrDefault returns the value of the named environment variable, or
// fallback if it is unset or empty.
func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// run contains the main service logic. It is separated from main() so that
// deferred cleanup functions (e.g. OpenTelemetry shutdown) run before
// main() calls os.Exit on error.
func run(bind, port string) error {
	// Set up OpenTelemetry SDK.
	// Command-line/environment OTEL_SERVICE_VERSION takes precedence over
	// the build-time Version variable.
	otelConfig := utils.OTelConfigFromEnv()
	if otelConfig.ServiceVersion == "" {
		otelConfig.ServiceVersion = Version
	}
	otelShutdown, err := utils.SetupOTelSDKWithConfig(context.Background(), otelConfig)
	if err != nil {
		return fmt.Errorf("error setting up OpenTelemetry SDK: %w", err)
	}
	// Handle shutdown properly so nothing leaks.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), gracefulShutdownSeconds*time.Second)
		defer cancel()
		if shutdownErr := otelShutdown(ctx); shutdownErr != nil {
			logger.With(errKey, shutdownErr).Error("error shutting down OpenTelemetry SDK")
		}
	}()

	natsURL := envOrDefault("NATS_URL", "nats://nats:4222")
	cacheBucketName := envOrDefault("CACHE_BUCKET", constants.KVBucketNameSyncCache)

	// Create an OpenFGA client.
	fgaClient, err := connectFga()
	if err != nil {
		return fmt.Errorf("error creating OpenFGA client: %w", err)
	}

	logger.With("url", os.Getenv("OPENFGA_API_URL")).Info("OpenFGA client created")

	srv := &server{
		subscriptionSem: make(chan struct{}, subscriptionConcurrency),
	}

	// Register HTTP handlers and start the listener early so /livez is
	// available throughout startup. /readyz gates on srv.ready, which is set
	// only after all subscriptions are up (see below), so Kubernetes will not
	// route traffic until the service can actually process messages.
	srv.createHTTPHandlers()
	srv.startHTTPListener(bind, port)

	// Support graceful shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	srv.cancel = cancel
	defer cancel()
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	// Create NATS connection. closeWG is incremented here, before the
	// connection exists, so the ClosedHandler can always call Done without
	// racing against the Add.
	srv.closeWG.Add(1)
	srv.natsConn, err = nats.Connect(
		natsURL,
		nats.DrainTimeout(gracefulShutdownSeconds*time.Second),
		nats.MaxReconnects(-1),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				logger.With(errKey, err).Warn("NATS disconnected with error")
			} else {
				logger.Warn("NATS disconnected")
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			logger.With("url", nc.ConnectedUrl()).Info("NATS reconnected")
		}),
		nats.ErrorHandler(func(_ *nats.Conn, s *nats.Subscription, err error) {
			if s != nil {
				logger.With(errKey, err, "subject", s.Subject, "queue", s.Queue).Error("async NATS error")
			} else {
				logger.With(errKey, err).Error("async NATS error outside subscription")
			}
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			srv.natsCloseOnce.Do(srv.closeWG.Done)
			if ctx.Err() != nil {
				logger.Info("NATS closed handler called during graceful shutdown")
				return
			}

			logger.With(
				"lastError", nc.LastError(),
				"stats", nc.Stats(),
			).Error("NATS connection closed unexpectedly")
			select {
			case done <- os.Interrupt:
			default:
			}
		}),
	)
	if err != nil {
		return fmt.Errorf("error creating NATS client: %w", err)
	}
	srv.natsChecker = srv.natsConn
	logger.With("url", natsURL).Info("NATS client created")

	srv.jsConn, err = jetstream.New(srv.natsConn)
	if err != nil {
		return fmt.Errorf("error creating JetStream client: %w", err)
	}
	cacheBucket, err := srv.jsConn.KeyValue(context.Background(), cacheBucketName)
	if err != nil {
		return fmt.Errorf("error binding to cache bucket: %w", err)
	}

	useCache := os.Getenv("USE_CACHE") == trueString

	handlerService := HandlerService{
		fgaService: newFgaService(fgaClient, cacheBucket, useCache),
	}

	if err = srv.createQueueSubscriptions(handlerService); err != nil {
		return fmt.Errorf("error creating queue subscriptions: %w", err)
	}

	if err = startMaxDeliveryAdvisorySubscription(ctx, srv.natsConn, srv.jsConn); err != nil {
		return fmt.Errorf("error starting max-delivery advisory subscription: %w", err)
	}

	srv.consumer, err = startAccessMutationConsumer(ctx, srv.jsConn, handlerService)
	if err != nil {
		return fmt.Errorf("error starting access mutation consumer: %w", err)
	}

	// All subscriptions and consumers are registered — the service can now
	// process messages. Signal readiness so /readyz returns 200.
	srv.ready.Store(true)
	logger.Info("service ready")

	// Block until SIGINT, SIGTERM, or an unexpected NATS close.
	<-done

	// Clear readiness immediately so /readyz returns 503 while draining,
	// preventing Kubernetes from routing new traffic here.
	srv.ready.Store(false)

	shutdownDeadline := time.Now().Add(gracefulShutdownSeconds * time.Second)
	err = srv.shutdown(shutdownDeadline)
	if err != nil {
		return err
	}

	// HTTP server closes after NATS is fully drained.
	if err = srv.httpServer.Close(); err != nil {
		logger.With(errKey, err).Error("http listener error on close")
	}

	return nil
}

// shutdown runs the five-phase graceful drain sequence, bounded by deadline.
// It must be called exactly once, after srv.ready has been cleared.
//
// Phase ordering is load-bearing — see the inline comments for the invariants
// that each transition depends on:
//
//  1. Stop the JetStream access-mutation consumer (cancel its context after a
//     grace period so any in-flight delivery attempt can finish first).
//  2. Drain each plain NATS subscription individually to stop new deliveries
//     without closing the connection (which handler goroutines still need).
//  3. Wait for the NATS delivery-loop barrier, then for all in-flight handler
//     goroutines to finish.
//  4. Drain the NATS connection (safe now that all goroutines are done).
//  5. Wait for the NATS ClosedHandler to signal closeWG.
func (s *server) shutdown(deadline time.Time) error {
	// Phase 1: stop the JetStream consumer.
	stopAccessMutationConsumer(s.consumer, s.cancel, deadline)

	// Phase 2: stop new deliveries on plain subscriptions without closing the
	// connection; see drainPlainSubscriptions for why this is not
	// natsConn.Drain().
	s.drainPlainSubscriptions()

	// Phase 3: wait for every already-queued message to have called Add on
	// subscriptionWG (barrier), then wait for those goroutines to finish.
	// If the barrier fails, skip the worker wait rather than risk undefined
	// WaitGroup usage — see waitForSubscriptionAdmission for full rationale.
	if s.waitForSubscriptionAdmission(time.Until(deadline)) {
		waitForSubscriptionWorkers(&s.subscriptionWG, time.Until(deadline))
	} else {
		logger.Warn("subscription admission barrier did not complete; skipping in-flight worker wait")
	}

	// Phase 4: all plain-subscription workers are done; drain the connection.
	if !s.natsConn.IsClosed() && !s.natsConn.IsDraining() {
		logger.Info("draining NATS connections")
		if err := s.natsConn.Drain(); err != nil {
			return fmt.Errorf("error draining NATS connection: %w", err)
		}
	}

	// Phase 5: wait for the ClosedHandler to signal closeWG, bounded by
	// whatever budget remains so a connection that never closes cannot block
	// shutdown indefinitely.
	waitForGracefulClose(&s.closeWG, time.Until(deadline))
	return nil
}

// waitForGracefulClose waits for gracefulCloseWG (signaled by the NATS
// ClosedHandler once the connection finishes draining) up to timeout, logging
// and giving up rather than blocking shutdown forever if the connection never
// closes.
func waitForGracefulClose(wg *sync.WaitGroup, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		logger.Warn("timed out waiting for NATS connection to close during shutdown")
	}
}

func (s *server) startHTTPListener(bind, port string) {
	// Add an http listener for health checks. This server does NOT participate
	// in the graceful shutdown process; we want it to stay up until the process
	// is killed, to avoid liveness checks failing during the graceful shutdown.
	var addr string
	if bind == "*" {
		addr = ":" + port
	} else {
		addr = bind + ":" + port
	}
	// Wrap the handler with OpenTelemetry instrumentation
	handler := otelhttp.NewHandler(http.DefaultServeMux, "fga-sync",
		otelhttp.WithFilter(func(r *http.Request) bool {
			p := r.URL.Path
			return p != "/livez" && p != "/readyz"
		}),
	)

	s.httpServer = &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 3 * time.Second,
	}
	go func() {
		logger.Info("starting HTTP server", "addr", addr)
		err := s.httpServer.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			logger.With(errKey, err).Error("http listener error")
		}
	}()
}

// createHTTPHandlers registers the /livez and /readyz handlers on
// http.DefaultServeMux.
func (s *server) createHTTPHandlers() {
	http.HandleFunc("/livez", s.livezHandler)
	http.HandleFunc("/readyz", s.readyzHandler)
}

// livezHandler reports liveness. It always returns 200 as long as the process
// is running. NATS reconnects indefinitely rather than exiting on connection
// loss, so connectivity health is reported via /readyz instead.
func (s *server) livezHandler(w http.ResponseWriter, _ *http.Request) {
	_, err := fmt.Fprintf(w, "OK\n")
	if err != nil {
		logger.With(errKey, err).Error("error writing to response writer")
	}
}

// readyzHandler reports readiness. It returns 503 until all NATS subscriptions
// and the JetStream consumer are registered (ready=true), and again once the
// shutdown signal is received (ready=false), so Kubernetes stops routing
// traffic while the service is draining.
func (s *server) readyzHandler(w http.ResponseWriter, _ *http.Request) {
	if !s.ready.Load() {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	if s.natsChecker == nil || !s.natsChecker.IsConnected() || s.natsChecker.IsDraining() {
		http.Error(w, "NATS connection not ready", http.StatusServiceUnavailable)
		return
	}
	_, err := fmt.Fprintf(w, "OK\n")
	if err != nil {
		logger.With(errKey, err).Error("error writing to response writer")
	}
}

// drainPlainSubscriptions unsubscribes each plain (non-JetStream)
// subscription individually so no further messages are delivered, without
// touching the NATS connection itself. This is deliberately not
// natsConn.Drain(), which closes the connection as soon as every
// subscription reports itself drained -- see the call site in run() for why
// that would race with subscribeToSubject's handler goroutines.
func (s *server) drainPlainSubscriptions() {
	for _, sub := range s.plainSubscriptions {
		if err := sub.Drain(); err != nil {
			logger.With(errKey, err, "subject", sub.Subject).Warn("error draining NATS subscription")
		}
	}
}

// waitForSubscriptionAdmission blocks, up to timeout, until every
// subscribeToSubject message queued on natsConn's async subscriptions at
// call time has reached its callback (and thus called wg.Add on
// subscriptionWG) -- see the call site comment in run() for why sub.Drain()
// alone does not guarantee this. It reports whether that guarantee was
// established: false if natsConn.Barrier itself errored (e.g. the connection
// is already closed) or the wait timed out, in which case the caller must not
// treat subscriptionWG's count as trustworthy.
func (s *server) waitForSubscriptionAdmission(timeout time.Duration) bool {
	done := make(chan struct{})
	if err := s.natsConn.Barrier(func() { close(done) }); err != nil {
		logger.With(errKey, err).Warn("error scheduling NATS subscription admission barrier")
		return false
	}
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		logger.Warn("timed out waiting for NATS subscription admission barrier during shutdown")
		return false
	}
}

// waitForSubscriptionWorkers waits for in-flight subscribeToSubject handler
// goroutines tracked by wg to finish, up to timeout, logging and giving up
// rather than blocking shutdown forever if one is stuck.
func waitForSubscriptionWorkers(wg *sync.WaitGroup, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		logger.Warn("timed out waiting for in-flight NATS handler goroutines during shutdown")
	}
}

// HandlerFunc defines a message handler function type.
type HandlerFunc func(context.Context, INatsMsg) error

// subscriptionConfig defines a NATS subscription configuration.
type subscriptionConfig struct {
	subject     string
	handler     HandlerFunc
	description string
}

// subscribeToSubject subscribes to a single NATS subject with error handling and logging.
// Each message is handled on its own goroutine, bounded by subscriptionSem,
// so a slow handler (e.g. waiting on OpenFGA) no longer serializes every
// other message on the same subject.
func (s *server) subscribeToSubject(subject, description, queue string, handler HandlerFunc) error {
	sub, err := s.natsConn.QueueSubscribe(subject, queue, func(msg *nats.Msg) {
		s.subscriptionSem <- struct{}{}
		s.subscriptionWG.Add(1)
		go func() {
			defer s.subscriptionWG.Done()
			defer func() { <-s.subscriptionSem }()

			// A fresh background context is used as the extraction base, not
			// the service context, since this callback must not inherit
			// shutdown cancellation ahead of any explicit handling of that
			// signal.
			msgCtx, span := startConsumerSpan(context.Background(), msg.Header, subject)
			defer span.End()
			if errHandler := handler(msgCtx, &NatsMsg{msg}); errHandler != nil {
				span.RecordError(errHandler)
				span.SetStatus(codes.Error, errHandler.Error())
				logger.Error("error handling "+description+" request",
					errKey, errHandler,
					"subject", subject,
					"queue", queue,
				)
			}
		}()
	})
	if err != nil {
		logger.Error("error subscribing to NATS subject",
			errKey, err,
			"subject", subject,
			"queue", queue,
		)
		return err
	}
	s.plainSubscriptions = append(s.plainSubscriptions, sub)
	logger.Info("subscribed to NATS subject",
		"subject", subject,
		"queue", queue,
	)
	return nil
}

// createQueueSubscriptions creates queue subscriptions for the NATS subjects.
func (s *server) createQueueSubscriptions(handlerService HandlerService) error {
	queue := constants.FgaSyncQueue

	for _, config := range queueSubscriptionConfigs(handlerService) {
		if err := s.subscribeToSubject(config.subject, config.description, queue, config.handler); err != nil {
			return err
		}
	}

	return nil
}

// queueSubscriptionConfigs lists core NATS (non-JetStream) subscriptions only.
// member_put and member_remove moved to the shared JetStream access-mutation
// consumer in the fga-sync-jetstream-membership change and are deployed
// together with the widened stream subjects (see values.yaml
// accessMutationStream.subjects) in the same release; do not add them back
// here. See docs/runbooks/fga-sync-jetstream-cutover.md (Phase 2) for the
// preconditions that gate this deploy and the accepted risk of shipping the
// stream widening and this removal together.
func queueSubscriptionConfigs(handlerService HandlerService) []subscriptionConfig {
	return []subscriptionConfig{
		{
			subject:     constants.AccessCheckSubject,
			handler:     handlerService.accessCheckHandler,
			description: "access check",
		},
		{
			subject:     constants.ReadTuplesSubject,
			handler:     handlerService.readTuplesHandler,
			description: "read tuples",
		},
	}
}
