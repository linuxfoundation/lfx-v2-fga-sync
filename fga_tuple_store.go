// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package main provides the fga-sync service entry point and supporting types.
package main

import (
	"bytes"
	"context"
	"errors"

	openfga "github.com/openfga/go-sdk"

	. "github.com/openfga/go-sdk/client"
)

// TupleStore wraps IFgaClient and owns all raw tuple I/O: paginated reads,
// batch writes with automatic invalid-tuple recovery, and convenience helpers.
// It has no cache knowledge; cache invalidation after any successful write is
// the caller's responsibility (see FgaService.WriteAndDeleteTuples).
type TupleStore struct {
	client IFgaClient
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

// ReadObjectTuples is a pagination helper to fetch all direct relationships
// (_no_ transitive evaluations) defined against a given object.
func (s TupleStore) ReadObjectTuples(ctx context.Context, object string) ([]openfga.Tuple, error) {
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
func (s TupleStore) ReadUserTuples(ctx context.Context, user, objectType string) ([]openfga.Tuple, error) {
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

// ListObjectsByUserAndRelation uses the List Objects API to find all objects
// of a specific type that have a given relation to a user.
func (s TupleStore) ListObjectsByUserAndRelation(
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

// WriteAndDeleteTuples writes and/or deletes the given tuples to/from OpenFGA.
// OpenFGA has a limit of 100 total operations (writes + deletes combined) per
// request, so this function automatically batches operations if needed. It
// returns the tuple strings of any write tuples OpenFGA rejected as invalid
// and skipped rather than storing, so callers that pre-computed cache keys
// from the original write list can exclude those tuples before seeding.
//
// This method does NOT invalidate the cache; that is the caller's
// responsibility after all batches have been confirmed (see
// FgaService.WriteAndDeleteTuples).
func (s TupleStore) WriteAndDeleteTuples(
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

// writeAndDeleteTuplesBatch performs a single write/delete operation to
// OpenFGA. If OpenFGA returns a validation_error for an invalid tuple, that
// tuple is removed and the batch is retried with the remaining tuples. It
// returns the tuple strings (e.g. "object:id#relation@user:id") of any write
// tuples skipped this way, so callers that pre-computed cache keys from the
// original write list can exclude tuples OpenFGA never actually stored.
//
// This is an internal helper; call WriteAndDeleteTuples for batching.
// It does NOT invalidate the cache; see FgaService.WriteAndDeleteTuples.
func (s TupleStore) writeAndDeleteTuplesBatch(
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

	logger.With(
		"writes_count", len(writes),
		"deletes_count", len(deletes),
		"writes", writes,
		"deletes", deletes,
	).InfoContext(ctx, "wrote and deleted tuples")

	return skippedWrites, nil
}

// WriteTuples writes the given tuples to OpenFGA without reading or comparing
// existing tuples. This is useful for adding specific relations without
// affecting other relations on the object. Does NOT invalidate the cache.
func (s TupleStore) WriteTuples(ctx context.Context, tuples []ClientTupleKey) error {
	_, err := s.WriteAndDeleteTuples(ctx, tuples, nil)
	return err
}

// DeleteTuples deletes the given tuples from OpenFGA without reading or
// comparing existing tuples. Does NOT invalidate the cache.
func (s TupleStore) DeleteTuples(ctx context.Context, tuples []ClientTupleKeyWithoutCondition) error {
	_, err := s.WriteAndDeleteTuples(ctx, nil, tuples)
	return err
}

// GetTuplesByUserAndObject returns all tuples for a specific user on a given object.
func (s TupleStore) GetTuplesByUserAndObject(ctx context.Context, user, object string) ([]ClientTupleKey, error) {
	tuples, err := s.ReadObjectTuples(ctx, object)
	if err != nil {
		return nil, err
	}

	// Filter the object tuples to only include the ones for the user.
	var filteredTuples []ClientTupleKey
	for _, tuple := range tuples {
		if tuple.Key.User == user {
			filteredTuples = append(filteredTuples, ClientTupleKey{
				User: tuple.Key.User, Relation: tuple.Key.Relation, Object: object,
			})
		}
	}
	return filteredTuples, nil
}

// GetTuplesByRelation returns tuples for a specific object filtered by relation.
// This provides a generic way to retrieve tuples with a specific relation.
func (s TupleStore) GetTuplesByRelation(ctx context.Context, object, relation string) ([]openfga.Tuple, error) {
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

// DeleteTuplesByUserAndObject deletes all tuples for a specific user and
// object, reading first to discover which relations exist.
// Does NOT invalidate the cache; that happens via FgaService.DeleteTuples.
func (s TupleStore) DeleteTuplesByUserAndObject(ctx context.Context, user, object string) error {
	tuples, err := s.GetTuplesByUserAndObject(ctx, user, object)
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

// batchCheck calls the FGA BatchCheck API and records any non-4xx span error.
func (s TupleStore) batchCheck(ctx context.Context, req ClientBatchCheckRequest) (*openfga.BatchCheckResponse, error) {
	resp, err := s.client.BatchCheck(ctx, req)
	if err != nil {
		recordSpanError(ctx, err)
		return nil, err
	}
	return resp, nil
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

// removeInvalidWriteTuple returns a new slice with the first write tuple
// matching tupleStr removed. Returns the original slice and false if not found.
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

// removeInvalidDeleteTuple returns a new slice with the first delete tuple
// matching tupleStr removed. Returns the original slice and false if not found.
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
