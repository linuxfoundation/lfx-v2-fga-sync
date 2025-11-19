// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package constants

// OpenFGA relation constants for fine-grained authorization.
// These define the relationships between users and objects in the authorization model.
// Note: constants for one object type can still be the same as for another object type.
// (e.g. RelationViewer is the same for both project and meeting)
const (
	// Project relations
	RelationParent             = "parent"
	RelationOwner              = "owner"
	RelationWriter             = "writer"
	RelationAuditor            = "auditor"
	RelationMeetingCoordinator = "meeting_coordinator"
	RelationViewer             = "viewer"

	// Meeting relations
	RelationProject     = "project"
	RelationCommittee   = "committee"
	RelationOrganizer   = "organizer"
	RelationHost        = "host"
	RelationParticipant = "participant"
	RelationAttendee    = "attendee"
	RelationInvitee     = "invitee"
	RelationMeeting     = "meeting"
	RelationPastMeeting = "past_meeting"

	// Team relations
	RelationMember = "member"

	// Object type prefixes
	ObjectTypeUser                  = "user:"
	ObjectTypeProject               = "project:"
	ObjectTypeCommittee             = "committee:"
	ObjectTypeTeam                  = "team:"
	ObjectTypeMeeting               = "meeting:"
	ObjectTypePastMeeting           = "past_meeting:"
	ObjectTypePastMeetingRecording  = "past_meeting_recording:"
	ObjectTypePastMeetingTranscript = "past_meeting_transcript:"
	ObjectTypePastMeetingSummary    = "past_meeting_summary:"
	ObjectTypeGroupsIOService       = "groupsio_service:"
	ObjectTypeGroupsIOMailingList   = "groupsio_mailing_list:"

	// Special user identifiers
	UserWildcard = "user:*" // Public access (all users)

	// Operation types for logging and message processing
	OperationPut    = "put"
	OperationRemove = "remove"
)
