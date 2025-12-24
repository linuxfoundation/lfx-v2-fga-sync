// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"log/slog"

	"github.com/linuxfoundation/lfx-v2-fga-sync/internal/domain"
	"github.com/openfga/go-sdk/client"
)

// PolicyHandler defines the interface for handling fine-grained authorization policies.
type PolicyHandler interface {
	EvaluatePolicy(ctx context.Context, policy domain.Policy, objectID string) error
}

type policyHandler struct {
	synchronizer RelationshipSynchronizer
	logger       *slog.Logger
}

// EvaluatePolicy creates and syncs the two-level policy relationship structure.
//
// This creates a two-level relationship structure for policy evaluation:
//
//  1. Link the object to the policy: objectID -> policy.Name -> policy.Name:policy.Value
//     e.g., committee:C#visibility_policy@visibility_policy:basic_profile
//
//  2. Link the policy to user relation: policy.Name:policy.Value -> policy.Relation -> objectID#relation
//     e.g., visibility_policy:basic_profile#allows_basic_profile@committee:C#member
//
// OpenFGA expands this as:
//
//	Object: {{objectID}}
//	└── relation: {{policy.Name}} → user: {{policy.Name:policy.Value}}
//	    └── relation: {{policy.Relation}} → user: {{objectID}}#member
//	        └── contains: user:{{userID}}
//
// Example:
//
//	Object: committee:1234
//	└── relation: visibility_policy → user: visibility_policy:basic_profile
//	    └── relation: allows_basic_profile → user: committee:1234#member
//	        └── contains: user:user_5678
func (ph *policyHandler) EvaluatePolicy(ctx context.Context, policy domain.Policy, objectID string) error {
	// Validate policy using domain validation
	if err := policy.Validate(); err != nil {
		return err
	}

	if objectID == "" {
		return errors.New("object ID is required for policy evaluation")
	}

	// Helper function to check existing tuples
	// for a given object, user, and relation
	// If exact tuple exists, it will not be added again
	// If conflicting tuples exist, they will be marked for deletion
	checkTuple := func(
		object, user, relation string,
	) ([]client.ClientTupleKey, []client.ClientTupleKeyWithoutCondition, error) {
		existingTuples, errReadObjectTuples := ph.synchronizer.ReadObjectTuples(ctx, object)
		if errReadObjectTuples != nil {
			ph.logger.With("error", errReadObjectTuples, "object", object).Error("failed to read existing object tuples")
			return nil, nil, errReadObjectTuples
		}

		var (
			tuplesToWrite  []client.ClientTupleKey
			tuplesToDelete []client.ClientTupleKeyWithoutCondition
		)

		exists := false
		for _, tuple := range existingTuples {

			if tuple.Key.User == user && tuple.Key.Relation != relation {
				tuplesToDelete = append(tuplesToDelete, ph.synchronizer.TupleKeyWithoutCondition(user, tuple.Key.Relation, object))
				continue
			}
			if tuple.Key.User == user && tuple.Key.Relation == relation {
				exists = true
				continue
			}
		}

		// If no existing tuple found, prepare to write a new one
		if !exists {
			tuplesToWrite = append(tuplesToWrite, ph.synchronizer.TupleKey(user, relation, object))
		}

		return tuplesToWrite, tuplesToDelete, nil
	}

	var (
		tuplesToWrite  []client.ClientTupleKey
		tuplesToDelete []client.ClientTupleKeyWithoutCondition
	)

	// Get the policy object ID from domain
	policyObject := policy.ObjectID()

	// 1. Link the object to the policy
	// Format: objectID -> policy.Name -> policy.Name:policy.Value
	// Example: committee:C#visibility_policy@visibility_policy:basic_profile
	writeObjPolicy, deleteObjPolicy, err := checkTuple(objectID, policyObject, policy.Name)
	if err != nil {
		return err
	}
	tuplesToWrite = append(tuplesToWrite, writeObjPolicy...)
	tuplesToDelete = append(tuplesToDelete, deleteObjPolicy...)

	// 2. Link the policy to user relation
	// Format: policy.Name:policy.Value -> policy.Relation -> objectID#userRelation
	// Example: visibility_policy:basic_profile#allows_basic_profile@committee:C#member
	userRelation := policy.UserRelation(objectID, "member") // Default to "member" relation
	writePolicyRelation, deletePolicyRelation, err := checkTuple(policyObject, userRelation, policy.Relation)
	if err != nil {
		return err
	}
	tuplesToWrite = append(tuplesToWrite, writePolicyRelation...)
	tuplesToDelete = append(tuplesToDelete, deletePolicyRelation...)

	ph.logger.With(
		"objectID", objectID,
		"policy", policy,
		"tuplesToWrite", tuplesToWrite,
		"tuplesToDelete", tuplesToDelete,
	).Debug("prepared policy tuples for synchronization")

	// Write tuples only if there are new ones to write or delete
	if len(tuplesToWrite) > 0 || len(tuplesToDelete) > 0 {
		errWriteAndDeleteTuples := ph.synchronizer.WriteAndDeleteTuples(ctx, tuplesToWrite, tuplesToDelete)
		if errWriteAndDeleteTuples != nil {
			ph.logger.With(
				"error", errWriteAndDeleteTuples,
				"tuplesToWrite", tuplesToWrite,
				"tuplesToDelete", tuplesToDelete,
			).Error("failed to write and delete policy tuples")
			return errWriteAndDeleteTuples
		}
	}

	return nil
}

// NewPolicyHandler creates a new instance of PolicyHandler with the given RelationshipSynchronizer.
func NewPolicyHandler(logger *slog.Logger, synchronizer RelationshipSynchronizer) PolicyHandler {
	return &policyHandler{
		synchronizer: synchronizer,
		logger:       logger,
	}
}
