// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package constants

// NATS Key-Value store bucket names.
const (
	// KVBucketNameSyncCache is the name of the KV bucket for the FGA sync cache.
	KVBucketNameSyncCache = "fga-sync-cache"
)

// NATS wildcard subjects that the FGA sync service handles messages about.
const (
	// AccessCheckSubject is the subject for the access check request.
	// The subject is of the form: lfx.access_check.request
	AccessCheckSubject = "lfx.access_check.request"

	// ProjectUpdateAccessSubject is the subject for the project access control updates.
	// The subject is of the form: lfx.update_access.project
	ProjectUpdateAccessSubject = "lfx.update_access.project"

	// ProjectDeleteAllAccessSubject is the subject for the project access control deletion.
	// The subject is of the form: lfx.delete_all_access.project
	ProjectDeleteAllAccessSubject = "lfx.delete_all_access.project"

	// MeetingUpdateAccessSubject is the subject for the meeting access control updates.
	// The subject is of the form: lfx.update_access.meeting
	MeetingUpdateAccessSubject = "lfx.update_access.meeting"

	// MeetingDeleteAllAccessSubject is the subject for the meeting access control deletion.
	// The subject is of the form: lfx.delete_all_access.meeting
	MeetingDeleteAllAccessSubject = "lfx.delete_all_access.meeting"

	// MeetingRegistrantPutSubject is the subject for adding meeting registrants.
	// The subject is of the form: lfx.put_registrant.meeting
	MeetingRegistrantPutSubject = "lfx.put_registrant.meeting"

	// MeetingRegistrantRemoveSubject is the subject for removing meeting registrants.
	// The subject is of the form: lfx.remove_registrant.meeting
	MeetingRegistrantRemoveSubject = "lfx.remove_registrant.meeting"

	// MeetingAttachmentUpdateAccessSubject is the subject for the meeting attachment access control updates.
	// The subject is of the form: lfx.update_access.meeting_attachment
	MeetingAttachmentUpdateAccessSubject = "lfx.update_access.meeting_attachment"

	// MeetingAttachmentDeleteAccessSubject is the subject for the meeting attachment access control deletion.
	// The subject is of the form: lfx.delete_access.meeting_attachment
	MeetingAttachmentDeleteAccessSubject = "lfx.delete_access.meeting_attachment"

	// PastMeetingUpdateAccessSubject is the subject for the past meeting access control updates.
	// The subject is of the form: lfx.update_access.past_meeting
	PastMeetingUpdateAccessSubject = "lfx.update_access.past_meeting"

	// PastMeetingDeleteAllAccessSubject is the subject for the past meeting access control deletion.
	// The subject is of the form: lfx.delete_all_access.past_meeting
	PastMeetingDeleteAllAccessSubject = "lfx.delete_all_access.past_meeting"

	// PastMeetingParticipantPutSubject is the subject for adding past meeting participants.
	// The subject is of the form: lfx.put_participant.past_meeting
	PastMeetingParticipantPutSubject = "lfx.put_participant.past_meeting"

	// PastMeetingParticipantRemoveSubject is the subject for removing past meeting participants.
	// The subject is of the form: lfx.remove_participant.past_meeting
	PastMeetingParticipantRemoveSubject = "lfx.remove_participant.past_meeting"

	// CommitteeUpdateAccessSubject is the subject for the committee access control updates.
	// The subject is of the form: lfx.update_access.committee
	CommitteeUpdateAccessSubject = "lfx.update_access.committee"

	// CommitteeDeleteAllAccessSubject is the subject for the committee access control deletion.
	// The subject is of the form: lfx.delete_all_access.committee
	CommitteeDeleteAllAccessSubject = "lfx.delete_all_access.committee"

	// GroupsIOServiceUpdateAccessSubject is the subject for the groups.io service access control updates.
	// The subject is of the form: lfx.update_access.groupsio_service
	GroupsIOServiceUpdateAccessSubject = "lfx.update_access.groupsio_service"

	// GroupsIOServiceDeleteAllAccessSubject is the subject for the groups.io service access control deletion.
	// The subject is of the form: lfx.delete_all_access.groupsio_service
	GroupsIOServiceDeleteAllAccessSubject = "lfx.delete_all_access.groupsio_service"

	// GroupsIOMailingListUpdateAccessSubject is the subject for the groups.io mailing list access control updates.
	// The subject is of the form: lfx.update_access.groupsio_mailing_list
	GroupsIOMailingListUpdateAccessSubject = "lfx.update_access.groupsio_mailing_list"

	// GroupsIOMailingListDeleteAllAccessSubject is the subject for the groups.io mailing list access control deletion.
	// The subject is of the form: lfx.delete_all_access.groupsio_mailing_list
	GroupsIOMailingListDeleteAllAccessSubject = "lfx.delete_all_access.groupsio_mailing_list"

	// PastMeetingRecordingUpdateAccessSubject is the subject for the past meeting recording access control updates.
	// The subject is of the form: lfx.update_access.past_meeting_recording
	PastMeetingRecordingUpdateAccessSubject = "lfx.update_access.past_meeting_recording"

	// PastMeetingTranscriptUpdateAccessSubject is the subject for the past meeting transcript access control updates.
	// The subject is of the form: lfx.update_access.past_meeting_transcript
	PastMeetingTranscriptUpdateAccessSubject = "lfx.update_access.past_meeting_transcript"

	// PastMeetingSummaryUpdateAccessSubject is the subject for the past meeting summary access control updates.
	// The subject is of the form: lfx.update_access.past_meeting_summary
	PastMeetingSummaryUpdateAccessSubject = "lfx.update_access.past_meeting_summary"

	// PastMeetingAttachmentUpdateAccessSubject is the subject for the past meeting attachment access control updates.
	// The subject is of the form: lfx.update_access.past_meeting_attachment
	PastMeetingAttachmentUpdateAccessSubject = "lfx.update_access.past_meeting_attachment"

	// PastMeetingAttachmentDeleteAccessSubject is the subject for the past meeting attachment access control deletion.
	// The subject is of the form: lfx.delete_access.past_meeting_attachment
	PastMeetingAttachmentDeleteAccessSubject = "lfx.delete_access.past_meeting_attachment"

	// V1 meeting subjects for LFX v1 data sync (read-only)
	// V1MeetingUpdateAccessSubject is the subject for the v1 meeting access control updates.
	// The subject is of the form: lfx.update_access.v1_meeting
	V1MeetingUpdateAccessSubject = "lfx.update_access.v1_meeting"

	// V1MeetingDeleteAllAccessSubject is the subject for the v1 meeting access control deletion.
	// The subject is of the form: lfx.delete_all_access.v1_meeting
	V1MeetingDeleteAllAccessSubject = "lfx.delete_all_access.v1_meeting"

	// V1PastMeetingUpdateAccessSubject is the subject for the v1 past meeting access control updates.
	// The subject is of the form: lfx.update_access.v1_past_meeting
	V1PastMeetingUpdateAccessSubject = "lfx.update_access.v1_past_meeting"

	// V1PastMeetingDeleteAllAccessSubject is the subject for the v1 past meeting access control deletion.
	// The subject is of the form: lfx.delete_all_access.v1_past_meeting
	V1PastMeetingDeleteAllAccessSubject = "lfx.delete_all_access.v1_past_meeting"

	// V1PastMeetingRecordingUpdateAccessSubject is the subject for the v1 past meeting recording access control updates.
	// The subject is of the form: lfx.update_access.v1_past_meeting_recording
	V1PastMeetingRecordingUpdateAccessSubject = "lfx.update_access.v1_past_meeting_recording"

	// V1PastMeetingTranscriptUpdateAccessSubject is the subject for the v1 past meeting transcript access control updates.
	// The subject is of the form: lfx.update_access.v1_past_meeting_transcript
	V1PastMeetingTranscriptUpdateAccessSubject = "lfx.update_access.v1_past_meeting_transcript"

	// V1PastMeetingSummaryUpdateAccessSubject is the subject for the v1 past meeting summary access control updates.
	// The subject is of the form: lfx.update_access.v1_past_meeting_summary
	V1PastMeetingSummaryUpdateAccessSubject = "lfx.update_access.v1_past_meeting_summary"

	// V1MeetingRegistrantPutSubject is the subject for adding v1 meeting registrants.
	// The subject is of the form: lfx.put_registrant.v1_meeting
	V1MeetingRegistrantPutSubject = "lfx.put_registrant.v1_meeting"

	// V1MeetingRegistrantRemoveSubject is the subject for removing v1 meeting registrants.
	// The subject is of the form: lfx.remove_registrant.v1_meeting
	V1MeetingRegistrantRemoveSubject = "lfx.remove_registrant.v1_meeting"
)

// NATS queue subjects that the FGA sync service handles messages about.
const (
	// FgaSyncQueue is the subject name for the FGA sync.
	// The subject is of the form: lfx.fga-sync.queue
	FgaSyncQueue = "lfx.fga-sync.queue"
)
