// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package main provides the fga-sync service entry point and supporting types.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
	"github.com/nats-io/nats.go/jetstream"
	openfga "github.com/openfga/go-sdk"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"

	. "github.com/openfga/go-sdk/client"
)

// Note: fga.go uses a dot-import for the OpenFGA SDK client package so that
// SDK types are available unqualified within this file. Handlers in other
// files use the qualified client.ClientTupleKey{} form instead.

const (
	// trueString is used for cache values representing allowed access.
	trueString = "true"

	// fgaHTTPMaxIdleConns and fgaHTTPMaxIdleConnsPerHost raise how many idle
	// connections the OpenFGA HTTP client keeps ready, well above Go's
	// http.DefaultTransport default (MaxIdleConnsPerHost: 2). Every request
	// goes to the same OpenFGA host, so under this service's concurrent
	// access-check load the default pool churned through TCP/TLS handshakes
	// instead of reusing connections. fgaHTTPMaxConnsPerHost additionally
	// caps the number of simultaneously active connections to that host,
	// which http.DefaultTransport otherwise leaves unbounded.
	fgaHTTPMaxIdleConns        = 100
	fgaHTTPMaxIdleConnsPerHost = 64
	fgaHTTPMaxConnsPerHost     = 64
)

// fgaHTTPTransport returns an *http.Transport matching http.DefaultTransport
// except for a connection pool sized for this service's OpenFGA call volume.
func fgaHTTPTransport() *http.Transport {
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		defaultTransport = &http.Transport{}
	}
	transport := defaultTransport.Clone()
	transport.MaxIdleConns = fgaHTTPMaxIdleConns
	transport.MaxIdleConnsPerHost = fgaHTTPMaxIdleConnsPerHost
	transport.MaxConnsPerHost = fgaHTTPMaxConnsPerHost
	return transport
}

// INatsKeyValue is a NATS KV interface needed for the FgaService cache layer.
type INatsKeyValue interface {
	Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error)
	Put(context.Context, string, []byte) (uint64, error)
	PutString(context.Context, string, string) (uint64, error)
}

// FgaService coordinates FGA authorization checks and tuple synchronization.
// It composes TupleStore (raw FGA I/O) and CacheLayer (JetStream KV caching)
// and exposes delegate methods for all I/O concerns so callers need only one
// type. The SyncObjectTuples and CheckRelationships methods are implemented
// here because they orchestrate both subsystems.
//
// Construct with newFgaService; do not use struct literals in new code.
type FgaService struct {
	store    TupleStore
	cache    CacheLayer
	useCache bool
}

// newFgaService creates a FgaService with the given FGA client, JetStream KV
// cache bucket, and cache-enabled flag.
func newFgaService(client IFgaClient, cacheBucket INatsKeyValue, useCache bool) FgaService {
	return FgaService{
		store:    TupleStore{client: client},
		cache:    CacheLayer{bucket: cacheBucket},
		useCache: useCache,
	}
}

// connectFga initializes the global shared fgaClient connection. This demo
// does not use or support authentication.
func connectFga() (IFgaClient, error) {
	fgaURL := os.Getenv("OPENFGA_API_URL")
	fgaStoreID := os.Getenv("OPENFGA_STORE_ID")
	fgaAuthModelID := os.Getenv("OPENFGA_AUTH_MODEL_ID")
	if fgaURL == "" {
		return nil, fmt.Errorf("OPENFGA_API_URL must be set")
	}
	if fgaStoreID == "" {
		return nil, fmt.Errorf("OPENFGA_STORE_ID must be set")
	}
	if fgaAuthModelID == "" {
		return nil, fmt.Errorf("OPENFGA_AUTH_MODEL_ID must be set")
	}
	fgaClient, err := NewSdkClient(&ClientConfiguration{
		ApiUrl:               fgaURL,
		StoreId:              fgaStoreID,
		AuthorizationModelId: fgaAuthModelID,
		HTTPClient: &http.Client{
			Transport: otelhttp.NewTransport(fgaHTTPTransport()),
		},
	})
	if err != nil {
		return nil, err
	}
	return FgaAdapter{OpenFgaClient: *fgaClient}, nil
}

// ── Delegate methods ──────────────────────────────────────────────────────────
//
// These methods forward to TupleStore for callers (handlers, tests) that hold
// only a FgaService. Write-path delegates also invalidate the cache after a
// successful OpenFGA write.

// ReadObjectTuples fetches all direct relationships for a given object.
func (s FgaService) ReadObjectTuples(ctx context.Context, object string) ([]openfga.Tuple, error) {
	return s.store.ReadObjectTuples(ctx, object)
}

// ReadUserTuples fetches all direct relationships for a given user across all
// objects of the specified type.
func (s FgaService) ReadUserTuples(ctx context.Context, user, objectType string) ([]openfga.Tuple, error) {
	return s.store.ReadUserTuples(ctx, user, objectType)
}

// ListObjectsByUserAndRelation finds all objects of a specific type that have
// a given relation to a user.
func (s FgaService) ListObjectsByUserAndRelation(
	ctx context.Context,
	objectType, relation, user string,
) ([]string, error) {
	return s.store.ListObjectsByUserAndRelation(ctx, objectType, relation, user)
}

// WriteAndDeleteTuples writes and/or deletes the given tuples to/from OpenFGA,
// batching at 100 ops/request, then invalidates the cache. It returns the
// tuple strings of any write tuples OpenFGA rejected as invalid.
//
// Cache invalidation happens even when the store returns an error: a
// multi-batch run may have committed earlier batches to OpenFGA before the
// failure, so skipping invalidation on a partial success would leave cache
// entries fresh for tuples that now exist in the store. Writing the per-pair
// markers is cheap and a spurious invalidation is far safer than serving
// stale auth results.
//
// Invalidation uses an independent short-lived context derived with
// context.WithoutCancel so that a canceled or deadline-exceeded parent context
// (e.g. from a batch-2 timeout after batch-1 committed) cannot prevent the
// invalidation markers from being written. Without this, a canceled ctx would
// cause bucket.Put to return immediately, leaving cached entries from
// committed writes fresh until some later successful mutation touching the
// same (object, relation) pair invalidates them.
func (s FgaService) WriteAndDeleteTuples(
	ctx context.Context,
	writes []ClientTupleKey,
	deletes []ClientTupleKeyWithoutCondition,
) ([]string, error) {
	skipped, storeErr := s.store.WriteAndDeleteTuples(ctx, writes, deletes)
	// Invalidate whenever there was anything to write/delete — even on error —
	// to cover partial multi-batch commits (see doc comment above). Scoped to
	// the (object, relation) pairs actually touched by this batch, not a
	// single global marker, so an unrelated concurrent batch's cache entries
	// are not needlessly invalidated.
	if len(writes) > 0 || len(deletes) > 0 {
		pairs := make([]invalidationPair, 0, len(writes)+len(deletes))
		for _, w := range writes {
			pairs = append(pairs, invalidationPair{object: w.Object, relation: w.Relation})
			pairs = append(pairs, expandCascadingPairs(w.Object, w.Relation)...)
		}
		for _, d := range deletes {
			pairs = append(pairs, invalidationPair{object: d.Object, relation: d.Relation})
			pairs = append(pairs, expandCascadingPairs(d.Object, d.Relation)...)
		}
		// Use a detached context with a short deadline so a canceled parent
		// (e.g. expired deadline after batch 1 committed) cannot block the Put.
		invCtx, invCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer invCancel()
		s.cache.invalidate(invCtx, pairs)
	}
	return skipped, storeErr
}

// WriteTuples writes the given tuples to OpenFGA and invalidates the cache.
func (s FgaService) WriteTuples(ctx context.Context, tuples []ClientTupleKey) error {
	_, err := s.WriteAndDeleteTuples(ctx, tuples, nil)
	return err
}

// DeleteTuples deletes the given tuples from OpenFGA and invalidates the cache.
func (s FgaService) DeleteTuples(ctx context.Context, tuples []ClientTupleKeyWithoutCondition) error {
	_, err := s.WriteAndDeleteTuples(ctx, nil, tuples)
	return err
}

// WriteTuple writes a single tuple to OpenFGA using simple string parameters.
func (s FgaService) WriteTuple(ctx context.Context, user, relation, object string) error {
	tuple := ClientTupleKey{User: user, Relation: relation, Object: object}
	return s.WriteTuples(ctx, []ClientTupleKey{tuple})
}

// DeleteTuple deletes a single tuple from OpenFGA using simple string parameters.
func (s FgaService) DeleteTuple(ctx context.Context, user, relation, object string) error {
	tuple := ClientTupleKeyWithoutCondition{User: user, Relation: relation, Object: object}
	return s.DeleteTuples(ctx, []ClientTupleKeyWithoutCondition{tuple})
}

// GetTuplesByUserAndObject returns all tuples for a specific user on a given object.
func (s FgaService) GetTuplesByUserAndObject(ctx context.Context, user, object string) ([]ClientTupleKey, error) {
	return s.store.GetTuplesByUserAndObject(ctx, user, object)
}

// GetTuplesByRelation returns tuples for a specific object filtered by relation.
func (s FgaService) GetTuplesByRelation(ctx context.Context, object, relation string) ([]openfga.Tuple, error) {
	return s.store.GetTuplesByRelation(ctx, object, relation)
}

// DeleteTuplesByUserAndObject deletes all tuples for a specific user and
// object, reading first to discover which relations exist.
func (s FgaService) DeleteTuplesByUserAndObject(ctx context.Context, user, object string) error {
	tuples, err := s.store.GetTuplesByUserAndObject(ctx, user, object)
	if err != nil {
		return err
	}
	tuplesWithoutConditions := make([]ClientTupleKeyWithoutCondition, 0, len(tuples))
	for _, tuple := range tuples {
		tuplesWithoutConditions = append(
			tuplesWithoutConditions,
			ClientTupleKeyWithoutCondition{User: tuple.User, Relation: tuple.Relation, Object: tuple.Object},
		)
	}
	return s.DeleteTuples(ctx, tuplesWithoutConditions)
}

// ── Coordinator methods ───────────────────────────────────────────────────────
//
// These methods orchestrate both TupleStore and CacheLayer to implement
// higher-level behaviors.

// getRelationsMap converts the desired relations slice into a key→tuple map,
// filling in any empty Object fields and skipping relations for other objects.
func (s FgaService) getRelationsMap(object string, relations []ClientTupleKey) (map[string]ClientTupleKey, error) {
	relationsMap := make(map[string]ClientTupleKey)
	for _, relation := range relations {
		switch {
		case relation.Object == "":
			relation.Object = object
		case relation.Object != object:
			// Not expected to happen, but ensure this function only syncs
			// relationships for a single object at a time.
			continue
		}
		// OpenFGA uses a composite key for tuples of the form
		// "project:acme#writer@user:alice", so our "relation@user" map key
		// should be similarly safe (no need for content escaping).
		key := relation.Relation + "@" + relation.User
		relationsMap[key] = relation
	}
	return relationsMap, nil
}

// SyncObjectTuples synchronizes the OpenFGA tuples for an object to match the
// desired relations. It reads current tuples, diffs them against the desired
// state, writes new ones, deletes stale ones, invalidates the cache, then
// seeds positive cache entries for the newly written user-relation tuples.
func (s FgaService) SyncObjectTuples(
	ctx context.Context,
	object string,
	relations []ClientTupleKey,
	excludeRelations ...string,
) (
	writes []ClientTupleKey,
	deletes []ClientTupleKeyWithoutCondition,
	err error,
) {
	relationsMap, err := s.getRelationsMap(object, relations)
	if err != nil {
		return nil, nil, err
	}

	// Create a map of relations to exclude from deletion
	excludeMap := make(map[string]bool)
	for _, rel := range excludeRelations {
		excludeMap[rel] = true
	}

	tuples, err := s.store.ReadObjectTuples(ctx, object)
	if err != nil {
		return nil, nil, err
	}

	// Iterate over the effective OpenFGA tuples and compare them against the
	// desired state of relationships passed as a function argument. Any matches
	// seen are removed from "map" version of the desired relationships. Any live
	// tuples not requested are added to the "deletes" list for the batch-write
	// request, assuming they are not in the excluded relations list.
	// Any tuples for "user:<principal>" are added to a NATS message for
	// a subsequent notify-after-invalidation.
	for _, tuple := range tuples {
		// See comment on our map key format earlier in this function.
		key := tuple.Key.Relation + "@" + tuple.Key.User
		_, match := relationsMap[key]
		switch match {
		case true:
			// Desired state matches current state. Remove the match from "desired
			// state" since we won't need to write/insert it.
			delete(relationsMap, key)
			if isUser := strings.HasPrefix(tuple.Key.User, "user:") && tuple.Key.User != constants.UserWildcard; isUser {
				// Save this for a later user-access notification.
				msg := fmt.Sprintf("%s#%s@%s\ttrue\n", tuple.Key.Object, tuple.Key.Relation, tuple.Key.User)
				logger.With("message", msg).DebugContext(ctx, "will send user access notification")
			}
		case false:
			// Check if this relation should be excluded from deletion
			if excludeMap[tuple.Key.Relation] {
				logger.With(
					"user", tuple.Key.User,
					"relation", tuple.Key.Relation,
					"object", object,
				).DebugContext(ctx, "skipping deletion of excluded relation")
				continue
			}
			// Preserve externally managed team grants. Relations prefixed with
			// global_ are publisher-managed and must remain removable by sync.
			if strings.HasPrefix(tuple.Key.User, constants.ObjectTypeTeam) &&
				!strings.HasPrefix(tuple.Key.Relation, "global_") {
				logger.With(
					"user", tuple.Key.User,
					"relation", tuple.Key.Relation,
					"object", object,
				).DebugContext(ctx, "skipping deletion of team member grant tuple")
				continue
			}
			logger.With(
				"user", tuple.Key.User,
				"relation", tuple.Key.Relation,
				"object", object,
			).DebugContext(ctx, "will delete relation in batch write")
			deletes = append(deletes, ClientTupleKeyWithoutCondition{
				User: tuple.Key.User, Relation: tuple.Key.Relation, Object: object,
			})
		}
	}

	// Any remaining relationships in the "map" version of the desired state are
	// new (not found in live OpenFGA) and therefore will be added to the "write"
	// list for the batch-write request. cacheKeysByTuple is keyed by the same
	// tuple-string format OpenFGA reports for a rejected write, so a tuple
	// skipped during invalid-tuple recovery can be excluded from seeding below.
	cacheKeysByTuple := make(map[string]string, len(relationsMap))
	for _, relation := range relationsMap {
		logger.With(
			"user", relation.User,
			"relation", relation.Relation,
			"object", object,
		).DebugContext(ctx, "will add relation in batch write")
		writes = append(writes, relation)
		if isUser := strings.HasPrefix(relation.User, "user:"); isUser {
			// Seed any (direct) user relationships to the cache after this
			// function returns (after the invalidation cache write, if there is
			// one). Only user relationships are written, because we don't support
			// explicit querying of resource-parent relationships (or similar)
			// which don't resolve back to a user.
			relationKey := relation.Object + "#" + relation.Relation + "@" + relation.User
			cacheKeysByTuple[relationKey] = "rel." + cacheKeyEncoder.EncodeToString([]byte(relationKey))
		}
	}

	// Escape early if there is nothing to write or delete.
	if len(writes) == 0 && len(deletes) == 0 {
		return writes, deletes, nil
	}

	// Use the shared write-and-cache-invalidate delegate.
	skippedWrites, err := s.WriteAndDeleteTuples(ctx, writes, deletes)
	if err != nil {
		return writes, deletes, err
	}

	// A tuple OpenFGA rejected as invalid and skipped was never stored, even
	// though the overall batch succeeded; do not seed a false-positive cache
	// entry for it.
	for _, tupleStr := range skippedWrites {
		delete(cacheKeysByTuple, tupleStr)
	}

	cacheKeys := make([]string, 0, len(cacheKeysByTuple))
	for _, cacheKey := range cacheKeysByTuple {
		cacheKeys = append(cacheKeys, cacheKey)
	}
	s.cache.seedPositiveEntries(ctx, cacheKeys)
	return writes, deletes, nil
}

// CheckRelationships uses OpenFGA to determine multiple relationships in
// bulk for any relationships not found in the cache.
func (s FgaService) CheckRelationships(ctx context.Context, tuples []ClientCheckRequest) ([]byte, error) {
	if len(tuples) == 0 {
		return nil, nil
	}

	// Preallocate our response slice based on an expected relation size of 80
	// bytes each.
	message := make([]byte, 0, 80*len(tuples))

	tuplesToCheck := make([]ClientBatchCheckItem, 0) // list of tuples to check in OpenFGA if not in cache
	tupleItems := make([]ClientBatchCheckItem, 0, len(tuples))
	for _, tuple := range tuples {
		tupleItems = append(tupleItems, ClientBatchCheckItem{
			User:     tuple.User,
			Relation: tuple.Relation,
			Object:   tuple.Object,
		})
	}

	if !s.useCache {
		// Cache disabled; all tuples go straight to OpenFGA.
		tuplesToCheck = append(tuplesToCheck, tupleItems...)
	} else {
		// Resolve every tuple against the cache concurrently rather than one
		// sequential NATS KV round-trip at a time — a batch of several hundred
		// tuples serially was the dominant cost in slow access checks. Each
		// lookup writes to its own index, so no synchronization is needed
		// beyond the errgroup itself. The pass below preserves input order
		// only for cache hits; lines for tuples that fall through to OpenFGA
		// are appended by buildResponseAndWriteBack in map-iteration order,
		// so overall response order is not guaranteed and callers must not
		// rely on it.
		lookupCtx, lookupSpan := tracer.Start(ctx, "fga_sync.cache.lookup",
			trace.WithAttributes(attribute.Int("fga_sync.cache.lookup.requested", len(tupleItems))),
		)

		invLookup := newInvalidationLookup(s.cache)
		outcomes := make([]cacheLookupOutcome, len(tupleItems))
		g, gctx := errgroup.WithContext(lookupCtx)
		g.SetLimit(cacheLookupConcurrency)
		for i, tuple := range tupleItems {
			g.Go(func() error {
				outcomes[i] = s.cache.lookupEntry(gctx, tuple, invLookup)
				return nil
			})
		}
		// lookupEntry never returns an error itself, so this only ever
		// reflects context cancellation, which none of the goroutines trigger.
		if waitErr := g.Wait(); waitErr != nil {
			logger.With(errKey, waitErr).ErrorContext(ctx, "cache lookup fan-out returned an error")
		}

		var hits, staleHits, misses int
		for i, outcome := range outcomes {
			switch outcome.kind {
			case cacheOutcomeHit:
				hits++
			case cacheOutcomeStaleHit:
				staleHits++
			default:
				misses++
			}
			if outcome.needsCheck {
				tuplesToCheck = append(tuplesToCheck, tupleItems[i])
				continue
			}
			message = append(message, outcome.hitLine...)
		}
		lookupSpan.SetAttributes(
			attribute.Int("fga_sync.cache.lookup.hits", hits),
			attribute.Int("fga_sync.cache.lookup.stale_hits", staleHits),
			attribute.Int("fga_sync.cache.lookup.misses", misses),
		)
		lookupSpan.End()
	}

	// If we have no tuples to check, return the cached message.
	if len(tuplesToCheck) == 0 {
		if len(message) < 1 {
			// This shouldn't happen (tuples was non-empty, so tuplesToCheck should
			// only be empty if we appended cache-hits to message), but it's a
			// sanity test before applying the len(message)-1 slice range.
			return nil, errors.New("batch check cached-built message empty")
		}
		// Trim the last newline and return.
		return message[:len(message)-1], nil
	}

	// Add correlation IDs to the tuples to check.
	// Increment each correlation ID by 1, starting from 1.
	mapCorrelationIDToTuple := make(map[string]ClientBatchCheckItem)
	for idx := range tuplesToCheck {
		correlationID := fmt.Sprintf("%d", idx+1)
		tuplesToCheck[idx].CorrelationId = correlationID
		mapCorrelationIDToTuple[correlationID] = tuplesToCheck[idx]
	}

	// Check all tuples that weren't found in the cache.
	batchCheckRequest := ClientBatchCheckRequest{
		Checks: tuplesToCheck,
	}
	batchResp, err := s.store.batchCheck(ctx, batchCheckRequest)
	if err != nil {
		return nil, err
	}

	if batchResp == nil || batchResp.Result == nil || len(*batchResp.Result) == 0 {
		return nil, errors.New("batch check response was nil or empty")
	}

	// Assemble result lines and fan out cache write-backs.
	message = s.cache.buildResponseAndWriteBack(ctx, message, *batchResp.Result, mapCorrelationIDToTuple)

	if len(message) < 1 {
		// This shouldn't happen (*batchResp was checked for ==0 above with an
		// early return, so there must have been at least one loop iteration), but
		// it's a sanity test before applying the len(message)-1 slice range.
		return nil, errors.New("batch check response message empty")
	}

	// Trim the last newline and return.
	return message[:len(message)-1], nil
}

// ExtractCheckRequests extracts the check requests from our binary message
// payload format, which is a newline-delineated list of the format
// `object#relation@user`.
func (s FgaService) ExtractCheckRequests(payload []byte) ([]ClientCheckRequest, error) {
	checkRequests := make([]ClientCheckRequest, 0)

	lines := bytes.Split(payload, []byte("\n"))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}

		checkRequest, err := s.parseCheckRequest(line)
		if err != nil {
			return nil, err
		}

		logger.With(
			"object", checkRequest.Object,
			"relation", checkRequest.Relation,
			"user", checkRequest.User,
		).Debug("parsed check request")

		checkRequests = append(checkRequests, *checkRequest)
	}

	return checkRequests, nil
}

// parseCheckRequest parses a single check request from the format
// `object#relation@user`.
func (s FgaService) parseCheckRequest(line []byte) (*ClientCheckRequest, error) {
	// Split the user from the object and relation.
	var firstPart, userPart []byte
	var found bool
	if firstPart, userPart, found = bytes.Cut(line, []byte("@")); !found {
		return nil, fmt.Errorf("invalid check request: %s", line)
	}

	// Split the object and relation.
	var objectPart, relationPart []byte
	if objectPart, relationPart, found = bytes.Cut(firstPart, []byte("#")); !found {
		return nil, fmt.Errorf("invalid check request: %s", line)
	}

	// Create the check request.
	checkRequest := &ClientCheckRequest{
		User:     string(userPart),
		Relation: string(relationPart),
		Object:   string(objectPart),
	}

	return checkRequest, nil
}

// ── Span helpers ──────────────────────────────────────────────────────────────

// fgaStatusCoder is implemented by all OpenFGA SDK API error types. It exposes
// the HTTP response status code so callers can distinguish client (4xx) from
// server (5xx) failures without importing concrete SDK error types.
type fgaStatusCoder interface {
	ResponseStatusCode() int
}

// fgaIs4xx returns true when err is an OpenFGA SDK API error with a 4xx HTTP
// status code. These represent expected client-side conditions (bad request,
// auth, not found) and must not be recorded as span errors.
func fgaIs4xx(err error) bool {
	var sc fgaStatusCoder
	return errors.As(err, &sc) && sc.ResponseStatusCode() >= 400 && sc.ResponseStatusCode() < 500
}

// recordSpanError records err on the active span and marks it errored,
// unless err represents an OpenFGA 4xx (expected client-side) condition.
func recordSpanError(ctx context.Context, err error) {
	if fgaIs4xx(err) {
		return
	}
	span := trace.SpanFromContext(ctx)
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}
