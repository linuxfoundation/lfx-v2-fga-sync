// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// The fga-sync service.
package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
	"github.com/openfga/go-sdk/client" // Only for client types, not the full SDK
)

type meetingStub struct {
	UID        string   `json:"uid"`
	Public     bool     `json:"public"`
	ProjectUID string   `json:"project_uid"`
	Organizers []string `json:"organizers"`
	Committees []string `json:"committees"`
}

// buildMeetingTuples builds all of the tuples for a meeting object.
func (h *HandlerService) buildMeetingTuples(
	object string,
	meeting *meetingStub,
) ([]client.ClientTupleKey, error) {
	tuples := h.fgaService.NewTupleKeySlice(4)

	// Convert the "public" attribute to a "user:*" relation.
	if meeting.Public {
		tuples = append(tuples, h.fgaService.TupleKey(constants.UserWildcard, constants.RelationViewer, object))
	}

	// Add the project relation to associate this meeting with its project
	if meeting.ProjectUID != "" {
		tuples = append(
			tuples,
			h.fgaService.TupleKey(constants.ObjectTypeProject+meeting.ProjectUID, constants.RelationProject, object),
		)
	}

	// Each committee set on the meeting according to the payload should have a committee relation with the meeting.
	for _, committee := range meeting.Committees {
		tuples = append(
			tuples,
			h.fgaService.TupleKey(constants.ObjectTypeCommittee+committee, constants.RelationCommittee, object),
		)
	}

	// Each organizer set on the meeting according to the payload should get the organizer relation.
	for _, principal := range meeting.Organizers {
		tuples = append(
			tuples,
			h.fgaService.TupleKey(constants.ObjectTypeUser+principal, constants.RelationOrganizer, object),
		)
	}

	return tuples, nil
}

// meetingUpdateAccessHandler handles meeting access control updates.
func (h *HandlerService) meetingUpdateAccessHandler(message INatsMsg) error {
	ctx := context.Background()

	logger.With("message", string(message.Data())).InfoContext(ctx, "handling meeting access control update")

	// Parse the event data.
	meeting := new(meetingStub)
	var err error
	err = json.Unmarshal(message.Data(), meeting)
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "event data parse error")
		return err
	}

	// Grab the project ID.
	if meeting.ProjectUID == "" {
		logger.ErrorContext(ctx, "meeting project ID not found")
		return errors.New("meeting project ID not found")
	}

	object := constants.ObjectTypeMeeting + meeting.UID

	// Build a list of tuples to sync. It should include all the tuples that need to be synced
	// with respect to the meeting object or else they will be deleted.
	//
	// Note: participant and host relations, however, are excluded from the sync operation
	// because they are managed separately via put_registrant and remove_registrant messages.
	tuples, err := h.buildMeetingTuples(object, meeting)
	if err != nil {
		logger.With(errKey, err, "object", object).ErrorContext(ctx, "failed to build meeting tuples")
		return err
	}

	// Sync the tuples.
	// Exclude participant and host relations from deletion - these are managed by other messages.
	tuplesWrites, tuplesDeletes, err := h.fgaService.SyncObjectTuples(
		ctx,
		object,
		tuples,
		constants.RelationParticipant,
		constants.RelationHost,
	)
	if err != nil {
		logger.With(errKey, err, "tuples", tuples, "object", object).ErrorContext(ctx, "failed to sync tuples")
		return err
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

		logger.With("object", object).InfoContext(ctx, "sent meeting access control update response")
	}

	return nil
}

// meetingDeleteAllAccessHandler handles deleting all tuples for a meeting object.
//
// This should only happen when a meeting is deleted.
func (h *HandlerService) meetingDeleteAllAccessHandler(message INatsMsg) error {
	return h.processDeleteAllAccessMessage(message, constants.ObjectTypeMeeting, "meeting")
}

type registrantStub struct {
	// UID is the registrant ID for the user's registration on the meeting.
	UID string `json:"uid"`
	// Username is the username (i.e. LFID) of the registrant. This is the identity of the user object in FGA.
	Username string `json:"username"`
	// MeetingUID is the meeting ID for the meeting the registrant is registered for.
	MeetingUID string `json:"meeting_uid"`
	// Host determines whether the user should get host relation on the meeting
	Host bool `json:"host"`
}

// registrantOperation defines the type of operation to perform on a registrant
type registrantOperation int

const (
	registrantPut registrantOperation = iota
	registrantRemove
)

// processRegistrantMessage handles the complete message processing flow for registrant operations
func (h *HandlerService) processRegistrantMessage(message INatsMsg, operation registrantOperation) error {
	ctx := context.Background()

	// Log the operation type
	operationType := constants.OperationPut
	responseMsg := "sent registrant put response"
	if operation == registrantRemove {
		operationType = constants.OperationRemove
		responseMsg = "sent registrant remove response"
	}

	logger.With("message", string(message.Data())).InfoContext(ctx, "handling meeting registrant "+operationType)

	// Parse the event data.
	registrant := new(registrantStub)
	err := json.Unmarshal(message.Data(), registrant)
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "event data parse error")
		return err
	}

	// Validate required fields.
	if registrant.Username == "" {
		logger.ErrorContext(ctx, "registrant username not found")
		return errors.New("registrant username not found")
	}
	if registrant.MeetingUID == "" {
		logger.ErrorContext(ctx, "meeting UID not found")
		return errors.New("meeting UID not found")
	}

	// Perform the FGA operation
	err = h.handleRegistrantOperation(ctx, registrant, operation)
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
			"meeting", constants.ObjectTypeMeeting+registrant.MeetingUID,
			"registrant", constants.ObjectTypeUser+registrant.Username,
		)
	}

	return nil
}

// handleRegistrantOperation handles the FGA operation for putting/removing registrants
func (h *HandlerService) handleRegistrantOperation(
	ctx context.Context,
	registrant *registrantStub,
	operation registrantOperation,
) error {
	meetingObject := constants.ObjectTypeMeeting + registrant.MeetingUID
	userPrincipal := constants.ObjectTypeUser + registrant.Username

	switch operation {
	case registrantPut:
		return h.putRegistrant(ctx, userPrincipal, meetingObject, registrant.Host)
	case registrantRemove:
		return h.removeRegistrant(ctx, userPrincipal, meetingObject, registrant.Host)
	default:
		return errors.New("unknown registrant operation")
	}
}

// putRegistrant implements idempotent put operation for registrant relations
func (h *HandlerService) putRegistrant(ctx context.Context, userPrincipal, meetingObject string, isHost bool) error {
	// Determine the desired relation
	desiredRelation := constants.RelationParticipant
	if isHost {
		desiredRelation = constants.RelationHost
	}

	// Read existing relations for this user on this meeting
	existingTuples, err := h.fgaService.ReadObjectTuples(ctx, meetingObject)
	if err != nil {
		logger.ErrorContext(ctx, "failed to read existing meeting tuples",
			errKey, err,
			"user", userPrincipal,
			"meeting", meetingObject,
		)
		return err
	}

	// Find existing registrant relations for this user
	var tuplesToDelete []client.ClientTupleKeyWithoutCondition
	var hasDesiredRelation bool

	for _, tuple := range existingTuples {
		if tuple.Key.User == userPrincipal &&
			(tuple.Key.Relation == constants.RelationParticipant || tuple.Key.Relation == constants.RelationHost) {
			if tuple.Key.Relation == desiredRelation {
				hasDesiredRelation = true
			} else {
				// This is an existing relation that needs to be removed
				tuplesToDelete = append(tuplesToDelete, client.ClientTupleKeyWithoutCondition{
					User:     tuple.Key.User,
					Relation: tuple.Key.Relation,
					Object:   tuple.Key.Object,
				})
			}
		}
	}

	// Prepare write operations
	var tuplesToWrite []client.ClientTupleKey
	if !hasDesiredRelation {
		tuplesToWrite = append(tuplesToWrite, h.fgaService.TupleKey(userPrincipal, desiredRelation, meetingObject))
	}

	// Apply changes if needed
	if len(tuplesToWrite) > 0 || len(tuplesToDelete) > 0 {
		err = h.fgaService.WriteAndDeleteTuples(ctx, tuplesToWrite, tuplesToDelete)
		if err != nil {
			logger.ErrorContext(ctx, "failed to put registrant tuple",
				errKey, err,
				"user", userPrincipal,
				"relation", desiredRelation,
				"meeting", meetingObject,
			)
			return err
		}

		logger.With(
			"user", userPrincipal,
			"relation", desiredRelation,
			"meeting", meetingObject,
		).InfoContext(ctx, "put registrant to meeting")
	} else {
		logger.With(
			"user", userPrincipal,
			"relation", desiredRelation,
			"meeting", meetingObject,
		).InfoContext(ctx, "registrant already has correct relation - no changes needed")
	}

	return nil
}

// removeRegistrant removes all registrant relations for a user from a meeting
func (h *HandlerService) removeRegistrant(ctx context.Context, userPrincipal, meetingObject string, isHost bool) error {
	// Determine the relation to remove
	relation := constants.RelationParticipant
	if isHost {
		relation = constants.RelationHost
	}

	err := h.fgaService.DeleteTuple(ctx, userPrincipal, relation, meetingObject)
	if err != nil {
		logger.ErrorContext(ctx, "failed to remove registrant tuple",
			errKey, err,
			"user", userPrincipal,
			"relation", relation,
			"meeting", meetingObject,
		)
		return err
	}

	logger.With(
		"user", userPrincipal,
		"relation", relation,
		"meeting", meetingObject,
	).InfoContext(ctx, "removed registrant from meeting")

	return nil
}

// meetingRegistrantPutHandler handles putting a registrant to a meeting (idempotent create/update).
func (h *HandlerService) meetingRegistrantPutHandler(message INatsMsg) error {
	return h.processRegistrantMessage(message, registrantPut)
}

// meetingRegistrantRemoveHandler handles removing a registrant from a meeting.
func (h *HandlerService) meetingRegistrantRemoveHandler(message INatsMsg) error {
	return h.processRegistrantMessage(message, registrantRemove)
}

type meetingAttachmentStub struct {
	UID        string `json:"uid"`
	MeetingUID string `json:"meeting_uid"`
}

// buildMeetingAttachmentTuples builds all of the tuples for a meeting attachment object.
func (h *HandlerService) buildMeetingAttachmentTuples(
	object string,
	attachment *meetingAttachmentStub,
) ([]client.ClientTupleKey, error) {
	tuples := h.fgaService.NewTupleKeySlice(1)

	// Add the meeting relation to associate this attachment with its meeting
	if attachment.MeetingUID != "" {
		tuples = append(
			tuples,
			h.fgaService.TupleKey(constants.ObjectTypeMeeting+attachment.MeetingUID, constants.RelationMeeting, object),
		)
	}

	return tuples, nil
}

// meetingAttachmentUpdateAccessHandler handles meeting attachment access control updates.
func (h *HandlerService) meetingAttachmentUpdateAccessHandler(message INatsMsg) error {
	ctx := context.Background()

	logger.With("message", string(message.Data())).InfoContext(ctx, "handling meeting attachment access control update")

	// Parse the event data.
	attachment := new(meetingAttachmentStub)
	var err error
	err = json.Unmarshal(message.Data(), attachment)
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "event data parse error")
		return err
	}

	// Validate required fields.
	if attachment.UID == "" {
		logger.ErrorContext(ctx, "meeting attachment UID not found")
		return errors.New("meeting attachment UID not found")
	}
	if attachment.MeetingUID == "" {
		logger.ErrorContext(ctx, "meeting UID not found")
		return errors.New("meeting UID not found")
	}

	object := constants.ObjectTypeMeetingAttachment + attachment.UID

	// Build a list of tuples to sync.
	//
	// It is important that all tuples that should exist with respect to the meeting attachment object
	// should be added to this tuples list because when SyncObjectTuples is called, it will delete
	// all tuples that are not in the tuples list parameter.
	tuples, err := h.buildMeetingAttachmentTuples(object, attachment)
	if err != nil {
		logger.With(errKey, err, "object", object).ErrorContext(ctx, "failed to build meeting attachment tuples")
		return err
	}

	tuplesWrites, tuplesDeletes, err := h.fgaService.SyncObjectTuples(ctx, object, tuples)
	if err != nil {
		logger.With(errKey, err, "tuples", tuples, "object", object).ErrorContext(ctx, "failed to sync tuples")
		return err
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

		logger.With("object", object).InfoContext(ctx, "sent meeting attachment access control update response")
	}

	return nil
}

// meetingAttachmentDeleteAccessHandler handles deleting all tuples for a meeting attachment object.
//
// This should happen when a meeting attachment is deleted.
func (h *HandlerService) meetingAttachmentDeleteAccessHandler(message INatsMsg) error {
	return h.processDeleteAllAccessMessage(message, constants.ObjectTypeMeetingAttachment, "meeting attachment")
}
