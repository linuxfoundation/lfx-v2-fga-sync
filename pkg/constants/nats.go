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
)

// NATS queue subjects that the FGA sync service handles messages about.
const (
	// FgaSyncQueue is the subject name for the FGA sync.
	// The subject is of the form: lfx.fga-sync.queue
	FgaSyncQueue = "lfx.fga-sync.queue"
)
