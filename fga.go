// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package main provides the fga-sync service entry point and supporting types.
package main

import (
	"bytes"
	"context"
	"encoding/base32"
	"errors"
	"expvar"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
	"github.com/nats-io/nats.go/jetstream"
	openfga "github.com/openfga/go-sdk"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"

	. "github.com/openfga/go-sdk/client"
)

// Note: all OpenFGA SDK calls are kept in the same file due to the namespace
// pollution which is the recommended way of using this SDK.

const (
	// trueString is used for cache values representing allowed access
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

	// cacheLookupConcurrency bounds how many NATS KV Get/Put calls run in
	// parallel when resolving a batch of tuples against the cache. A batch of
	// several hundred tuples run serially (one JetStream round-trip each, ~20ms
	// apiece) was directly responsible for double-digit-second access-check
	// latency; this trades a bounded amount of extra NATS load for wall-clock.
	cacheLookupConcurrency = 64

	// cacheOpConcurrency caps total in-flight JetStream KV operations for this
	// process. cacheLookupConcurrency bounds one request's fan-out, but the
	// subscription layer admits subscriptionConcurrency (64) handlers at once,
	// so per-request limits alone permit ~4,096 simultaneous KV round-trips per
	// pod. The cache bucket is single-replica (see the chart's
	// nats-kv-bucket.yaml, which sets no replicas field), so every pod's cache
	// traffic funnels into one JetStream node; cluster-wide pressure is this
	// value times application.replicas (3 in prod). Sized at 2x
	// cacheLookupConcurrency so a couple of large batches still overlap fully
	// while that product stays reasonable for a single-node bucket.
	cacheOpConcurrency = 2 * cacheLookupConcurrency
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

var (
	cacheHits       *expvar.Int
	cacheStaleHits  *expvar.Int
	cacheMisses     *expvar.Int
	cacheKeyEncoder = base32.StdEncoding.WithPadding(base32.NoPadding)

	// cacheOpSem is the service-wide budget for JetStream KV operations. Every
	// concurrent KV Get/Put in this file acquires a slot before the
	// round-trip and releases it after. Held only around the KV call itself,
	// never across an OpenFGA call, so a slow BatchCheck cannot hold cache
	// capacity hostage.
	cacheOpSem = make(chan struct{}, cacheOpConcurrency)
)

// withCacheOpSlot runs fn while holding a slot in the service-wide KV budget
// (cacheOpSem). If ctx is canceled before a slot frees, fn is not run and
// ctx.Err() is returned; callers treat that the same as any other cache
// failure (fall through to OpenFGA, or skip a best-effort write).
func withCacheOpSlot(ctx context.Context, fn func()) error {
	select {
	case cacheOpSem <- struct{}{}:
		defer func() { <-cacheOpSem }()
		fn()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func init() {
	cacheHits = expvar.NewInt("cache_hits")
	cacheStaleHits = expvar.NewInt("cache_stale_hits")
	cacheMisses = expvar.NewInt("cache_misses")
}

// INatsKeyValue is a NATS KV interface needed for the [ProjectsService].
type INatsKeyValue interface {
	Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error)
	Put(context.Context, string, []byte) (uint64, error)
	PutString(context.Context, string, string) (uint64, error)
}

// FgaService is a service for OpenFGA client operations used in this service.
type FgaService struct {
	client      IFgaClient
	cacheBucket INatsKeyValue
	useCache    bool
}

// connectFga initializes the global shared fgaClient connection. This demo
// does not use or support authentication.
func connectFga() (IFgaClient, error) {
	var err error
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

// NewTupleKeySlice abstracts the creation of a ClientTupleKey slice for our
// handler functions.
func (s FgaService) NewTupleKeySlice(size int) []ClientTupleKey {
	// Preallocate our slice to avoid extra allocations.
	slice := make([]ClientTupleKey, 0, size)
	return slice
}

// TupleKey abstracts the creation of a ClientTupleKey for our handler functions.
func (s FgaService) TupleKey(user, relation, object string) ClientTupleKey {
	return ClientTupleKey{
		User:     user,
		Relation: relation,
		Object:   object,
	}
}

// TupleKeyWithoutCondition abstracts the creation of a ClientTupleKeyWithoutCondition for our handler functions.
func (s FgaService) TupleKeyWithoutCondition(user, relation, object string) ClientTupleKeyWithoutCondition {
	return ClientTupleKeyWithoutCondition{
		User:     user,
		Relation: relation,
		Object:   object,
	}
}

// ReadObjectTuples is a pagination helper to fetch all direct relationships (_no_
// transitive evaluations) defined against a given object.
func (s FgaService) ReadObjectTuples(ctx context.Context, object string) ([]openfga.Tuple, error) {
	req := ClientReadRequest{
		Object: openfga.PtrString(object),
	}
	options := ClientReadOptions{}
	var tuples []openfga.Tuple
	for {
		resp, err := s.client.Read(ctx, req, options)
		if err != nil {
			recordSpanError(ctx, err)
			return nil, err
		}
		tuples = append(tuples, resp.Tuples...)
		if resp.ContinuationToken == "" {
			break
		}
		options.ContinuationToken = openfga.PtrString(resp.ContinuationToken)
	}

	return tuples, nil
}

// ReadUserTuples fetches all direct relationships for a given user across all
// objects of the specified type. It paginates internally via ContinuationToken.
func (s FgaService) ReadUserTuples(ctx context.Context, user, objectType string) ([]openfga.Tuple, error) {
	objectTypeColon := objectType + ":"
	req := ClientReadRequest{
		User:   openfga.PtrString(user),
		Object: openfga.PtrString(objectTypeColon),
	}
	options := ClientReadOptions{}
	var tuples []openfga.Tuple
	for {
		resp, err := s.client.Read(ctx, req, options)
		if err != nil {
			recordSpanError(ctx, err)
			return nil, err
		}
		tuples = append(tuples, resp.Tuples...)
		if resp.ContinuationToken == "" {
			break
		}
		options.ContinuationToken = openfga.PtrString(resp.ContinuationToken)
	}

	return tuples, nil
}

// ListObjectsByUserAndRelation uses the List Objects API to find all objects of a specific type
// that have a given relation to a user. This is useful for finding all artifacts that relate to a past meeting.
func (s FgaService) ListObjectsByUserAndRelation(
	ctx context.Context,
	objectType, relation, user string,
) ([]string, error) {
	body := ClientListObjectsRequest{
		User:     user,
		Relation: relation,
		Type:     objectType,
	}

	options := ClientListObjectsOptions{}

	resp, err := s.client.ListObjects(ctx, body, options)
	if err != nil {
		recordSpanError(ctx, err)
		return nil, err
	}

	return resp.Objects, nil
}

func (s FgaService) getRelationsMap(object string, relations []ClientTupleKey) (map[string]ClientTupleKey, error) {
	// Convert the passed relationships into a map.
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
		// "project:acme#writer@user:alice", so our "relation@user" map key should
		// be similarly safe (no need for content escaping).
		key := relation.Relation + "@" + relation.User
		relationsMap[key] = relation
	}

	return relationsMap, nil
}

// SyncObjectTuples synchronizes the OpenFGA tuples for an object to match the desired relations.
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

	tuples, err := s.ReadObjectTuples(ctx, object)
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
			// Preserve team member grant tuples (e.g. team:my-team#member) — these are
			// managed by a separate workflow and must not be clobbered by resource
			// service sync operations.
			if strings.HasPrefix(tuple.Key.User, "team:") {
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
			deletes = append(deletes, s.TupleKeyWithoutCondition(tuple.Key.User, tuple.Key.Relation, object))
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
			// Seed any (direct) user relationships to the cache after this function
			// returns (after the invalidation cache write, if there is one). Only
			// user relationships are written, because we don't support explicit
			// querying of resource-parent relationships (or similar) which don't
			// resolve back to a user. TBD figure out a way to measure the impact
			// this has on overall cache effectiveness, especially once we start
			// updating large-scale relationships, like groups with over a thousand
			// members.
			relationKey := relation.Object + "#" + relation.Relation + "@" + relation.User
			cacheKeysByTuple[relationKey] = "rel." + cacheKeyEncoder.EncodeToString([]byte(relationKey))
		}
	}

	// Escape early if there is nothing to write or delete.
	if len(writes) == 0 && len(deletes) == 0 {
		return writes, deletes, nil
	}

	// Use the shared write and delete function
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
	s.seedPositiveCacheEntries(ctx, cacheKeys)
	return writes, deletes, nil
}

// seedPositiveCacheEntries writes each cacheKey and blocks until all writes
// complete (or time out), rather than firing detached goroutines. The access
// mutation consumer processes exactly one message at a time
// (MaxAckPending: 1) and only ACKs after this call returns, so awaiting the
// seed here guarantees it lands before any later message's invalidateCache
// call. A detached seed could otherwise still be in flight when a later
// message (e.g. a delete for the same relation) invalidates the cache first;
// if the stale seed then landed after that invalidation's timestamp, the
// staleness check in CheckRelationships (entry created after last
// invalidation) would treat it as fresh and resurrect access the later
// message had just revoked.
//
// Every cacheKey passed in corresponds to a direct user relationship that
// SyncObjectTuples just wrote, so writing trueString is always correct here:
// all such relations are "true" (allowed) access relations by construction.
//
// Each write additionally waits on cacheOpSem, the service-wide JetStream KV
// budget shared with CheckRelationships. That only ever shortens how many
// writes run at once; it does not affect the wg.Wait() ordering guarantee
// above, since every goroutine below is still awaited regardless of whether
// it ran immediately or queued for a slot.
func (s FgaService) seedPositiveCacheEntries(ctx context.Context, cacheKeys []string) {
	var wg sync.WaitGroup
	for _, cacheKey := range cacheKeys {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()

			//nolint:errcheck // Cache seeding is best-effort after a successful OpenFGA write.
			_ = withCacheOpSlot(timeoutCtx, func() {
				_, _ = s.cacheBucket.PutString(timeoutCtx, key, trueString)
			})
		}(cacheKey)
	}
	wg.Wait()
}

// invalidateCache invalidates the cache by writing a timestamp marker.
// Any value will work, since it is the native timestamp of the record that is checked, not its value.
func (s FgaService) invalidateCache(ctx context.Context) error {
	_, err := s.cacheBucket.Put(ctx, "inv", []byte("1"))
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "failed to write cache invalidation marker")
		return err
	}
	return nil
}

// WriteAndDeleteTuples writes and/or deletes the given tuples to/from OpenFGA.
// This is a general-purpose method for modifying tuples without reading existing state.
// OpenFGA has a limit of 100 total operations (writes + deletes combined) per request,
// so this function will automatically batch operations if needed. It returns
// the tuple strings of any write tuples OpenFGA rejected as invalid and
// skipped rather than storing, so callers that pre-computed cache keys from
// the original write list can exclude those tuples before seeding.
func (s FgaService) WriteAndDeleteTuples(
	ctx context.Context,
	writes []ClientTupleKey,
	deletes []ClientTupleKeyWithoutCondition,
) ([]string, error) {
	// Return early if there's nothing to do
	if len(writes) == 0 && len(deletes) == 0 {
		return nil, nil
	}

	// This max operations limit is set by the OpenFGA Write API
	const maxOperationsPerBatch = 100
	totalOperations := len(writes) + len(deletes)

	// If total operations fit in a single batch, process normally
	if totalOperations <= maxOperationsPerBatch {
		return s.writeAndDeleteTuplesBatch(ctx, writes, deletes)
	}

	// Need to batch the operations
	logger.With(
		"total_operations", totalOperations,
		"writes_count", len(writes),
		"deletes_count", len(deletes),
	).InfoContext(ctx, "batching write operations due to size")

	// Process writes and deletes in batches
	writeIdx := 0
	deleteIdx := 0
	batchNumber := 0
	var skippedWrites []string

	for writeIdx < len(writes) || deleteIdx < len(deletes) {
		batchNumber++
		var batchWrites []ClientTupleKey
		var batchDeletes []ClientTupleKeyWithoutCondition

		// Fill the batch with writes first, then deletes, up to maxOperationsPerBatch
		remainingCapacity := maxOperationsPerBatch

		// Add writes to this batch
		if writeIdx < len(writes) && remainingCapacity > 0 {
			writeEnd := writeIdx + remainingCapacity
			if writeEnd > len(writes) {
				writeEnd = len(writes)
			}
			batchWrites = writes[writeIdx:writeEnd]
			writeIdx = writeEnd
			remainingCapacity -= len(batchWrites)
		}

		// Add deletes to this batch
		if deleteIdx < len(deletes) && remainingCapacity > 0 {
			deleteEnd := deleteIdx + remainingCapacity
			if deleteEnd > len(deletes) {
				deleteEnd = len(deletes)
			}
			batchDeletes = deletes[deleteIdx:deleteEnd]
			deleteIdx = deleteEnd
		}

		// Execute this batch
		logger.With(
			"batch_number", batchNumber,
			"batch_writes", len(batchWrites),
			"batch_deletes", len(batchDeletes),
		).DebugContext(ctx, "executing batch")

		batchSkipped, err := s.writeAndDeleteTuplesBatch(ctx, batchWrites, batchDeletes)
		skippedWrites = append(skippedWrites, batchSkipped...)
		if err != nil {
			logger.With("error_type", safeErrorType(err),
				"batch_number", batchNumber,
				"total_operations", totalOperations,
				"batch_writes", len(batchWrites),
				"batch_deletes", len(batchDeletes),
			).ErrorContext(ctx, "failed to execute batch")
			return skippedWrites, err
		}
	}

	logger.With(
		"total_batches", batchNumber,
		"total_writes", len(writes),
		"total_deletes", len(deletes),
	).InfoContext(ctx, "completed batched write operations")

	return skippedWrites, nil
}

// writeCollisionIgnoreOptions instructs OpenFGA to treat a write of an
// already-existing tuple, or a delete of an already-absent tuple, as a
// server-side no-op instead of a failed transaction. Both fields must be set
// together: a request mixing ignore and error semantics reverts to error for
// the whole request, so setting only one has no effect on a batch carrying
// both writes and deletes. This applies to every writeAndDeleteTuplesBatch
// call, including the two Phase 1 access subjects, because the same
// collision can occur on a retry after a partially applied batch.
var writeCollisionIgnoreOptions = ClientWriteOptions{
	Conflict: ClientWriteConflictOptions{
		OnDuplicateWrites: CLIENT_WRITE_REQUEST_ON_DUPLICATE_WRITES_IGNORE,
		OnMissingDeletes:  CLIENT_WRITE_REQUEST_ON_MISSING_DELETES_IGNORE,
	},
}

// writeAndDeleteTuplesBatch performs a single write/delete operation to OpenFGA.
// If OpenFGA returns a validation_error for an invalid tuple, that tuple is
// removed and the batch is retried with the remaining tuples. It returns the
// tuple strings (e.g. "object:id#relation@user:id") of any write tuples
// skipped this way, so callers that pre-computed cache keys from the original
// write list can exclude tuples OpenFGA never actually stored.
// This is an internal helper function that should not be called directly.
func (s FgaService) writeAndDeleteTuplesBatch(
	ctx context.Context,
	writes []ClientTupleKey,
	deletes []ClientTupleKeyWithoutCondition,
) ([]string, error) {
	var skippedWrites []string
	for {
		req := ClientWriteRequest{
			Writes:  writes,
			Deletes: deletes,
		}

		_, err := s.client.Write(ctx, req, writeCollisionIgnoreOptions)
		if err != nil {
			tupleStr, ok := extractInvalidTuple(err)
			if !ok {
				recordSpanError(ctx, err)
				return skippedWrites, err
			}

			removedWrite := false
			writes, removedWrite = removeInvalidWriteTuple(writes, tupleStr)
			removed := removedWrite
			if !removed {
				deletes, removed = removeInvalidDeleteTuple(deletes, tupleStr)
			}
			if !removed {
				recordSpanError(ctx, err)
				return skippedWrites, err
			}
			if removedWrite {
				skippedWrites = append(skippedWrites, tupleStr)
			}

			logger.With(
				"skipped_tuple", tupleStr,
				"remaining_writes", len(writes),
				"remaining_deletes", len(deletes),
			).WarnContext(ctx, "skipping invalid tuple and retrying batch write")

			if len(writes) == 0 && len(deletes) == 0 {
				return skippedWrites, nil
			}
			continue
		}

		break
	}

	// Invalidate cache after write
	if err := s.invalidateCache(ctx); err != nil {
		// Log but don't fail the operation since the write succeeded
		logger.With(errKey, err).WarnContext(ctx, "cache invalidation failed")
	}

	logger.With(
		"writes_count", len(writes),
		"deletes_count", len(deletes),
		"writes", writes,
		"deletes", deletes,
	).InfoContext(ctx, "wrote and deleted tuples")

	return skippedWrites, nil
}

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

// extractInvalidTuple extracts the tuple string from an OpenFGA validation error.
// Returns the tuple string (e.g. "object:id#relation@user:id") and true if the
// error is a validation_error containing an invalid tuple message.
func extractInvalidTuple(err error) (string, bool) {
	var validationErr openfga.FgaApiValidationError
	if !errors.As(err, &validationErr) {
		return "", false
	}
	const prefix = "Invalid tuple '"
	_, afterPrefix, found := bytes.Cut([]byte(validationErr.Error()), []byte(prefix))
	if !found {
		return "", false
	}
	tuple, _, found := bytes.Cut(afterPrefix, []byte("'"))
	if !found {
		return "", false
	}
	return string(tuple), true
}

// removeInvalidWriteTuple returns a new slice with the first write tuple matching
// tupleStr removed. Returns the original slice and false if no match is found.
func removeInvalidWriteTuple(writes []ClientTupleKey, tupleStr string) ([]ClientTupleKey, bool) {
	for i, t := range writes {
		if t.Object+"#"+t.Relation+"@"+t.User == tupleStr {
			result := make([]ClientTupleKey, 0, len(writes)-1)
			result = append(result, writes[:i]...)
			result = append(result, writes[i+1:]...)
			return result, true
		}
	}
	return writes, false
}

// removeInvalidDeleteTuple returns a new slice with the first delete tuple matching
// tupleStr removed. Returns the original slice and false if no match is found.
func removeInvalidDeleteTuple(
	deletes []ClientTupleKeyWithoutCondition,
	tupleStr string,
) ([]ClientTupleKeyWithoutCondition, bool) {
	for i, t := range deletes {
		if t.Object+"#"+t.Relation+"@"+t.User == tupleStr {
			result := make([]ClientTupleKeyWithoutCondition, 0, len(deletes)-1)
			result = append(result, deletes[:i]...)
			result = append(result, deletes[i+1:]...)
			return result, true
		}
	}
	return deletes, false
}

// WriteTuples writes the given tuples to OpenFGA without reading or comparing existing tuples.
// This is useful for adding specific relations without affecting other relations on the object.
func (s FgaService) WriteTuples(ctx context.Context, tuples []ClientTupleKey) error {
	_, err := s.WriteAndDeleteTuples(ctx, tuples, nil)
	return err
}

// DeleteTuples deletes the given tuples from OpenFGA without reading or comparing existing tuples.
// This is useful for removing specific relations without affecting other relations on the object.
func (s FgaService) DeleteTuples(ctx context.Context, tuples []ClientTupleKeyWithoutCondition) error {
	_, err := s.WriteAndDeleteTuples(ctx, nil, tuples)
	return err
}

// WriteTuple writes a single tuple to OpenFGA using simple string parameters.
// This provides a cleaner API for handlers that don't need to know about OpenFGA types.
func (s FgaService) WriteTuple(ctx context.Context, user, relation, object string) error {
	tuple := s.TupleKey(user, relation, object)
	return s.WriteTuples(ctx, []ClientTupleKey{tuple})
}

// DeleteTuple deletes a single tuple from OpenFGA using simple string parameters.
// This provides a cleaner API for handlers that don't need to know about OpenFGA types.
func (s FgaService) DeleteTuple(ctx context.Context, user, relation, object string) error {
	tuple := s.TupleKeyWithoutCondition(user, relation, object)
	return s.DeleteTuples(ctx, []ClientTupleKeyWithoutCondition{tuple})
}

// DeleteTuplesByUserAndObject deletes all tuples for a specific user and object.
// e.g. delete all tuples associated with user X on meeting Y.
func (s FgaService) DeleteTuplesByUserAndObject(ctx context.Context, user, object string) error {
	tuples, err := s.GetTuplesByUserAndObject(ctx, user, object)
	if err != nil {
		return err
	}
	tuplesWithoutConditions := make([]ClientTupleKeyWithoutCondition, 0, len(tuples))
	for _, tuple := range tuples {
		tuplesWithoutConditions = append(
			tuplesWithoutConditions,
			s.TupleKeyWithoutCondition(tuple.User, tuple.Relation, tuple.Object),
		)
	}
	return s.DeleteTuples(ctx, tuplesWithoutConditions)
}

// GetTuplesByUserAndObject returns all tuples for a specific user on a given object.
func (s FgaService) GetTuplesByUserAndObject(ctx context.Context, user, object string) ([]ClientTupleKey, error) {
	tuples, err := s.ReadObjectTuples(ctx, object)
	if err != nil {
		return nil, err
	}

	// Filter the object tuples to only include the ones for the user.
	var filteredTuples []ClientTupleKey
	for _, tuple := range tuples {
		if tuple.Key.User == user {
			filteredTuples = append(filteredTuples, s.TupleKey(tuple.Key.User, tuple.Key.Relation, object))
		}
	}
	return filteredTuples, nil
}

// GetTuplesByRelation returns tuples for a specific object filtered by relation.
// This provides a generic way to retrieve tuples with a specific relation from an object.
func (s FgaService) GetTuplesByRelation(ctx context.Context, object, relation string) ([]openfga.Tuple, error) {
	allTuples, err := s.ReadObjectTuples(ctx, object)
	if err != nil {
		return nil, err
	}

	var filteredTuples []openfga.Tuple
	for _, tuple := range allTuples {
		if tuple.Key.Relation == relation {
			filteredTuples = append(filteredTuples, tuple)
		}
	}

	return filteredTuples, nil
}

func (s FgaService) getLastCacheInvalidation(ctx context.Context) (time.Time, error) {
	var lastInvalidation time.Time
	entry, err := s.cacheBucket.Get(ctx, "inv")
	switch {
	case err == jetstream.ErrKeyNotFound:
		// No invalidation in the TTL of the cache; all found cache entries are
		// valid. Keep the zero-value of lastInvalidation.
	case err != nil:
		return time.Time{}, err
	default:
		lastInvalidation = entry.Created()
	}

	return lastInvalidation, nil
}

func (s FgaService) appendToMessage(
	ctx context.Context,
	message []byte,
	result map[string]openfga.BatchCheckSingleResult,
	mapCorrelationIDToTuple map[string]ClientBatchCheckItem,
) []byte {
	// Cache write-backs are fanned out below with bounded concurrency once the
	// message is assembled. g.Wait() still blocks below before this function
	// returns, so they complete within the request's lifetime; the fan-out
	// only bounds how many run at once, not whether the reply waits for them.
	cachePuts := make([]func() error, 0, len(result))

	for correlationID, resp := range result {
		// This is the specific request tuple that the response corresponds to.
		req, ok := mapCorrelationIDToTuple[correlationID]
		if !ok {
			continue
		}
		relationKey := req.Object + "#" + req.Relation + "@" + req.User

		// Need a bool to handle whether or not a response should be cached
		// This is needed since it may be an error and not a valid response, but we
		// still need to return not allowed and not cache it
		shouldCache := true
		// Check if the response contains an error (e.g., timeout, deadline exceeded)
		// and skip caching/responding with error results.
		if resp.HasError() {
			checkErr := resp.GetError()
			logger.With(
				"correlation_id", correlationID,
				"relation_key", relationKey,
				"error_code", checkErr.GetInternalError(),
				"error_message", checkErr.GetMessage(),
			).WarnContext(ctx, "batch check returned error for tuple, skipping cache")
			shouldCache = false
		}

		allowed := strconv.FormatBool(resp.GetAllowed())

		// Append the result to our response message.
		message = append(message, []byte(relationKey+"\t"+allowed+"\n")...)

		// Queue the cache write.
		if shouldCache {
			cacheKey := "rel." + cacheKeyEncoder.EncodeToString([]byte(relationKey))
			allowedValue := allowed
			cachePuts = append(cachePuts, func() error {
				putCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				if slotErr := withCacheOpSlot(putCtx, func() {
					if _, err := s.cacheBucket.Put(putCtx, cacheKey, []byte(allowedValue)); err != nil {
						logger.With(errKey, err).ErrorContext(ctx, "failed to cache relation")
					}
				}); slotErr != nil {
					logger.With(errKey, slotErr).ErrorContext(ctx, "failed to cache relation")
				}
				return nil
			})
		}
	}

	if len(cachePuts) > 0 {
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(cacheLookupConcurrency)
		for _, put := range cachePuts {
			g.Go(func() error {
				select {
				case <-gctx.Done():
					return nil
				default:
					return put()
				}
			})
		}
		// Every closure above always returns nil (cache-write failures are
		// already logged per-entry, not propagated), so g.Wait() can only
		// ever return nil here; it is called solely to block until all
		// writes finish.
		//nolint:errcheck // g.Wait() can only return nil; see comment above.
		_ = g.Wait()
	}

	return message
}

// cacheLookupOutcome is the result of resolving a single tuple against the
// cache: either a ready-to-append response line, or a signal that the tuple
// still needs to be resolved via OpenFGA.
type cacheLookupOutcome struct {
	needsCheck bool
	hitLine    []byte
}

// lookupCacheEntry checks the cache for a single tuple. It never returns a Go
// error: a cache miss, a stale hit, or an unexpected cache error all result
// in needsCheck being set so the tuple falls through to OpenFGA instead of
// failing the whole batch.
func (s FgaService) lookupCacheEntry(
	ctx context.Context,
	tuple ClientBatchCheckItem,
	lastInvalidation time.Time,
) cacheLookupOutcome {
	relationKey := tuple.Object + "#" + tuple.Relation + "@" + tuple.User
	// Encode relation using base32 without padding to conform to the allowed
	// characters for NATS subjects.
	cacheKey := "rel." + cacheKeyEncoder.EncodeToString([]byte(relationKey))
	var entry jetstream.KeyValueEntry
	var errCache error
	if slotErr := withCacheOpSlot(ctx, func() {
		entry, errCache = s.cacheBucket.Get(ctx, cacheKey)
	}); slotErr != nil {
		// The service-wide KV budget didn't free up before ctx was canceled;
		// treat this the same as any other cache miss so the tuple falls
		// through to OpenFGA.
		cacheMisses.Add(1)
		return cacheLookupOutcome{needsCheck: true}
	}
	switch {
	case errCache == jetstream.ErrKeyNotFound:
		cacheMisses.Add(1)
		return cacheLookupOutcome{needsCheck: true}
	case errCache != nil:
		// This is not expected (we would have exited early already on cache
		// errors when grabbing the invalidation timestamp), but log and treat
		// this single tuple as a miss rather than failing the whole request.
		logger.With(errKey, errCache).ErrorContext(ctx, "cache error; treating as miss")
		cacheMisses.Add(1)
		return cacheLookupOutcome{needsCheck: true}
	}

	// Cache entry was found. If the cache entry is older than the invalidation
	// timestamp, skip it.
	if lastInvalidation.After(entry.Created()) {
		logger.With(
			"relation_key", relationKey,
			"last_invalidation", lastInvalidation,
			"entry_created", entry.Created(),
			"entry_value", string(entry.Value()),
		).DebugContext(ctx, "cache stale hit")
		cacheStaleHits.Add(1)
		return cacheLookupOutcome{needsCheck: true}
	}

	logger.With(
		"relation_key", relationKey,
		"last_invalidation", lastInvalidation,
		"entry_created", entry.Created(),
		"entry_value", string(entry.Value()),
	).DebugContext(ctx, "cache hit")
	cacheHits.Add(1)
	return cacheLookupOutcome{
		hitLine: []byte(fmt.Sprintf("%s\t%s\n", relationKey, string(entry.Value()))),
	}
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

	// Get the most recent cache invalidation.
	lastInvalidation, err := s.getLastCacheInvalidation(ctx)
	if err != nil {
		return nil, err
	}

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
		// are appended by appendToMessage in map-iteration order, so overall
		// response order is not guaranteed and callers must not rely on it.
		outcomes := make([]cacheLookupOutcome, len(tupleItems))
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(cacheLookupConcurrency)
		for i, tuple := range tupleItems {
			g.Go(func() error {
				outcomes[i] = s.lookupCacheEntry(gctx, tuple, lastInvalidation)
				return nil
			})
		}
		// lookupCacheEntry never returns an error itself, so this only ever
		// reflects context cancellation, which none of the goroutines trigger.
		if waitErr := g.Wait(); waitErr != nil {
			logger.With(errKey, waitErr).ErrorContext(ctx, "cache lookup fan-out returned an error")
		}

		for i, outcome := range outcomes {
			if outcome.needsCheck {
				tuplesToCheck = append(tuplesToCheck, tupleItems[i])
				continue
			}
			message = append(message, outcome.hitLine...)
		}
	}

	// If we have no tuples to check, return the cached message.
	if len(tuplesToCheck) == 0 {
		if len(message) < 1 {
			// This shouldn't happen (tuples was non-empty, so tuplesToCheck should
			// only be empty if we appended cache-hits to message), but it's a sanity
			// test before applying the len(message)-1 slice range.
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
	batchResp, err := s.client.BatchCheck(ctx, batchCheckRequest)
	if err != nil {
		recordSpanError(ctx, err)
		return nil, err
	}

	if batchResp == nil || batchResp.Result == nil || len(*batchResp.Result) == 0 {
		return nil, errors.New("batch check response was nil or empty")
	}

	// Loop through the responses.
	message = s.appendToMessage(ctx, message, *batchResp.Result, mapCorrelationIDToTuple)

	if len(message) < 1 {
		// This shouldn't happen (*batchResp was checked for ==0 above with an
		// early return, so there must have been at least one loop iteration), but
		// it's a sanity test before applying the `len(message)-1` slice range.
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
