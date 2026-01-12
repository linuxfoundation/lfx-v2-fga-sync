// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// The fga-sync service.
package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
	"github.com/openfga/go-sdk/client"
)

const (
	operationTypePut    = "put"
	operationTypeRemove = "remove"
)

// ============================================================================
// Meeting Handlers
// ============================================================================

type meetingStub struct {
	UID        string   `json:"uid"`
	Public     bool     `json:"public"`
	ProjectUID string   `json:"project_uid"`
	Organizers []string `json:"organizers"`
	Committees []string `json:"committees"`
}

// toStandardAccessStub converts meetingStub to standardAccessStub format.
func (m *meetingStub) toStandardAccessStub() *standardAccessStub {
	stub := &standardAccessStub{
		UID:        m.UID,
		ObjectType: "meeting", // Without colon - processStandardAccessUpdate adds it
		Public:     m.Public,
		Relations:  make(map[string][]string),
		References: make(map[string][]string),
	}

	// Convert project_uid to References
	if m.ProjectUID != "" {
		stub.References[constants.RelationProject] = []string{m.ProjectUID}
	}

	// Convert committees to References
	if len(m.Committees) > 0 {
		stub.References[constants.RelationCommittee] = m.Committees
	}

	// Convert organizers to Relations
	if len(m.Organizers) > 0 {
		stub.Relations[constants.RelationOrganizer] = m.Organizers
	}

	return stub
}

// meetingUpdateAccessHandler handles meeting access control updates.
func (h *HandlerService) meetingUpdateAccessHandler(message INatsMsg) error {
	ctx := context.Background()

	// Parse the event data.
	meeting := new(meetingStub)
	err := json.Unmarshal(message.Data(), meeting)
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "event data parse error")
		return err
	}

	// Validate project ID.
	if meeting.ProjectUID == "" {
		logger.ErrorContext(ctx, "meeting project ID not found")
		return errors.New("meeting project ID not found")
	}

	// Convert to standard format
	standardAccess := meeting.toStandardAccessStub()

	// Use the generic handler with excluded relations.
	// Exclude participant and host relations from deletion - these are managed by other messages.
	return h.processStandardAccessUpdate(
		message,
		standardAccess,
		constants.RelationParticipant,
		constants.RelationHost,
	)
}

// meetingDeleteAllAccessHandler handles deleting all tuples for a meeting object.
//
// This should only happen when a meeting is deleted.
func (h *HandlerService) meetingDeleteAllAccessHandler(message INatsMsg) error {
	return h.processDeleteAllAccessMessage(message, constants.ObjectTypeMeeting, "meeting")
}

// ============================================================================
// Meeting Registrant Handlers
// ============================================================================

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

// meetingRegistrantPutHandler handles putting a registrant to a meeting (idempotent create/update).
func (h *HandlerService) meetingRegistrantPutHandler(message INatsMsg) error {
	// Parse registrant-specific format
	registrant := new(registrantStub)
	if err := json.Unmarshal(message.Data(), registrant); err != nil {
		return err
	}

	// Convert to generic format
	genericMember := &memberOperationStub{
		Username:  registrant.Username,
		ObjectUID: registrant.MeetingUID,
	}

	// Determine the relation based on whether they're a host
	relation := constants.RelationParticipant
	if registrant.Host {
		relation = constants.RelationHost
	}

	// Configure with mutually exclusive relations
	// When setting a registrant as participant or host, the opposite relation should be removed
	config := memberOperationConfig{
		objectTypePrefix:      constants.ObjectTypeMeeting,
		objectTypeName:        "meeting",
		relation:              relation,
		mutuallyExclusiveWith: []string{constants.RelationParticipant, constants.RelationHost},
	}

	return h.processMemberOperation(message, genericMember, memberOperationPut, config)
}

// meetingRegistrantRemoveHandler handles removing a registrant from a meeting.
func (h *HandlerService) meetingRegistrantRemoveHandler(message INatsMsg) error {
	// Parse registrant-specific format
	registrant := new(registrantStub)
	if err := json.Unmarshal(message.Data(), registrant); err != nil {
		return err
	}

	// Convert to generic format
	genericMember := &memberOperationStub{
		Username:  registrant.Username,
		ObjectUID: registrant.MeetingUID,
	}

	// Determine the relation based on whether they're a host
	relation := constants.RelationParticipant
	if registrant.Host {
		relation = constants.RelationHost
	}

	// Configure with mutually exclusive relations
	config := memberOperationConfig{
		objectTypePrefix:      constants.ObjectTypeMeeting,
		objectTypeName:        "meeting",
		relation:              relation,
		mutuallyExclusiveWith: []string{constants.RelationParticipant, constants.RelationHost},
	}

	return h.processMemberOperation(message, genericMember, memberOperationRemove, config)
}

// ============================================================================
// Meeting Attachment Handlers
// ============================================================================

type meetingAttachmentStub struct {
	UID        string `json:"uid"`
	MeetingUID string `json:"meeting_uid"`
}

// meetingAttachmentUpdateAccessHandler handles meeting attachment access control updates.
func (h *HandlerService) meetingAttachmentUpdateAccessHandler(message INatsMsg) error {
	ctx := context.Background()

	logger.With("message", string(message.Data())).InfoContext(ctx, "handling meeting attachment access control update")

	// Parse the event data
	attachment := new(meetingAttachmentStub)
	if err := json.Unmarshal(message.Data(), attachment); err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "event data parse error")
		return err
	}

	// Validate required fields
	if attachment.UID == "" {
		logger.ErrorContext(ctx, "meeting attachment UID not found")
		return errors.New("meeting attachment UID not found")
	}
	if attachment.MeetingUID == "" {
		logger.ErrorContext(ctx, "meeting UID not found")
		return errors.New("meeting UID not found")
	}

	object := constants.ObjectTypeMeetingAttachment + attachment.UID

	// Build tuples - associate attachment with its meeting
	tuples := h.fgaService.NewTupleKeySlice(1)
	if attachment.MeetingUID != "" {
		tuples = append(
			tuples,
			h.fgaService.TupleKey(constants.ObjectTypeMeeting+attachment.MeetingUID, constants.RelationMeeting, object),
		)
	}

	// Sync tuples
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

	// Reply handling
	if message.Reply() != "" {
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

// ============================================================================
// Past Meeting Handlers
// ============================================================================

type pastMeetingStub struct {
	UID        string   `json:"uid"`
	MeetingUID string   `json:"meeting_uid"`
	Public     bool     `json:"public"`
	ProjectUID string   `json:"project_uid"`
	Committees []string `json:"committees"`
}

// toStandardAccessStub converts pastMeetingStub to standardAccessStub format.
func (p *pastMeetingStub) toStandardAccessStub() *standardAccessStub {
	stub := &standardAccessStub{
		UID:        p.UID,
		ObjectType: "past_meeting", // Without colon - processStandardAccessUpdate adds it
		Public:     p.Public,
		Relations:  make(map[string][]string),
		References: make(map[string][]string),
	}

	// Convert meeting_uid to References
	if p.MeetingUID != "" {
		stub.References[constants.RelationMeeting] = []string{p.MeetingUID}
	}

	// Convert project_uid to References
	if p.ProjectUID != "" {
		stub.References[constants.RelationProject] = []string{p.ProjectUID}
	}

	// Convert committees to References
	if len(p.Committees) > 0 {
		stub.References[constants.RelationCommittee] = p.Committees
	}

	return stub
}

// pastMeetingUpdateAccessHandler handles past meeting access control updates.
func (h *HandlerService) pastMeetingUpdateAccessHandler(message INatsMsg) error {
	ctx := context.Background()

	// Parse the event data.
	pastMeeting := new(pastMeetingStub)
	err := json.Unmarshal(message.Data(), pastMeeting)
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "event data parse error")
		return err
	}

	// Validate project ID.
	if pastMeeting.ProjectUID == "" {
		logger.With("past_meeting", pastMeeting).ErrorContext(ctx, "past meeting project ID not found")
		return errors.New("past meeting project ID not found")
	}

	// Convert to standard format
	standardAccess := pastMeeting.toStandardAccessStub()

	// Use the generic handler with excluded relations.
	// Exclude organizer, host, invitee, and attendee relations from deletion - these are managed by other messages.
	return h.processStandardAccessUpdate(
		message,
		standardAccess,
		constants.RelationOrganizer,
		constants.RelationHost,
		constants.RelationInvitee,
		constants.RelationAttendee,
	)
}

// pastMeetingDeleteAllAccessHandler handles deleting all tuples for a past meeting object.
//
// This should only happen when a past meeting is deleted.
func (h *HandlerService) pastMeetingDeleteAllAccessHandler(message INatsMsg) error {
	return h.processDeleteAllAccessMessage(message, constants.ObjectTypePastMeeting, "past_meeting")
}

// ============================================================================
// Past Meeting Participant Handlers
// ============================================================================

type pastMeetingParticipantStub struct {
	UID                string `json:"uid"`
	PastMeetingUID     string `json:"past_meeting_uid"`
	ArtifactVisibility string `json:"artifact_visibility"`
	Username           string `json:"username"`
	Host               bool   `json:"host"`
	IsInvited          bool   `json:"is_invited"`
	IsAttended         bool   `json:"is_attended"`
}

// pastMeetingParticipantOperation defines the type of operation to perform on a past meeting participant
type pastMeetingParticipantOperation int

const (
	pastMeetingParticipantPut pastMeetingParticipantOperation = iota
	pastMeetingParticipantRemove
)

// processPastMeetingParticipantMessage handles the complete message processing flow for past meeting
// participant operations
func (h *HandlerService) processPastMeetingParticipantMessage(
	message INatsMsg,
	operation pastMeetingParticipantOperation,
) error {
	ctx := context.Background()

	// Log the operation type
	operationType := operationTypePut
	responseMsg := "sent past meeting participant put response"
	if operation == pastMeetingParticipantRemove {
		operationType = operationTypeRemove
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
		logger.With("participant", pastMeetingParticipant).ErrorContext(ctx, "past meeting participant username not found")
		return errors.New("past meeting participant username not found")
	}
	if pastMeetingParticipant.PastMeetingUID == "" {
		logger.With("participant", pastMeetingParticipant).ErrorContext(ctx, "past meeting UID not found")
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
	pastMeetingObject := constants.ObjectTypePastMeeting + pastMeetingParticipant.PastMeetingUID
	userPrincipal := constants.ObjectTypeUser + pastMeetingParticipant.Username

	switch operation {
	case pastMeetingParticipantPut:
		return h.putPastMeetingParticipant(ctx, userPrincipal, pastMeetingObject, pastMeetingParticipant)
	case pastMeetingParticipantRemove:
		return h.removePastMeetingParticipant(ctx, userPrincipal, pastMeetingObject, pastMeetingParticipant)
	default:
		return errors.New("unknown past meeting participant operation")
	}
}

// putPastMeetingParticipant implements idempotent put operation for past meeting participant relations
func (h *HandlerService) putPastMeetingParticipant(
	ctx context.Context,
	userPrincipal,
	pastMeetingObject string,
	participant *pastMeetingParticipantStub,
) error {
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
	existingTuples, err := h.fgaService.ReadObjectTuples(ctx, pastMeetingObject)
	if err != nil {
		logger.ErrorContext(ctx, "failed to read existing past meeting tuples",
			errKey, err,
			"user", userPrincipal,
			"past_meeting", pastMeetingObject,
		)
		return err
	}

	// Find which relations need to be removed based on the desired relations compared to the existing relations.
	// For example, if a participant was marked as attended but the participant in the payload is not
	// marked as attended, we need to remove the attendee relation.
	tuplesToDelete := make([]client.ClientTupleKeyWithoutCondition, 0)
	alreadyHasDesiredRelationsMap := make(map[string]bool)
	for _, tuple := range existingTuples {
		if tuple.Key.User != userPrincipal {
			continue
		}

		matchesRelation := tuple.Key.Relation == constants.RelationHost ||
			tuple.Key.Relation == constants.RelationAttendee ||
			tuple.Key.Relation == constants.RelationInvitee
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
			tuplesToWrite = append(tuplesToWrite, h.fgaService.TupleKey(userPrincipal, relation, pastMeetingObject))
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
				"object", pastMeetingObject,
			)
			return err
		}

		logger.With(
			"user", userPrincipal,
			"relations", desiredRelationsMap,
			"object", pastMeetingObject,
		).InfoContext(ctx, "put past meeting participant tuples for past meeting")
	} else {
		logger.With(
			"user", userPrincipal,
			"relations", desiredRelationsMap,
			"object", pastMeetingObject,
		).InfoContext(ctx, "past meeting participant already has correct past_meeting relations")
	}

	return nil
}

// removePastMeetingParticipant removes all existing past meeting participant relations for a user from a past meeting
func (h *HandlerService) removePastMeetingParticipant(
	ctx context.Context,
	userPrincipal,
	pastMeetingObject string,
	participant *pastMeetingParticipantStub,
) error {
	err := h.fgaService.DeleteTuplesByUserAndObject(ctx, userPrincipal, pastMeetingObject)
	if err != nil {
		logger.ErrorContext(ctx, "failed to remove past meeting participant tuples for past meeting",
			errKey, err,
			"user", userPrincipal,
			"object", pastMeetingObject,
		)
		return err
	}

	logger.With(
		"user", userPrincipal,
		"object", pastMeetingObject,
	).InfoContext(ctx, "removed past meeting participant tuples for past meeting")

	return nil
}

// pastMeetingParticipantPutHandler handles putting a past meeting participant to a past meeting
// (idempotent create/update).
func (h *HandlerService) pastMeetingParticipantPutHandler(message INatsMsg) error {
	return h.processPastMeetingParticipantMessage(message, pastMeetingParticipantPut)
}

// pastMeetingParticipantRemoveHandler handles removing a past meeting participant from a past meeting.
func (h *HandlerService) pastMeetingParticipantRemoveHandler(message INatsMsg) error {
	return h.processPastMeetingParticipantMessage(message, pastMeetingParticipantRemove)
}

// ============================================================================
// Past Meeting Attachment Handlers
// ============================================================================

type pastMeetingAttachmentStub struct {
	UID            string `json:"uid"`
	PastMeetingUID string `json:"past_meeting_uid"`
}

// pastMeetingAttachmentUpdateAccessHandler handles past meeting attachment access control updates.
func (h *HandlerService) pastMeetingAttachmentUpdateAccessHandler(message INatsMsg) error {
	ctx := context.Background()

	logger.With("message", string(message.Data())).InfoContext(ctx, "handling past meeting attachment access control update")

	// Parse the event data
	attachment := new(pastMeetingAttachmentStub)
	if err := json.Unmarshal(message.Data(), attachment); err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "event data parse error")
		return err
	}

	// Validate required fields
	if attachment.UID == "" {
		logger.ErrorContext(ctx, "past meeting attachment UID not found")
		return errors.New("past meeting attachment UID not found")
	}
	if attachment.PastMeetingUID == "" {
		logger.ErrorContext(ctx, "past meeting UID not found")
		return errors.New("past meeting UID not found")
	}

	object := constants.ObjectTypePastMeetingAttachment + attachment.UID

	// Build tuples - associate attachment with its past meeting
	tuples := h.fgaService.NewTupleKeySlice(1)
	if attachment.PastMeetingUID != "" {
		tuples = append(
			tuples,
			h.fgaService.TupleKey(
				constants.ObjectTypePastMeeting+attachment.PastMeetingUID,
				constants.RelationPastMeeting,
				object,
			),
		)
	}

	// Sync tuples
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

	// Reply handling
	if message.Reply() != "" {
		if err = message.Respond([]byte("OK")); err != nil {
			logger.With(errKey, err).WarnContext(ctx, "failed to send reply")
			return err
		}

		logger.With("object", object).InfoContext(ctx, "sent past meeting attachment access control update response")
	}

	return nil
}

// pastMeetingAttachmentDeleteAccessHandler handles deleting all tuples for a past meeting attachment object.
//
// This should happen when a past meeting attachment is deleted.
func (h *HandlerService) pastMeetingAttachmentDeleteAccessHandler(message INatsMsg) error {
	return h.processDeleteAllAccessMessage(message, constants.ObjectTypePastMeetingAttachment, "past meeting attachment")
}

// ============================================================================
// Past Meeting Artifact Handlers (Recording, Transcript, Summary)
// ============================================================================

// PastMeetingParticipant represents a participant of a past meeting.
type PastMeetingParticipant struct {
	Username string `json:"username"`
	Host     bool   `json:"host"`
}

// artifactAccessMessage is a generic structure for past meeting artifact access messages.
// This is used for recordings, transcripts, and summaries.
type artifactAccessMessage struct {
	UID                string `json:"uid"`
	PastMeetingUID     string `json:"past_meeting_uid"`
	ArtifactVisibility string `json:"artifact_visibility"`
}

// artifactConfig configures the behavior of artifact operations
type artifactConfig struct {
	objectTypePrefix string // e.g., constants.ObjectTypePastMeetingRecording
	objectTypeName   string // e.g., "past meeting recording" (for logging)
}

// processArtifactUpdate handles artifact access control updates generically
func (h *HandlerService) processArtifactUpdate(
	message INatsMsg,
	artifact *artifactAccessMessage,
	config artifactConfig,
) error {
	ctx := context.Background()

	logger.With("message", string(message.Data())).InfoContext(ctx, "handling "+config.objectTypeName+" access control update")

	// Validate required fields
	if artifact.PastMeetingUID == "" {
		logger.ErrorContext(ctx, "past meeting UID not found")
		return errors.New("past meeting UID not found")
	}

	// Build object identifier
	object := config.objectTypePrefix + artifact.UID

	// Build tuples using the shared artifact logic
	tuples, err := h.buildPastMeetingArtifactTuples(object, artifact.PastMeetingUID, artifact.ArtifactVisibility)
	if err != nil {
		logger.With(errKey, err, "object", object).ErrorContext(ctx, "failed to build "+config.objectTypeName+" tuples")
		return err
	}

	// Sync tuples
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

	// Reply handling
	if message.Reply() != "" {
		if err = message.Respond([]byte("OK")); err != nil {
			logger.With(errKey, err).WarnContext(ctx, "failed to send reply")
			return err
		}

		logger.With("object", object).InfoContext(ctx, "sent "+config.objectTypeName+" access control update response")
	}

	return nil
}

// buildPastMeetingArtifactTuples builds all of the tuples for a past meeting artifact
// (recording, transcript, or summary).
func (h *HandlerService) buildPastMeetingArtifactTuples(
	object string,
	pastMeetingUID string,
	artifactVisibility string,
) ([]client.ClientTupleKey, error) {
	tuples := h.fgaService.NewTupleKeySlice(4)

	// Add the past_meeting relation to associate this artifact with its past meeting
	if pastMeetingUID != "" {
		tuples = append(
			tuples,
			h.fgaService.TupleKey(constants.ObjectTypePastMeeting+pastMeetingUID, constants.RelationPastMeeting, object),
		)
	}

	// Handle artifact visibility.
	switch artifactVisibility {
	case constants.VisibilityPublic:
		// Public access - all users get viewer access.
		tuples = append(tuples, h.fgaService.TupleKey(constants.UserWildcard, constants.RelationViewer, object))

	case constants.VisibilityMeetingHosts:
		// Only hosts get viewer access.
		tuples = append(
			tuples,
			h.fgaService.TupleKey(
				constants.ObjectTypePastMeeting+pastMeetingUID,
				constants.RelationPastMeetingForHostView,
				object,
			),
		)

	case constants.VisibilityMeetingParticipants:
		// All participants get viewer access.
		tuples = append(
			tuples,
			h.fgaService.TupleKey(
				constants.ObjectTypePastMeeting+pastMeetingUID,
				constants.RelationPastMeetingForHostView,
				object,
			),
			h.fgaService.TupleKey(
				constants.ObjectTypePastMeeting+pastMeetingUID,
				constants.RelationPastMeetingForAttendeeView,
				object,
			),
			h.fgaService.TupleKey(
				constants.ObjectTypePastMeeting+pastMeetingUID,
				constants.RelationPastMeetingForParticipantView,
				object,
			),
		)

	default:
		logger.ErrorContext(context.Background(), "unknown artifact visibility", "visibility", artifactVisibility)
		return nil, errors.New("unknown artifact visibility: " + artifactVisibility)
	}

	return tuples, nil
}

// pastMeetingRecordingUpdateAccessHandler handles past meeting recording access control updates.
func (h *HandlerService) pastMeetingRecordingUpdateAccessHandler(message INatsMsg) error {
	// Parse the artifact message
	artifact := new(artifactAccessMessage)
	if err := json.Unmarshal(message.Data(), artifact); err != nil {
		return err
	}

	// Configure for recording
	config := artifactConfig{
		objectTypePrefix: constants.ObjectTypePastMeetingRecording,
		objectTypeName:   "past meeting recording",
	}

	return h.processArtifactUpdate(message, artifact, config)
}

// pastMeetingTranscriptUpdateAccessHandler handles past meeting transcript access control updates.
func (h *HandlerService) pastMeetingTranscriptUpdateAccessHandler(message INatsMsg) error {
	// Parse the artifact message
	artifact := new(artifactAccessMessage)
	if err := json.Unmarshal(message.Data(), artifact); err != nil {
		return err
	}

	// Configure for transcript
	config := artifactConfig{
		objectTypePrefix: constants.ObjectTypePastMeetingTranscript,
		objectTypeName:   "past meeting transcript",
	}

	return h.processArtifactUpdate(message, artifact, config)
}

// pastMeetingSummaryUpdateAccessHandler handles past meeting summary access control updates.
func (h *HandlerService) pastMeetingSummaryUpdateAccessHandler(message INatsMsg) error {
	// Parse the artifact message
	artifact := new(artifactAccessMessage)
	if err := json.Unmarshal(message.Data(), artifact); err != nil {
		return err
	}

	// Configure for summary
	config := artifactConfig{
		objectTypePrefix: constants.ObjectTypePastMeetingSummary,
		objectTypeName:   "past meeting summary",
	}

	return h.processArtifactUpdate(message, artifact, config)
}
