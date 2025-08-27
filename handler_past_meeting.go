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

type pastMeetingStub struct {
	UID        string   `json:"uid"`
	MeetingUID string   `json:"meeting_uid"`
	Public     bool     `json:"public"`
	ProjectUID string   `json:"project_uid"`
	Committees []string `json:"committees"`
	// The invitees and attendees are needed for the cases where when the past meeting is created the participants
	// are also created and therefore we need to add the invitees and attendees to the past meeting FGA object.
	// This case happens when handling the zoom meeting.started webhook event for example, because at that moment
	// the participants (invitees) get created in the system.
	//
	// The service sending the message to this service is responsible for determining the invitees and attendees.
	// Here we just add the FGA tuples for them.
	//
	// Note: no removal of the invitees and attendees is supported. This is just meant for when initially creating the
	// past meeting object.
	//
	// Both of these are arrays of usernames.
	Invitees  []string `json:"invitees"`
	Attendees []string `json:"attendees"`
}

// buildPastMeetingTuples builds all of the tuples for a past meeting object.
func (h *HandlerService) buildPastMeetingTuples(
	object string,
	meeting *pastMeetingStub,
) ([]client.ClientTupleKey, error) {
	tuples := h.fgaService.NewTupleKeySlice(4)

	// Convert the "public" attribute to a "user:*" relation.
	if meeting.Public {
		tuples = append(tuples, h.fgaService.TupleKey(constants.UserWildcard, constants.RelationViewer, object))
	}

	// Add the meeting relation to associate this past meeting with its meeting
	if meeting.MeetingUID != "" {
		tuples = append(
			tuples,
			h.fgaService.TupleKey(constants.ObjectTypeMeeting+meeting.MeetingUID, constants.RelationMeeting, object),
		)
	}

	// Add the project relation to associate this meeting with its project
	if meeting.ProjectUID != "" {
		tuples = append(
			tuples,
			h.fgaService.TupleKey(constants.ObjectTypeProject+meeting.ProjectUID, constants.RelationProject, object),
		)
	}

	// Each committee should have a committee relation with the past meeting.
	for _, committee := range meeting.Committees {
		tuples = append(
			tuples,
			h.fgaService.TupleKey(constants.ObjectTypeCommittee+committee, constants.RelationCommittee, object),
		)
	}

	// Each invitee should have an invitee relation with the past meeting.
	for _, invitee := range meeting.Invitees {
		tuples = append(
			tuples,
			h.fgaService.TupleKey(constants.ObjectTypeUser+invitee, constants.RelationInvitee, object),
		)
	}

	// Each attendee should have an attendee relation with the past meeting.
	for _, attendee := range meeting.Attendees {
		tuples = append(
			tuples,
			h.fgaService.TupleKey(constants.ObjectTypeUser+attendee, constants.RelationAttendee, object),
		)
	}

	return tuples, nil
}

// pastMeetingUpdateAccessHandler handles past meeting access control updates.
func (h *HandlerService) pastMeetingUpdateAccessHandler(message INatsMsg) error {
	ctx := context.Background()

	logger.With("message", string(message.Data())).InfoContext(ctx, "handling meeting access control update")

	// Parse the event data.
	pastMeeting := new(pastMeetingStub)
	var err error
	err = json.Unmarshal(message.Data(), pastMeeting)
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "event data parse error")
		return err
	}

	// Grab the project ID.
	if pastMeeting.ProjectUID == "" {
		logger.ErrorContext(ctx, "past meeting project ID not found")
		return errors.New("past meeting project ID not found")
	}

	object := constants.ObjectTypePastMeeting + pastMeeting.UID

	// Build a list of tuples to sync.
	//
	// It is important that all tuples that should exist with respect to the meeting object
	// should be added to this tuples list because when SyncObjectTuples is called, it will delete
	// all tuples that are not in the tuples list parameter.
	tuples, err := h.buildPastMeetingTuples(object, pastMeeting)
	if err != nil {
		logger.With(errKey, err, "object", object).ErrorContext(ctx, "failed to build meeting tuples")
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

		logger.With("object", object).InfoContext(ctx, "sent meeting access control update response")
	}

	return nil
}

// meetingDeleteAllAccessHandler handles deleting all tuples for a meeting object.
//
// This should only happen when a meeting is deleted.
func (h *HandlerService) pastMeetingDeleteAllAccessHandler(message INatsMsg) error {
	return h.processDeleteAllAccessMessage(message, constants.ObjectTypePastMeeting, "past_meeting")
}

type pastMeetingParticipantStub struct {
	UID            string `json:"uid"`
	PastMeetingUID string `json:"past_meeting_uid"`
	Username       string `json:"username"`
	Host           bool   `json:"host"`
	IsInvited      bool   `json:"is_invited"`
	IsAttended     bool   `json:"is_attended"`
}

// pastMeetingParticipantOperation defines the type of operation to perform on a past meeting participant
type pastMeetingParticipantOperation int

const (
	pastMeetingParticipantPut pastMeetingParticipantOperation = iota
	pastMeetingParticipantRemove
)

// processPastMeetingParticipantMessage handles the complete message processing flow for past meeting participant operations
func (h *HandlerService) processPastMeetingParticipantMessage(message INatsMsg, operation pastMeetingParticipantOperation) error {
	ctx := context.Background()

	// Log the operation type
	operationType := "put"
	responseMsg := "sent past meeting participant put response"
	if operation == pastMeetingParticipantRemove {
		operationType = "remove"
		responseMsg = "sent past meeting participant remove response"
	}

	logger.With("message", string(message.Data())).InfoContext(ctx, "handling past meeting participant "+operationType)

	// Parse the event data.
	pastMeetingParticipant := new(pastMeetingParticipantStub)
	err := json.Unmarshal(message.Data(), pastMeetingParticipant)
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "event data parse error")
		return err
	}

	// Validate required fields.
	if pastMeetingParticipant.Username == "" {
		logger.ErrorContext(ctx, "past meeting participant username not found")
		return errors.New("past meeting participant username not found")
	}
	if pastMeetingParticipant.PastMeetingUID == "" {
		logger.ErrorContext(ctx, "past meeting UID not found")
		return errors.New("past meeting UID not found")
	}

	// Perform the FGA operation
	err = h.handlePastMeetingParticipantOperation(ctx, pastMeetingParticipant, operation)
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
			"past_meeting", constants.ObjectTypePastMeeting+pastMeetingParticipant.PastMeetingUID,
			"past_meeting_participant", constants.ObjectTypeUser+pastMeetingParticipant.Username,
		)
	}

	return nil
}

// handlePastMeetingParticipantOperation handles the FGA operation for putting/removing past meeting participants
func (h *HandlerService) handlePastMeetingParticipantOperation(
	ctx context.Context,
	pastMeetingParticipant *pastMeetingParticipantStub,
	operation pastMeetingParticipantOperation,
) error {
	meetingObject := constants.ObjectTypePastMeeting + pastMeetingParticipant.PastMeetingUID
	userPrincipal := constants.ObjectTypeUser + pastMeetingParticipant.Username

	switch operation {
	case pastMeetingParticipantPut:
		return h.putPastMeetingParticipant(ctx, userPrincipal, meetingObject, pastMeetingParticipant)
	case pastMeetingParticipantRemove:
		return h.removePastMeetingParticipant(ctx, userPrincipal, meetingObject, pastMeetingParticipant)
	default:
		return errors.New("unknown past meeting participant operation")
	}
}

// putPastMeetingParticipant implements idempotent put operation for past meeting participant relations
func (h *HandlerService) putPastMeetingParticipant(ctx context.Context, userPrincipal, meetingObject string, participant *pastMeetingParticipantStub) error {
	// Determine the desired relations by looking at the attributes of the participant.
	// There is a separate relation to represent a host, attendee, and invitee. None are mutually exclusive.
	desiredRelationsMap := make(map[string]bool)
	if participant.Host {
		desiredRelationsMap[constants.RelationHost] = true
	}
	if participant.IsAttended {
		desiredRelationsMap[constants.RelationAttendee] = true
	}
	if participant.IsInvited {
		desiredRelationsMap[constants.RelationInvitee] = true
	}

	// Read existing relations for this user on this past meeting
	existingTuples, err := h.fgaService.ReadObjectTuples(ctx, meetingObject)
	if err != nil {
		logger.ErrorContext(ctx, "failed to read existing past meeting tuples",
			errKey, err,
			"user", userPrincipal,
			"meeting", meetingObject,
		)
		return err
	}

	// Find which relations need to be removed based on the desired relations compared to the existing relations.
	// For example, if a participant was marked as attended but the participant in the payload is not
	// marked as attended, we need to remove the attendee relation.
	var tuplesToDelete []client.ClientTupleKeyWithoutCondition
	alreadyHasDesiredRelationsMap := make(map[string]bool)
	for _, tuple := range existingTuples {
		if tuple.Key.User != userPrincipal {
			continue
		}

		matchesRelation := tuple.Key.Relation == constants.RelationHost || tuple.Key.Relation == constants.RelationAttendee || tuple.Key.Relation == constants.RelationInvitee
		if !matchesRelation {
			continue
		}

		if desiredRelationsMap[tuple.Key.Relation] {
			alreadyHasDesiredRelationsMap[tuple.Key.Relation] = true
			continue
		}

		// This is an existing relation that needs to be removed
		tuplesToDelete = append(tuplesToDelete, client.ClientTupleKeyWithoutCondition{
			User:     tuple.Key.User,
			Relation: tuple.Key.Relation,
			Object:   tuple.Key.Object,
		})
	}

	// Find which relations need to be added based on the desired relations compared to the existing relations.
	var tuplesToWrite []client.ClientTupleKey
	for relation := range desiredRelationsMap {
		if !alreadyHasDesiredRelationsMap[relation] {
			tuplesToWrite = append(tuplesToWrite, h.fgaService.TupleKey(userPrincipal, relation, meetingObject))
		}
	}

	// Apply changes if needed
	if len(tuplesToWrite) > 0 || len(tuplesToDelete) > 0 {
		err = h.fgaService.WriteAndDeleteTuples(ctx, tuplesToWrite, tuplesToDelete)
		if err != nil {
			logger.ErrorContext(ctx, "failed to put past meeting participant tuples for past meeting",
				errKey, err,
				"user", userPrincipal,
				"tuples_to_write", tuplesToWrite,
				"tuples_to_delete", tuplesToDelete,
				"object", meetingObject,
			)
			return err
		}

		logger.With(
			"user", userPrincipal,
			"relations", desiredRelationsMap,
			"object", meetingObject,
		).InfoContext(ctx, "put past meeting participant tuples for past meeting")
	} else {
		logger.With(
			"user", userPrincipal,
			"relations", desiredRelationsMap,
			"object", meetingObject,
		).InfoContext(ctx, "past meeting participant already has correct relations - no changes needed")
	}

	return nil
}

// removePastMeetingParticipant removes all past meeting participant relations for a user from a past meeting
func (h *HandlerService) removePastMeetingParticipant(ctx context.Context, userPrincipal, meetingObject string, participant *pastMeetingParticipantStub) error {
	// Determine the relations to remove based on the current participant attributes.
	tuplesToDelete := []client.ClientTupleKeyWithoutCondition{}
	if participant.Host {
		tuplesToDelete = append(
			tuplesToDelete,
			h.fgaService.TupleKeyWithoutCondition(userPrincipal, constants.RelationHost, meetingObject),
		)
	}
	if participant.IsAttended {
		tuplesToDelete = append(
			tuplesToDelete,
			h.fgaService.TupleKeyWithoutCondition(userPrincipal, constants.RelationAttendee, meetingObject),
		)
	}
	if participant.IsInvited {
		tuplesToDelete = append(
			tuplesToDelete,
			h.fgaService.TupleKeyWithoutCondition(userPrincipal, constants.RelationInvitee, meetingObject),
		)
	}

	err := h.fgaService.DeleteTuples(ctx, tuplesToDelete)
	if err != nil {
		logger.ErrorContext(ctx, "failed to remove past meeting participant tuples for past meeting",
			errKey, err,
			"user", userPrincipal,
			"tuples_to_delete", tuplesToDelete,
			"object", meetingObject,
		)
		return err
	}

	logger.With(
		"user", userPrincipal,
		"object", meetingObject,
	).InfoContext(ctx, "removed past meeting participant tuples for past meeting")

	return nil
}

// pastMeetingParticipantPutHandler handles putting a past meeting participant to a past meeting (idempotent create/update).
func (h *HandlerService) pastMeetingParticipantPutHandler(message INatsMsg) error {
	return h.processPastMeetingParticipantMessage(message, pastMeetingParticipantPut)
}

// pastMeetingParticipantRemoveHandler handles removing a past meeting participant from a past meeting.
func (h *HandlerService) pastMeetingParticipantRemoveHandler(message INatsMsg) error {
	return h.processPastMeetingParticipantMessage(message, pastMeetingParticipantRemove)
}
