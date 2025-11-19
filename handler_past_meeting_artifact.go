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

// PastMeetingRecordingAccessMessage is the schema for the data in the message sent to the fga-sync service.
// These are the fields that the fga-sync service needs in order to update the OpenFGA permissions for recordings.
type PastMeetingRecordingAccessMessage struct {
	UID                string                   `json:"uid"`
	PastMeetingUID     string                   `json:"past_meeting_uid"`
	ArtifactVisibility string                   `json:"artifact_visibility"`
	Participants       []PastMeetingParticipant `json:"participants"`
}

// PastMeetingTranscriptAccessMessage is the schema for the data in the message sent to the fga-sync service.
// These are the fields that the fga-sync service needs in order to update the OpenFGA permissions for transcripts.
type PastMeetingTranscriptAccessMessage struct {
	UID                string                   `json:"uid"`
	PastMeetingUID     string                   `json:"past_meeting_uid"`
	ArtifactVisibility string                   `json:"artifact_visibility"`
	Participants       []PastMeetingParticipant `json:"participants"`
}

// PastMeetingSummaryAccessMessage is the schema for the data in the message sent to the fga-sync service.
// These are the fields that the fga-sync service needs in order to update the OpenFGA permissions for summaries.
type PastMeetingSummaryAccessMessage struct {
	UID                string                   `json:"uid"`
	PastMeetingUID     string                   `json:"past_meeting_uid"`
	ArtifactVisibility string                   `json:"artifact_visibility"`
	Participants       []PastMeetingParticipant `json:"participants"`
}

// buildPastMeetingArtifactTuples builds all of the tuples for a past meeting artifact
// (recording, transcript, or summary).
func (h *HandlerService) buildPastMeetingArtifactTuples(
	object string,
	pastMeetingUID string,
	artifactVisibility string,
	participants []PastMeetingParticipant,
) ([]client.ClientTupleKey, error) {
	tuples := h.fgaService.NewTupleKeySlice(4)

	// Add the past_meeting relation to associate this artifact with its past meeting
	if pastMeetingUID != "" {
		tuples = append(
			tuples,
			h.fgaService.TupleKey(constants.ObjectTypePastMeeting+pastMeetingUID, constants.RelationPastMeeting, object),
		)
	}

	// Handle artifact visibility
	switch artifactVisibility {
	case constants.VisibilityPublic:
		// Public access - all users get viewer access
		tuples = append(tuples, h.fgaService.TupleKey(constants.UserWildcard, constants.RelationViewer, object))

	case constants.VisibilityMeetingHosts:
		// Only hosts get viewer access
		for _, participant := range participants {
			if participant.Host && participant.Username != "" {
				tuples = append(
					tuples,
					h.fgaService.TupleKey(constants.ObjectTypeUser+participant.Username, constants.RelationViewer, object),
				)
			}
		}

	case constants.VisibilityMeetingParticipants:
		// All participants get viewer access
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

// pastMeetingRecordingUpdateAccessHandler handles past meeting recording access control updates.
func (h *HandlerService) pastMeetingRecordingUpdateAccessHandler(message INatsMsg) error {
	ctx := context.Background()

	logger.With("message", string(message.Data())).InfoContext(
		ctx,
		"handling past meeting recording access control update",
	)

	// Parse the event data.
	recording := new(PastMeetingRecordingAccessMessage)
	err := json.Unmarshal(message.Data(), recording)
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "event data parse error")
		return err
	}

	// Validate required fields.
	if recording.PastMeetingUID == "" {
		logger.ErrorContext(ctx, "past meeting UID not found")
		return errors.New("past meeting UID not found")
	}

	object := constants.ObjectTypePastMeetingRecording + recording.UID

	// Build a list of tuples to sync.
	tuples, err := h.buildPastMeetingArtifactTuples(
		object,
		recording.PastMeetingUID,
		recording.ArtifactVisibility,
		recording.Participants,
	)
	if err != nil {
		logger.With(errKey, err, "object", object).ErrorContext(ctx, "failed to build past meeting recording tuples")
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

		logger.With("object", object).InfoContext(ctx, "sent past meeting recording access control update response")
	}

	return nil
}

// pastMeetingTranscriptUpdateAccessHandler handles past meeting transcript access control updates.
func (h *HandlerService) pastMeetingTranscriptUpdateAccessHandler(message INatsMsg) error {
	ctx := context.Background()

	logger.With("message", string(message.Data())).InfoContext(
		ctx,
		"handling past meeting transcript access control update",
	)

	// Parse the event data.
	transcript := new(PastMeetingTranscriptAccessMessage)
	err := json.Unmarshal(message.Data(), transcript)
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "event data parse error")
		return err
	}

	// Validate required fields.
	if transcript.PastMeetingUID == "" {
		logger.ErrorContext(ctx, "past meeting UID not found")
		return errors.New("past meeting UID not found")
	}

	object := constants.ObjectTypePastMeetingTranscript + transcript.UID

	// Build a list of tuples to sync.
	tuples, err := h.buildPastMeetingArtifactTuples(
		object,
		transcript.PastMeetingUID,
		transcript.ArtifactVisibility,
		transcript.Participants,
	)
	if err != nil {
		logger.With(errKey, err, "object", object).ErrorContext(ctx, "failed to build past meeting transcript tuples")
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

		logger.With("object", object).InfoContext(ctx, "sent past meeting transcript access control update response")
	}

	return nil
}

// pastMeetingSummaryUpdateAccessHandler handles past meeting summary access control updates.
func (h *HandlerService) pastMeetingSummaryUpdateAccessHandler(message INatsMsg) error {
	ctx := context.Background()

	logger.With("message", string(message.Data())).InfoContext(ctx, "handling past meeting summary access control update")

	// Parse the event data.
	summary := new(PastMeetingSummaryAccessMessage)
	err := json.Unmarshal(message.Data(), summary)
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "event data parse error")
		return err
	}

	// Validate required fields.
	if summary.PastMeetingUID == "" {
		logger.ErrorContext(ctx, "past meeting UID not found")
		return errors.New("past meeting UID not found")
	}

	object := constants.ObjectTypePastMeetingSummary + summary.UID

	// Build a list of tuples to sync.
	tuples, err := h.buildPastMeetingArtifactTuples(
		object,
		summary.PastMeetingUID,
		summary.ArtifactVisibility,
		summary.Participants,
	)
	if err != nil {
		logger.With(errKey, err, "object", object).ErrorContext(ctx, "failed to build past meeting summary tuples")
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

		logger.With("object", object).InfoContext(ctx, "sent past meeting summary access control update response")
	}

	return nil
}
