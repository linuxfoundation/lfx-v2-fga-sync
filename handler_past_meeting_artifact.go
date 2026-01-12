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
