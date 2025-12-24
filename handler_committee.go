// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// The fga-sync service.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/linuxfoundation/lfx-v2-fga-sync/internal/domain"
	"github.com/linuxfoundation/lfx-v2-fga-sync/internal/service"
	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
	"github.com/openfga/go-sdk/client" // Only for client types, not the full SDK
)

type committeeStub struct {
	UID        string              `json:"uid"`
	ObjectType string              `json:"object_type"`
	Public     bool                `json:"public"`
	Relations  map[string][]string `json:"relations"`
	References map[string]string   `json:"references"`
	Policies   []domain.Policy     `json:"policies"`
}

// committeeUpdateAccessHandler handles committee access control updates.
func (h *HandlerService) committeeUpdateAccessHandler(message INatsMsg) error {
	ctx := context.Background()

	logger.With("message", string(message.Data())).InfoContext(ctx, "handling committee access control update")

	// Parse the event data.
	committee := new(committeeStub)
	var err error
	err = json.Unmarshal(message.Data(), committee)
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "event data parse error")
		return err
	}

	if committee.UID == "" {
		logger.ErrorContext(ctx, "committee ID not found")
		return errors.New("committee ID not found")
	}

	object := fmt.Sprintf("%s:%s", committee.ObjectType, committee.UID)

	// Build a list of tuples to sync.
	tuples := h.fgaService.NewTupleKeySlice(4)

	// Convert the "public" attribute to a "user:*" relation.
	if committee.Public {
		tuples = append(tuples, h.fgaService.TupleKey(constants.UserWildcard, constants.RelationViewer, object))
	}

	// for parent relation, project relation, etc
	for reference, value := range committee.References {
		refType := reference
		if reference == constants.RelationParent {
			refType = committee.ObjectType
		}

		key := fmt.Sprintf("%s:%s", refType, value)
		tuples = append(tuples, h.fgaService.TupleKey(key, reference, object))
	}

	// Add each principal from the object as the corresponding relationship tuple
	// (as defined in the OpenFGA schema).
	// for writer, auditor etc
	for relation, principals := range committee.Relations {
		for _, principal := range principals {
			tuples = append(tuples, h.fgaService.TupleKey(constants.ObjectTypeUser+principal, relation, object))
		}
	}

	// Sync committee tuples
	tuplesWrites, tuplesDeletes, err := h.fgaService.SyncObjectTuples(ctx, object, tuples, "member")
	if err != nil {
		logger.With(errKey, err, "tuples", tuples, "object", object).ErrorContext(ctx, "failed to sync tuples")
		return err
	}

	if len(committee.Policies) > 0 {
		policyEval := service.NewPolicyHandler(logger, h.fgaService)

		// Evaluate each policy associated with the committee
		for _, policy := range committee.Policies {
			errEvaluatePolicy := policyEval.EvaluatePolicy(ctx, policy, object, "member")
			if errEvaluatePolicy != nil {
				logger.With(errKey, errEvaluatePolicy,
					"policy", policy,
					"object", object,
				).ErrorContext(ctx, "failed to evaluate policy")
				return errEvaluatePolicy
			}
		}
	}

	logger.With(
		"tuples", tuples,
		"object", object,
		"writes", tuplesWrites,
		"deletes", tuplesDeletes,
	).InfoContext(ctx, "synced tuples")

	if message.Reply() != "" {
		// Send a reply if an inbox was provided.
		if err = message.Respond([]byte("OK")); err != nil {
			logger.With(errKey, err).WarnContext(ctx, "failed to send reply")
			return err
		}

		logger.With("object", object).InfoContext(ctx, "sent project access control update response")
	}

	return nil
}

// committeeDeleteAllAccessHandler handles committee access control deletions.
func (h *HandlerService) committeeDeleteAllAccessHandler(message INatsMsg) error {
	return h.processDeleteAllAccessMessage(message, constants.ObjectTypeCommittee, "committee")
}

type committeeMemberStub struct {
	// Username is the username (i.e. LFID) of the member. This is the identity of the user object in FGA.
	Username string `json:"username"`
	// CommitteeUID is the committee ID for the committee the member belongs to.
	CommitteeUID string `json:"committee_uid"`
}

// committeeMemberOperation defines the type of operation to perform on a committee member
type committeeMemberOperation int

const (
	committeeMemberPut committeeMemberOperation = iota
	committeeMemberRemove
)

// processCommitteeMemberMessage handles the complete message processing flow for committee member operations
func (h *HandlerService) processCommitteeMemberMessage(message INatsMsg, operation committeeMemberOperation) error {
	ctx := context.Background()

	// Log the operation type
	operationType := constants.OperationPut
	responseMsg := "sent committee member put response"
	if operation == committeeMemberRemove {
		operationType = constants.OperationRemove
		responseMsg = "sent committee member remove response"
	}

	logger.With("message", string(message.Data())).InfoContext(ctx, "handling committee member "+operationType)

	// Parse the event data.
	member := new(committeeMemberStub)
	err := json.Unmarshal(message.Data(), member)
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "event data parse error")
		return err
	}

	// Validate required fields.
	if member.Username == "" {
		logger.ErrorContext(ctx, "committee member username not found")
		return errors.New("committee member username not found")
	}
	if member.CommitteeUID == "" {
		logger.ErrorContext(ctx, "committee UID not found")
		return errors.New("committee UID not found")
	}

	// Perform the FGA operation
	err = h.handleCommitteeMemberOperation(ctx, member, operation)
	if err != nil {
		return err
	}

	// Send reply if requested
	if message.Reply() != "" {
		if err = message.Respond([]byte("OK")); err != nil {
			logger.With(errKey, err).WarnContext(ctx, "failed to send reply")
			return err
		}

		logger.InfoContext(ctx, responseMsg,
			"committee", constants.ObjectTypeCommittee+member.CommitteeUID,
			"member", constants.ObjectTypeUser+member.Username,
		)
	}

	return nil
}

// handleCommitteeMemberOperation handles the FGA operation for putting/removing committee members
func (h *HandlerService) handleCommitteeMemberOperation(
	ctx context.Context,
	member *committeeMemberStub,
	operation committeeMemberOperation,
) error {
	committeeObject := constants.ObjectTypeCommittee + member.CommitteeUID
	userPrincipal := constants.ObjectTypeUser + member.Username

	switch operation {
	case committeeMemberPut:
		return h.putCommitteeMember(ctx, userPrincipal, committeeObject)
	case committeeMemberRemove:
		return h.removeCommitteeMember(ctx, userPrincipal, committeeObject)
	default:
		return errors.New("unknown committee member operation")
	}
}

// putCommitteeMember implements idempotent put operation for committee member relations
func (h *HandlerService) putCommitteeMember(ctx context.Context, userPrincipal, committeeObject string) error {
	// Read existing relations for this user on this committee
	existingTuples, err := h.fgaService.ReadObjectTuples(ctx, committeeObject)
	if err != nil {
		logger.ErrorContext(ctx, "failed to read existing committee tuples",
			errKey, err,
			"user", userPrincipal,
			"committee", committeeObject,
		)
		return err
	}

	// Check if the member relation already exists
	var hasMemberRelation bool
	for _, tuple := range existingTuples {
		if tuple.Key.User == userPrincipal && tuple.Key.Relation == constants.RelationMember {
			hasMemberRelation = true
			break
		}
	}

	// Only write if the relation doesn't already exist
	if !hasMemberRelation {
		tuples := []client.ClientTupleKey{
			h.fgaService.TupleKey(userPrincipal, constants.RelationMember, committeeObject),
		}

		err = h.fgaService.WriteTuples(ctx, tuples)
		if err != nil {
			logger.ErrorContext(ctx, "failed to put committee member tuple",
				errKey, err,
				"user", userPrincipal,
				"relation", constants.RelationMember,
				"committee", committeeObject,
			)
			return err
		}

		logger.With(
			"user", userPrincipal,
			"relation", constants.RelationMember,
			"committee", committeeObject,
		).InfoContext(ctx, "put member to committee")
	} else {
		logger.With(
			"user", userPrincipal,
			"relation", constants.RelationMember,
			"committee", committeeObject,
		).InfoContext(ctx, "member already has correct relation - no changes needed")
	}

	return nil
}

// removeCommitteeMember removes the member relation for a user from a committee
func (h *HandlerService) removeCommitteeMember(ctx context.Context, userPrincipal, committeeObject string) error {
	err := h.fgaService.DeleteTuple(ctx, userPrincipal, constants.RelationMember, committeeObject)
	if err != nil {
		logger.ErrorContext(ctx, "failed to remove committee member tuple",
			errKey, err,
			"user", userPrincipal,
			"relation", constants.RelationMember,
			"committee", committeeObject,
		)
		return err
	}

	logger.With(
		"user", userPrincipal,
		"relation", constants.RelationMember,
		"committee", committeeObject,
	).InfoContext(ctx, "removed member from committee")

	return nil
}

// committeeMemberPutHandler handles putting a member to a committee (idempotent create/update).
func (h *HandlerService) committeeMemberPutHandler(message INatsMsg) error {
	return h.processCommitteeMemberMessage(message, committeeMemberPut)
}

// committeeMemberRemoveHandler handles removing a member from a committee.
func (h *HandlerService) committeeMemberRemoveHandler(message INatsMsg) error {
	return h.processCommitteeMemberMessage(message, committeeMemberRemove)
}
