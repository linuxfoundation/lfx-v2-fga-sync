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

// server holds the lifecycle state for a single run of the service. All fields
// are set during startup inside run() before any subscription or shutdown code
// reads them, so no synchronization is needed beyond the sequential startup
// order itself — which is now visible at the call sites rather than hidden in
// package-level assignments.
type server struct {
	natsConn   *nats.Conn
	jsConn     jetstream.JetStream
	httpServer *http.Server

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

	// Create a wait group which is used to wait while draining (gracefully
	// closing) a connection.
	gracefulCloseWG := sync.WaitGroup{}

	// Support graceful shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	// Create NATS connection.
	gracefulCloseWG.Add(1)
	var natsCloseOnce sync.Once
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
			natsCloseOnce.Do(gracefulCloseWG.Done)
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
	logger.With("url", natsURL).Info("NATS client created")

	// Register HTTP handlers and start the listener only after srv.natsConn is
	// set, so /readyz never races with startup assignment.
	srv.createHTTPHandlers()
	srv.startHTTPListener(bind, port)

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
		fgaService: FgaService{
			client:      fgaClient,
			cacheBucket: cacheBucket,
			useCache:    useCache,
		},
	}

	if err = srv.createQueueSubscriptions(handlerService); err != nil {
		return fmt.Errorf("error creating queue subscriptions: %w", err)
	}

	if err = startMaxDeliveryAdvisorySubscription(ctx, srv.natsConn, srv.jsConn); err != nil {
		return fmt.Errorf("error starting max-delivery advisory subscription: %w", err)
	}

	accessMutationConsumer, err := startAccessMutationConsumer(ctx, srv.jsConn, handlerService)
	if err != nil {
		return fmt.Errorf("error starting access mutation consumer: %w", err)
	}

	// This next line blocks until SIGINT or SIGTERM is received, or NATS closes.
	<-done

	// shutdownDeadline is a single overall budget shared by every remaining
	// shutdown phase (access-mutation consumer stop, subscription drain/wait,
	// and waiting for the NATS connection to close). Each phase below waits
	// only for whatever is left of this one deadline instead of getting its
	// own independent gracefulShutdownSeconds timeout, so the phases cannot
	// compound into a multiple of gracefulShutdownSeconds that exceeds the
	// pod's terminationGracePeriodSeconds.
	shutdownDeadline := time.Now().Add(gracefulShutdownSeconds * time.Second)

	stopAccessMutationConsumer(accessMutationConsumer, cancel, shutdownDeadline)

	// Stop new deliveries on each plain (non-JetStream) subscription
	// individually, before touching the connection itself. QueueSubscribe
	// callbacks hand off to a goroutine and return immediately, so draining
	// the whole connection here would let natsConn.Drain() consider every
	// subject drained -- and close the connection -- long before those
	// goroutines finish, causing in-flight access-check/read-tuples replies
	// to fail against an already-closed connection. Draining just the
	// subscriptions stops new work without closing the connection those
	// goroutines still need to call Respond() on.
	srv.drainPlainSubscriptions()

	// sub.Drain() above unsubscribes but returns before nats.go's internal
	// delivery loop has necessarily dispatched every already-queued message
	// to its callback. Without this barrier, waitForSubscriptionWorkers could
	// observe subscriptionWG at zero and return before one of those pending
	// callbacks ever reaches subscriptionWG.Add(1), letting shutdown proceed
	// to close the connection out from under that late-arriving worker.
	// natsConn.Barrier schedules a marker behind every currently registered
	// subscription's queue, so waiting for it guarantees every message queued
	// at drain time has already reached its callback -- and thus already
	// called Add -- before subscriptionWG.Wait() runs.
	//
	// If the barrier times out or natsConn.Barrier itself errors, that
	// guarantee no longer holds: a late callback could still be about to call
	// Add. Calling subscriptionWG.Wait() in that state risks observing the
	// counter at zero and returning before that Add happens, which is
	// undefined WaitGroup usage. So skip the worker wait entirely and fall
	// through to draining the connection, rather than trusting a count we can
	// no longer verify is complete.
	if srv.waitForSubscriptionAdmission(time.Until(shutdownDeadline)) {
		// Now wait for the goroutines those subscriptions handed off to, using
		// whatever is left of the shutdown budget, while the connection is
		// still open so their replies can still be sent.
		waitForSubscriptionWorkers(&srv.subscriptionWG, time.Until(shutdownDeadline))
	} else {
		logger.Warn("subscription admission barrier did not complete; skipping in-flight worker wait")
	}

	// Every plain-subscription worker is done (or the wait above gave up on
	// a stuck one); it is now safe to drain and close the connection.
	if !srv.natsConn.IsClosed() && !srv.natsConn.IsDraining() {
		logger.Info("draining NATS connections")
		if err = srv.natsConn.Drain(); err != nil {
			return fmt.Errorf("error draining NATS connection: %w", err)
		}
	}

	// Wait for the graceful shutdown steps to complete, bounded by whatever
	// is left of shutdownDeadline rather than blocking indefinitely if the
	// NATS connection never reports itself closed.
	waitForGracefulClose(&gracefulCloseWG, time.Until(shutdownDeadline))

	// Immediately close the HTTP server after graceful shutdown has finished.
	if err = srv.httpServer.Close(); err != nil {
		logger.With(errKey, err).Error("http listener error on close")
	}

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

// createHTTPHandlers creates HTTP handlers for health checks.
func (s *server) createHTTPHandlers() {
	// Support GET/POST monitoring "ping".
	http.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) {
		// This always returns OK as long as the process is running. NATS
		// reconnects indefinitely rather than exiting on connection loss, so
		// connectivity health is reported via /readyz instead.
		_, err := fmt.Fprintf(w, "OK\n")
		if err != nil {
			logger.With(errKey, err).Error("error writing to response writer")
		}
	})

	// Basic health check.
	http.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if s.natsConn == nil {
			http.Error(w, "no NATS connection", http.StatusServiceUnavailable)
			return
		}
		if !s.natsConn.IsConnected() || s.natsConn.IsDraining() {
			http.Error(w, "NATS connection not ready", http.StatusServiceUnavailable)
			return
		}
		_, err := fmt.Fprintf(w, "OK\n")
		if err != nil {
			logger.With(errKey, err).Error("error writing to response writer")
		}
	})
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
