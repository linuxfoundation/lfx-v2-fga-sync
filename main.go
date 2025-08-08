// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// The fga-sync service.
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
	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// The slog key for errors.
	errKey            = "error"
	defaultListenPort = "8080"
	// gracefulShutdownSeconds should be higher than NATS client
	// request timeout, and lower than the pod or liveness probe's
	// terminationGracePeriodSeconds.
	gracefulShutdownSeconds = 25
)

var (
	logger          *slog.Logger
	httpServer      *http.Server
	natsURL         string
	natsConn        *nats.Conn
	jetstreamConn   jetstream.JetStream
	cacheBucketName string
	// TODO: improve the configuration of the service to use dependency injection instead of global variables
	useCache bool
)

func init() {
	natsURL = os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = "nats://nats:4222"
	}
	cacheBucketName = os.Getenv("CACHE_BUCKET")
	if cacheBucketName == "" {
		cacheBucketName = "fga-sync-cache"
	}
	useCacheStr := os.Getenv("USE_CACHE")
	if useCacheStr == "true" {
		useCache = true
	}
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

	logger = slog.New(slog.NewJSONHandler(os.Stdout, logOptions))
	slog.SetDefault(logger)

	// Create an OpenFGA client.
	fgaClient, err := connectFga()
	if err != nil {
		logger.With(errKey, err).Error("error creating OpenFGA client")
		os.Exit(1)
	}

	logger.With("url", os.Getenv("OPENFGA_API_URL")).Info("OpenFGA client created")

	// Create HTTP handlers for health checks.
	createHTTPHandlers()

	startHTTPListener(*bind, *port)

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
	natsConn, err = nats.Connect(
		natsURL,
		nats.DrainTimeout(gracefulShutdownSeconds*time.Second),
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
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
			if ctx.Err() != nil {
				// If our parent background context has already been canceled, this is
				// a graceful shutdown. Decrement the wait group but do not exit, to
				// allow other graceful shutdown steps to complete.
				logger.Info("NATS closed handler called during graceful shutdown")
				gracefulCloseWG.Done()
				return
			}
			// Otherwise, this handler means that max reconnect attempts have been
			// exhausted.
			logger.With(
				"lastError", nc.LastError(),
				"stats", nc.Stats(),
			).Error("NATS max-reconnects exhausted; connection closed")
			// Send a synthetic interrupt and give any graceful-shutdown tasks 5
			// seconds to clean up.
			done <- os.Interrupt
			time.Sleep(5 * time.Second)
			// Exit with an error instead of decrementing the wait group.
			os.Exit(1)
		}),
	)
	if err != nil {
		logger.With(errKey, err).Error("error creating NATS client")
		return
	}
	logger.With("url", natsURL).Info("NATS client created")

	jetstreamConn, err = jetstream.New(natsConn)
	if err != nil {
		logger.With(errKey, err).Error("error creating JetStream client")
		return
	}
	cacheBucket, err := jetstreamConn.KeyValue(context.Background(), cacheBucketName)
	if err != nil {
		logger.With(errKey, err).Error("error binding to cache bucket")
		return
	}

	handlerService := HandlerService{
		fgaService: FgaService{
			client:      fgaClient,
			cacheBucket: cacheBucket,
		},
	}

	if err = createQueueSubscriptions(handlerService); err != nil {
		logger.With(errKey, err).Error("error creating queue subscriptions")
		return
	}

	// This next line blocks until SIGINT or SIGTERM is received, or NATS disconnects.
	<-done

	// Cancel the background context.
	cancel()

	// Drain the connection, which will drain all subscriptions, then close the
	// connection when complete.
	if !natsConn.IsClosed() && !natsConn.IsDraining() {
		logger.Info("draining NATS connections")
		if err = natsConn.Drain(); err != nil {
			logger.With(errKey, err).Error("error draining NATS connection")
			return
		}
	}

	// Wait for the graceful shutdown steps to complete.
	gracefulCloseWG.Wait()

	// Immediately close the HTTP server after graceful shutdown has finished.
	if err = httpServer.Close(); err != nil {
		logger.With(errKey, err).Error("http listener error on close")
	}
}

func startHTTPListener(bind, port string) {
	// Add an http listener for health checks. This server does NOT participate
	// in the graceful shutdown process; we want it to stay up until the process
	// is killed, to avoid liveness checks failing during the graceful shutdown.
	var addr string
	if bind == "*" {
		addr = ":" + port
	} else {
		addr = bind + ":" + port
	}
	httpServer = &http.Server{
		Addr:              addr,
		Handler:           http.DefaultServeMux,
		ReadHeaderTimeout: 3 * time.Second,
	}
	go func() {
		logger.Info("starting HTTP server", "addr", addr)
		err := httpServer.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			logger.With(errKey, err).Error("http listener error")
		}
	}()
}

// createHTTPHandlers creates HTTP handlers for health checks.
func createHTTPHandlers() {
	// Support GET/POST monitoring "ping".
	http.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) {
		// This always returns as long as the service is still running. As this
		// endpoint is expected to be used as a Kubernetes liveness check, this
		// service must likewise self-detect non-recoverable errors and
		// self-terminate.
		_, err := fmt.Fprintf(w, "OK\n")
		if err != nil {
			logger.With(errKey, err).Error("error writing to response writer")
		}
	})

	// Basic health check.
	http.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if natsConn == nil {
			http.Error(w, "no NATS connection", http.StatusServiceUnavailable)
			return
		}
		if !natsConn.IsConnected() || natsConn.IsDraining() {
			http.Error(w, "NATS connection not ready", http.StatusServiceUnavailable)
			return
		}
		_, err := fmt.Fprintf(w, "OK\n")
		if err != nil {
			logger.With(errKey, err).Error("error writing to response writer")
		}
	})
}

// HandlerFunc defines a message handler function type.
type HandlerFunc func(INatsMsg) error

// subscriptionConfig defines a NATS subscription configuration.
type subscriptionConfig struct {
	subject     string
	handler     HandlerFunc
	description string
}

// subscribeToSubject subscribes to a single NATS subject with error handling and logging.
func subscribeToSubject(subject, description, queue string, handler HandlerFunc) error {
	if _, err := natsConn.QueueSubscribe(subject, queue, func(msg *nats.Msg) {
		if errHandler := handler(&NatsMsg{msg}); errHandler != nil {
			logger.Error("error handling "+description+" request",
				errKey, errHandler,
				"subject", subject,
				"queue", queue,
			)
		}
	}); err != nil {
		logger.With(
			errKey, err,
			"subject", subject,
			"queue", queue,
		).Error("error subscribing to NATS subject")
		return err
	}
	logger.With("subject", subject).Info("subscribed to NATS subject")
	return nil
}

// createQueueSubscriptions creates queue subscriptions for the NATS subjects.
func createQueueSubscriptions(handlerService HandlerService) error {
	queue := constants.FgaSyncQueue

	// Define all subscriptions in a slice for easy maintenance
	subscriptions := []subscriptionConfig{
		{
			subject:     constants.AccessCheckSubject,
			handler:     handlerService.accessCheckHandler,
			description: "access check",
		},
		{
			subject:     constants.ProjectUpdateAccessSubject,
			handler:     handlerService.projectUpdateAccessHandler,
			description: "project update access",
		},
		{
			subject:     constants.ProjectDeleteAllAccessSubject,
			handler:     handlerService.projectDeleteAllAccessHandler,
			description: "project delete all access",
		},
		{
			subject:     constants.MeetingUpdateAccessSubject,
			handler:     handlerService.meetingUpdateAccessHandler,
			description: "meeting update access",
		},
		{
			subject:     constants.MeetingDeleteAllAccessSubject,
			handler:     handlerService.meetingDeleteAllAccessHandler,
			description: "meeting delete all access",
		},
		{
			subject:     constants.MeetingRegistrantPutSubject,
			handler:     handlerService.meetingRegistrantPutHandler,
			description: "meeting registrant put",
		},
		{
			subject:     constants.MeetingRegistrantRemoveSubject,
			handler:     handlerService.meetingRegistrantRemoveHandler,
			description: "meeting registrant remove",
		},
	}

	// Subscribe to each subject using the helper function
	for _, config := range subscriptions {
		if err := subscribeToSubject(config.subject, config.description, queue, config.handler); err != nil {
			return err
		}
	}

	return nil
}
