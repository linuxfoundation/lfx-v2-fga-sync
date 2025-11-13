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

// v1MeetingStub represents the structure of v1 meeting data for FGA sync.
type v1MeetingStub struct {
	UID        string   `json:"uid"`
	Public     bool     `json:"public"`
	ProjectUID string   `json:"project_uid"`
	Committees []string `json:"committees"`
	Hosts      []string `json:"hosts"`
}

// buildV1MeetingTuples builds all of the tuples for a v1 meeting object.
func (h *HandlerService) buildV1MeetingTuples(
	object string,
	meeting *v1MeetingStub,
) ([]client.ClientTupleKey, error) {
	tuples := h.fgaService.NewTupleKeySlice(4)

	// Convert the "public" attribute to a "user:*" relation.
	if meeting.Public {
		tuples = append(tuples, h.fgaService.TupleKey(constants.UserWildcard, constants.RelationViewer, object))
	}

	// Add the project relation to associate this v1 meeting with its project.
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

	// Each host set on the meeting according to the payload should get the host relation.
	// Note: v1 meetings don't have explicit organizers like v2, only hosts.
	for _, host := range meeting.Hosts {
		tuples = append(
			tuples,
			h.fgaService.TupleKey(constants.ObjectTypeUser+host, constants.RelationHost, object),
		)
	}

	return tuples, nil
}

// v1MeetingUpdateAccessHandler handles v1 meeting access control updates.
func (h *HandlerService) v1MeetingUpdateAccessHandler(message INatsMsg) error {
	ctx := context.Background()

	logger.With("message", string(message.Data())).InfoContext(ctx, "handling v1 meeting access control update")

	// Parse the event data.
	meeting := new(v1MeetingStub)
	var err error
	err = json.Unmarshal(message.Data(), meeting)
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "event data parse error")
		return err
	}

	// Grab the project ID.
	if meeting.ProjectUID == "" {
		logger.ErrorContext(ctx, "v1 meeting project ID not found")
		return errors.New("v1 meeting project ID not found")
	}

	object := constants.ObjectTypeV1Meeting + meeting.UID

	// Build a list of tuples to sync.
	//
	// It is important that all tuples that should exist with respect to the v1 meeting object
	// should be added to this tuples list because when SyncObjectTuples is called, it will delete
	// all tuples that are not in the tuples list parameter.
	tuples, err := h.buildV1MeetingTuples(object, meeting)
	if err != nil {
		logger.With(errKey, err, "object", object).ErrorContext(ctx, "failed to build v1 meeting tuples")
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

		logger.With("object", object).InfoContext(ctx, "sent v1 meeting access control update response")
	}

	return nil
}

// v1MeetingDeleteAllAccessHandler handles deleting all tuples for a v1 meeting object.
//
// This should only happen when a v1 meeting is deleted.
func (h *HandlerService) v1MeetingDeleteAllAccessHandler(message INatsMsg) error {
	return h.processDeleteAllAccessMessage(message, constants.ObjectTypeV1Meeting, "v1_meeting")
}

// v1PastMeetingStub represents the structure of v1 past meeting data for FGA sync.
type v1PastMeetingStub struct {
	UID          string   `json:"uid"`
	V1MeetingUID string   `json:"v1_meeting_uid"`
	Public       bool     `json:"public"`
	ProjectUID   string   `json:"project_uid"`
	Committees   []string `json:"committees"`
}

// buildV1PastMeetingTuples builds all of the tuples for a v1 past meeting object.
func (h *HandlerService) buildV1PastMeetingTuples(
	object string,
	pastMeeting *v1PastMeetingStub,
) ([]client.ClientTupleKey, error) {
	tuples := h.fgaService.NewTupleKeySlice(4)

	// Convert the "public" attribute to a "user:*" relation.
	if pastMeeting.Public {
		tuples = append(tuples, h.fgaService.TupleKey(constants.UserWildcard, constants.RelationViewer, object))
	}

	// Add the meeting relation to associate this v1 past meeting with its v1 meeting.
	if pastMeeting.V1MeetingUID != "" {
		tuples = append(
			tuples,
			h.fgaService.TupleKey(constants.ObjectTypeV1Meeting+pastMeeting.V1MeetingUID, constants.RelationMeeting, object),
		)
	}

	// Add the project relation to associate this v1 past meeting with its project.
	if pastMeeting.ProjectUID != "" {
		tuples = append(
			tuples,
			h.fgaService.TupleKey(constants.ObjectTypeProject+pastMeeting.ProjectUID, constants.RelationProject, object),
		)
	}

	// Each committee should have a committee relation with the v1 past meeting.
	for _, committee := range pastMeeting.Committees {
		tuples = append(
			tuples,
			h.fgaService.TupleKey(constants.ObjectTypeCommittee+committee, constants.RelationCommittee, object),
		)
	}

	return tuples, nil
}

// v1PastMeetingUpdateAccessHandler handles v1 past meeting access control updates.
func (h *HandlerService) v1PastMeetingUpdateAccessHandler(message INatsMsg) error {
	ctx := context.Background()

	logger.With("message", string(message.Data())).InfoContext(ctx, "handling v1 past meeting access control update")

	// Parse the event data.
	pastMeeting := new(v1PastMeetingStub)
	var err error
	err = json.Unmarshal(message.Data(), pastMeeting)
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "event data parse error")
		return err
	}

	// Grab the project ID.
	if pastMeeting.ProjectUID == "" {
		logger.ErrorContext(ctx, "v1 past meeting project ID not found")
		return errors.New("v1 past meeting project ID not found")
	}

	object := constants.ObjectTypeV1PastMeeting + pastMeeting.UID

	// Build a list of tuples to sync.
	//
	// It is important that all tuples that should exist with respect to the v1 past meeting object
	// should be added to this tuples list because when SyncObjectTuples is called, it will delete
	// all tuples that are not in the tuples list parameter.
	tuples, err := h.buildV1PastMeetingTuples(object, pastMeeting)
	if err != nil {
		logger.With(errKey, err, "object", object).ErrorContext(ctx, "failed to build v1 past meeting tuples")
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

		logger.With("object", object).InfoContext(ctx, "sent v1 past meeting access control update response")
	}

	return nil
}

// v1PastMeetingDeleteAllAccessHandler handles deleting all tuples for a v1 past meeting object.
//
// This should only happen when a v1 past meeting is deleted.
func (h *HandlerService) v1PastMeetingDeleteAllAccessHandler(message INatsMsg) error {
	return h.processDeleteAllAccessMessage(message, constants.ObjectTypeV1PastMeeting, "v1_past_meeting")
}

// V1PastMeetingParticipant represents a participant of a v1 past meeting.
type V1PastMeetingParticipant struct {
	Username   string `json:"username"`
	Host       bool   `json:"host"`
	IsInvited  bool   `json:"is_invited"`
	IsAttended bool   `json:"is_attended"`
}

// V1PastMeetingRecordingAccessMessage is the schema for the data in the message sent to the fga-sync service.
// These are the fields that the fga-sync service needs in order to update the OpenFGA permissions for v1 recordings.
type V1PastMeetingRecordingAccessMessage struct {
	UID                string                     `json:"uid"`
	V1PastMeetingUID   string                     `json:"v1_past_meeting_uid"`
	ArtifactVisibility string                     `json:"artifact_visibility"`
	Participants       []V1PastMeetingParticipant `json:"participants"`
}

// V1PastMeetingTranscriptAccessMessage is the schema for the data in the message sent to the fga-sync service.
// These are the fields that the fga-sync service needs in order to update the OpenFGA permissions for v1 transcripts.
type V1PastMeetingTranscriptAccessMessage struct {
	UID                string                     `json:"uid"`
	V1PastMeetingUID   string                     `json:"v1_past_meeting_uid"`
	ArtifactVisibility string                     `json:"artifact_visibility"`
	Participants       []V1PastMeetingParticipant `json:"participants"`
}

// V1PastMeetingSummaryAccessMessage is the schema for the data in the message sent to the fga-sync service.
// These are the fields that the fga-sync service needs in order to update the OpenFGA permissions for v1 summaries.
type V1PastMeetingSummaryAccessMessage struct {
	UID                string                     `json:"uid"`
	V1PastMeetingUID   string                     `json:"v1_past_meeting_uid"`
	ArtifactVisibility string                     `json:"artifact_visibility"`
	Participants       []V1PastMeetingParticipant `json:"participants"`
}

// buildV1PastMeetingArtifactTuples builds all of the tuples for a v1 past meeting artifact
// (recording, transcript, or summary).
func (h *HandlerService) buildV1PastMeetingArtifactTuples(
	object string,
	v1PastMeetingUID string,
	artifactVisibility string,
	participants []V1PastMeetingParticipant,
) ([]client.ClientTupleKey, error) {
	tuples := h.fgaService.NewTupleKeySlice(4)

	// Add the past_meeting relation to associate this artifact with its v1 past meeting.
	if v1PastMeetingUID != "" {
		tuples = append(
			tuples,
			h.fgaService.TupleKey(constants.ObjectTypeV1PastMeeting+v1PastMeetingUID, constants.RelationPastMeeting, object),
		)
	}

	// Handle artifact visibility.
	switch artifactVisibility {
	case "public":
		// Public access - all users get viewer access.
		tuples = append(tuples, h.fgaService.TupleKey(constants.UserWildcard, constants.RelationViewer, object))

	case "meeting_hosts":
		// Only hosts get viewer access.
		for _, participant := range participants {
			if participant.Host && participant.Username != "" {
				tuples = append(
					tuples,
					h.fgaService.TupleKey(constants.ObjectTypeUser+participant.Username, constants.RelationViewer, object),
				)
			}
		}

	case "meeting_participants":
		// All participants get viewer access.
		for _, participant := range participants {
			if participant.Username != "" {
				tuples = append(
					tuples,
					h.fgaService.TupleKey(constants.ObjectTypeUser+participant.Username, constants.RelationViewer, object),
				)
			}
		}

	default:
		logger.ErrorContext(context.Background(), "unknown artifact visibility", "visibility", artifactVisibility)
		return nil, errors.New("unknown artifact visibility: " + artifactVisibility)
	}

	return tuples, nil
}

// v1PastMeetingRecordingUpdateAccessHandler handles v1 past meeting recording access control updates.
func (h *HandlerService) v1PastMeetingRecordingUpdateAccessHandler(message INatsMsg) error {
	ctx := context.Background()

	logger.With("message", string(message.Data())).InfoContext(
		ctx,
		"handling v1 past meeting recording access control update",
	)

	// Parse the event data.
	recording := new(V1PastMeetingRecordingAccessMessage)
	err := json.Unmarshal(message.Data(), recording)
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "event data parse error")
		return err
	}

	// Validate required fields.
	if recording.V1PastMeetingUID == "" {
		logger.ErrorContext(ctx, "v1 past meeting UID not found")
		return errors.New("v1 past meeting UID not found")
	}

	object := constants.ObjectTypeV1PastMeetingRecording + recording.UID

	// Build a list of tuples to sync.
	tuples, err := h.buildV1PastMeetingArtifactTuples(
		object,
		recording.V1PastMeetingUID,
		recording.ArtifactVisibility,
		recording.Participants,
	)
	if err != nil {
		logger.With(errKey, err, "object", object).ErrorContext(ctx, "failed to build v1 past meeting recording tuples")
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

		logger.With("object", object).InfoContext(ctx, "sent v1 past meeting recording access control update response")
	}

	return nil
}

// v1PastMeetingTranscriptUpdateAccessHandler handles v1 past meeting transcript access control updates.
func (h *HandlerService) v1PastMeetingTranscriptUpdateAccessHandler(message INatsMsg) error {
	ctx := context.Background()

	logger.With("message", string(message.Data())).InfoContext(
		ctx,
		"handling v1 past meeting transcript access control update",
	)

	// Parse the event data.
	transcript := new(V1PastMeetingTranscriptAccessMessage)
	err := json.Unmarshal(message.Data(), transcript)
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "event data parse error")
		return err
	}

	// Validate required fields.
	if transcript.V1PastMeetingUID == "" {
		logger.ErrorContext(ctx, "v1 past meeting UID not found")
		return errors.New("v1 past meeting UID not found")
	}

	object := constants.ObjectTypeV1PastMeetingTranscript + transcript.UID

	// Build a list of tuples to sync.
	tuples, err := h.buildV1PastMeetingArtifactTuples(
		object,
		transcript.V1PastMeetingUID,
		transcript.ArtifactVisibility,
		transcript.Participants,
	)
	if err != nil {
		logger.With(errKey, err, "object", object).ErrorContext(ctx, "failed to build v1 past meeting transcript tuples")
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

		logger.With("object", object).InfoContext(ctx, "sent v1 past meeting transcript access control update response")
	}

	return nil
}

// v1PastMeetingSummaryUpdateAccessHandler handles v1 past meeting summary access control updates.
func (h *HandlerService) v1PastMeetingSummaryUpdateAccessHandler(message INatsMsg) error {
	ctx := context.Background()

	logger.With("message", string(message.Data())).InfoContext(ctx, "handling v1 past meeting summary access control update")

	// Parse the event data.
	summary := new(V1PastMeetingSummaryAccessMessage)
	err := json.Unmarshal(message.Data(), summary)
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "event data parse error")
		return err
	}

	// Validate required fields.
	if summary.V1PastMeetingUID == "" {
		logger.ErrorContext(ctx, "v1 past meeting UID not found")
		return errors.New("v1 past meeting UID not found")
	}

	object := constants.ObjectTypeV1PastMeetingSummary + summary.UID

	// Build a list of tuples to sync.
	tuples, err := h.buildV1PastMeetingArtifactTuples(
		object,
		summary.V1PastMeetingUID,
		summary.ArtifactVisibility,
		summary.Participants,
	)
	if err != nil {
		logger.With(errKey, err, "object", object).ErrorContext(ctx, "failed to build v1 past meeting summary tuples")
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

		logger.With("object", object).InfoContext(ctx, "sent v1 past meeting summary access control update response")
	}

	return nil
}

// v1RegistrantStub represents the structure of v1 meeting registrant data for FGA sync.
// Note: v1 uses "ID" instead of "UID" for meeting and registrant identifiers.
type v1RegistrantStub struct {
	// ID is the registrant ID for the user's registration on the meeting.
	ID string `json:"id"`
	// Username is the username (i.e. LFID) of the registrant. This is the identity of the user object in FGA.
	Username string `json:"username"`
	// MeetingID is the meeting ID for the meeting the registrant is registered for.
	MeetingID string `json:"meeting_id"`
	// Host determines whether the user should get host relation on the meeting.
	Host bool `json:"host"`
}

// v1RegistrantOperation defines the type of operation to perform on a v1 registrant.
type v1RegistrantOperation int

const (
	v1RegistrantPut v1RegistrantOperation = iota
	v1RegistrantRemove
)

// v1ProcessRegistrantMessage handles the complete message processing flow for v1 registrant operations.
func (h *HandlerService) v1ProcessRegistrantMessage(message INatsMsg, operation v1RegistrantOperation) error {
	ctx := context.Background()

	// Log the operation type.
	operationType := "put"
	responseMsg := "sent v1 registrant put response"
	if operation == v1RegistrantRemove {
		operationType = "remove"
		responseMsg = "sent v1 registrant remove response"
	}

	logger.With("message", string(message.Data())).InfoContext(ctx, "handling v1 meeting registrant "+operationType)

	// Parse the event data.
	registrant := new(v1RegistrantStub)
	err := json.Unmarshal(message.Data(), registrant)
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "event data parse error")
		return err
	}

	// Validate required fields.
	if registrant.Username == "" {
		logger.ErrorContext(ctx, "v1 registrant username not found")
		return errors.New("v1 registrant username not found")
	}
	if registrant.MeetingID == "" {
		logger.ErrorContext(ctx, "v1 meeting ID not found")
		return errors.New("v1 meeting ID not found")
	}

	// Perform the FGA operation.
	err = h.v1HandleRegistrantOperation(ctx, registrant, operation)
	if err != nil {
		return err
	}

	// Send a reply if an inbox was provided.
	replySubject := message.Reply()
	if replySubject != "" {
		if err = message.Respond([]byte("OK")); err != nil {
			logger.With(errKey, err).WarnContext(ctx, "failed to send reply")
			return err
		}

		logger.InfoContext(ctx, responseMsg,
			"meeting", constants.ObjectTypeV1Meeting+registrant.MeetingID,
			"registrant", constants.ObjectTypeUser+registrant.Username,
		)
	}

	return nil
}

// v1HandleRegistrantOperation handles the FGA operation for putting/removing v1 registrants.
func (h *HandlerService) v1HandleRegistrantOperation(
	ctx context.Context,
	registrant *v1RegistrantStub,
	operation v1RegistrantOperation,
) error {
	meetingObject := constants.ObjectTypeV1Meeting + registrant.MeetingID
	userPrincipal := constants.ObjectTypeUser + registrant.Username

	switch operation {
	case v1RegistrantPut:
		return h.v1PutRegistrant(ctx, userPrincipal, meetingObject, registrant.Host)
	case v1RegistrantRemove:
		return h.v1RemoveRegistrant(ctx, userPrincipal, meetingObject, registrant.Host)
	default:
		return errors.New("unknown v1 registrant operation")
	}
}

// v1PutRegistrant implements idempotent put operation for v1 registrant relations.
func (h *HandlerService) v1PutRegistrant(ctx context.Context, userPrincipal, meetingObject string, isHost bool) error {
	// Determine the desired relation.
	desiredRelation := constants.RelationParticipant
	if isHost {
		desiredRelation = constants.RelationHost
	}

	// Read existing relations for this user on this meeting.
	existingTuples, err := h.fgaService.ReadObjectTuples(ctx, meetingObject)
	if err != nil {
		logger.ErrorContext(ctx, "failed to read existing v1 meeting tuples",
			errKey, err,
			"meeting", meetingObject,
		)
		return err
	}

	// Find existing registrant relations for this user.
	var tuplesToDelete []client.ClientTupleKeyWithoutCondition
	var hasDesiredRelation bool

	for _, tuple := range existingTuples {
		if tuple.Key.User == userPrincipal &&
			(tuple.Key.Relation == constants.RelationParticipant || tuple.Key.Relation == constants.RelationHost) {
			if tuple.Key.Relation == desiredRelation {
				hasDesiredRelation = true
			} else {
				// This is an existing relation that needs to be removed.
				tuplesToDelete = append(tuplesToDelete, client.ClientTupleKeyWithoutCondition{
					User:     tuple.Key.User,
					Relation: tuple.Key.Relation,
					Object:   tuple.Key.Object,
				})
			}
		}
	}

	// Prepare write operations.
	var tuplesToWrite []client.ClientTupleKey
	if !hasDesiredRelation {
		tuplesToWrite = append(tuplesToWrite, h.fgaService.TupleKey(userPrincipal, desiredRelation, meetingObject))
	}

	// Apply changes if needed.
	if len(tuplesToWrite) > 0 || len(tuplesToDelete) > 0 {
		err = h.fgaService.WriteAndDeleteTuples(ctx, tuplesToWrite, tuplesToDelete)
		if err != nil {
			logger.ErrorContext(ctx, "failed to put v1 registrant tuple",
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
		).InfoContext(ctx, "put v1 registrant to meeting")
	} else {
		logger.With(
			"user", userPrincipal,
			"relation", desiredRelation,
			"meeting", meetingObject,
		).InfoContext(ctx, "v1 registrant already has correct relation - no changes needed")
	}

	return nil
}

// v1RemoveRegistrant removes all registrant relations for a user from a v1 meeting.
func (h *HandlerService) v1RemoveRegistrant(ctx context.Context, userPrincipal, meetingObject string, isHost bool) error {
	// Determine the relation to remove.
	relation := constants.RelationParticipant
	if isHost {
		relation = constants.RelationHost
	}

	err := h.fgaService.DeleteTuple(ctx, userPrincipal, relation, meetingObject)
	if err != nil {
		logger.ErrorContext(ctx, "failed to remove v1 registrant tuple",
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
	).InfoContext(ctx, "removed v1 registrant from meeting")

	return nil
}

// v1MeetingRegistrantPutHandler handles putting a registrant to a v1 meeting (idempotent create/update).
func (h *HandlerService) v1MeetingRegistrantPutHandler(message INatsMsg) error {
	return h.v1ProcessRegistrantMessage(message, v1RegistrantPut)
}

// v1MeetingRegistrantRemoveHandler handles removing a registrant from a v1 meeting.
func (h *HandlerService) v1MeetingRegistrantRemoveHandler(message INatsMsg) error {
	return h.v1ProcessRegistrantMessage(message, v1RegistrantRemove)
}
