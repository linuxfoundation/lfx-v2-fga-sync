// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// The fga-sync service.
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
	"time"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
	"github.com/nats-io/nats.go/jetstream"
	openfga "github.com/openfga/go-sdk"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	. "github.com/openfga/go-sdk/client"
)

// Note: all OpenFGA SDK calls are kept in the same file due to the namespace
// pollution which is the recommended way of using this SDK.

var (
	cacheHits       *expvar.Int
	cacheStaleHits  *expvar.Int
	cacheMisses     *expvar.Int
	cacheKeyEncoder = base32.StdEncoding.WithPadding(base32.NoPadding)
)

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
			Transport: otelhttp.NewTransport(http.DefaultTransport),
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
	// list for the batch-write request.
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
			cacheKey := "rel." + cacheKeyEncoder.EncodeToString([]byte(relationKey))
			// Execute cache update asynchronously without defer to avoid resource leak
			go func(cacheKey string) {
				// Define a timeout context for the cache update operation.
				timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel() // Ensure the context is cleaned up after the operation.

				// All direct relations handled in this function correspond to "true"
				// access relations. This happens asynchronously so we are not checking
				// for errors or logging anything.
				//nolint:errcheck // This happens asynchronously so we are not checking for errors.
				_, _ = s.cacheBucket.PutString(timeoutCtx, cacheKey, "true")
			}(cacheKey)
		}
	}

	// Escape early if there is nothing to write or delete.
	if len(writes) == 0 && len(deletes) == 0 {
		return writes, deletes, nil
	}

	// Use the shared write and delete function
	err = s.WriteAndDeleteTuples(ctx, writes, deletes)
	if err != nil {
		return writes, deletes, err
	}

	return writes, deletes, nil
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
// so this function will automatically batch operations if needed.
func (s FgaService) WriteAndDeleteTuples(
	ctx context.Context,
	writes []ClientTupleKey,
	deletes []ClientTupleKeyWithoutCondition,
) error {
	// Return early if there's nothing to do
	if len(writes) == 0 && len(deletes) == 0 {
		return nil
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

		if err := s.writeAndDeleteTuplesBatch(ctx, batchWrites, batchDeletes); err != nil {
			logger.With(errKey, err,
				"batch_number", batchNumber,
				"total_operations", totalOperations,
				"batch_writes", len(batchWrites),
				"batch_deletes", len(batchDeletes),
			).ErrorContext(ctx, "failed to execute batch")
			return err
		}
	}

	logger.With(
		"total_batches", batchNumber,
		"total_writes", len(writes),
		"total_deletes", len(deletes),
	).InfoContext(ctx, "completed batched write operations")

	return nil
}

// writeAndDeleteTuplesBatch performs a single write/delete operation to OpenFGA.
// This is an internal helper function that should not be called directly.
func (s FgaService) writeAndDeleteTuplesBatch(
	ctx context.Context,
	writes []ClientTupleKey,
	deletes []ClientTupleKeyWithoutCondition,
) error {
	req := ClientWriteRequest{
		Writes:  writes,
		Deletes: deletes,
	}

	_, err := s.client.Write(ctx, req)
	if err != nil {
		return err
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

	return nil
}

// WriteTuples writes the given tuples to OpenFGA without reading or comparing existing tuples.
// This is useful for adding specific relations without affecting other relations on the object.
func (s FgaService) WriteTuples(ctx context.Context, tuples []ClientTupleKey) error {
	return s.WriteAndDeleteTuples(ctx, tuples, nil)
}

// DeleteTuples deletes the given tuples from OpenFGA without reading or comparing existing tuples.
// This is useful for removing specific relations without affecting other relations on the object.
func (s FgaService) DeleteTuples(ctx context.Context, tuples []ClientTupleKeyWithoutCondition) error {
	return s.WriteAndDeleteTuples(ctx, nil, tuples)
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

// GetTuplesByUser returns all tuples for a specific user.
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
	message []byte,
	result map[string]openfga.BatchCheckSingleResult,
	mapCorrelationIDToTuple map[string]ClientBatchCheckItem,
	ctx context.Context,
) []byte {
	for correlationID, resp := range result {
		// This is the specific request tuple that the response corresponds to.
		req, ok := mapCorrelationIDToTuple[correlationID]
		if !ok {
			continue
		}
		relationKey := req.Object + "#" + req.Relation + "@" + req.User
		allowed := strconv.FormatBool(resp.GetAllowed())

		// Append the result to our response message.
		message = append(message, []byte(relationKey+"\t"+allowed+"\n")...)

		// Cache the result.
		cacheKey := "rel." + cacheKeyEncoder.EncodeToString([]byte(relationKey))
		_, err := s.cacheBucket.Put(ctx, cacheKey, []byte(allowed))
		if err != nil {
			logger.With(errKey, err).ErrorContext(ctx, "failed to cache relation")
		}
	}

	return message
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

	// Loop through the requested tuples to check for cache hits.
	for i, tuple := range tupleItems {
		// If the cache is disabled, all tuples are added to the check list.
		if !useCache {
			tuplesToCheck = append(tuplesToCheck, tuple)
			continue
		}

		relationKey := tuple.Object + "#" + tuple.Relation + "@" + tuple.User
		// Encode relation using base32 without padding to conform to the allowed
		// characters for NATS subjects.
		cacheKey := "rel." + cacheKeyEncoder.EncodeToString([]byte(relationKey))
		entry, errCache := s.cacheBucket.Get(ctx, cacheKey)
		if errCache == jetstream.ErrKeyNotFound {
			// No cache hit; continue.
			cacheMisses.Add(1)
			tuplesToCheck = append(tuplesToCheck, tuple)
			continue
		}
		if errCache != nil {
			// This is not expected (we would have exited early already on cache
			// errors when grabbing the invalidation timestamp), but log the error
			// and skip cache lookups for remaining items without breaking the
			// request at this point.
			logger.With(errKey, errCache).ErrorContext(ctx, "cache error; continuing")
			// Add all remaining tuples to the check list.
			tuplesToCheck = append(tuplesToCheck, tupleItems[i:]...)
			break
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
			tuplesToCheck = append(tuplesToCheck, tupleItems[i])
			continue
		}
		logger.With(
			"relation_key", relationKey,
			"last_invalidation", lastInvalidation,
			"entry_created", entry.Created(),
			"entry_value", string(entry.Value()),
		).DebugContext(ctx, "cache hit")
		cacheHits.Add(1)
		// Append the cached value to our response message.
		message = append(message, []byte(fmt.Sprintf("%s\t%s\n", relationKey, string(entry.Value())))...)
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
		return nil, err
	}

	if batchResp == nil || batchResp.Result == nil || len(*batchResp.Result) == 0 {
		return nil, errors.New("batch check response was nil or empty")
	}

	// Loop through the responses.
	message = s.appendToMessage(message, *batchResp.Result, mapCorrelationIDToTuple, ctx)

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
