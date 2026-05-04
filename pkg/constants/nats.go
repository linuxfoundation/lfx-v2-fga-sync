// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package constants defines shared NATS and OpenFGA constants for the fga-sync service.
package constants

// NATS Key-Value store bucket names.
const (
	// KVBucketNameSyncCache is the name of the KV bucket for the FGA sync cache.
	KVBucketNameSyncCache = "fga-sync-cache"
)

// NATS subjects that the FGA sync service handles messages about.
const (
	// AccessCheckSubject is the subject for the access check request.
	// The subject is of the form: lfx.access_check.request
	AccessCheckSubject = "lfx.access_check.request"

	// ReadTuplesSubject is the subject for reading a user's direct tuples by object type.
	// The subject is of the form: lfx.access_check.read_tuples
	ReadTuplesSubject = "lfx.access_check.read_tuples"
)

// NATS queue subjects that the FGA sync service handles messages about.
const (
	// FgaSyncQueue is the subject name for the FGA sync.
	// The subject is of the form: lfx.fga-sync.queue
	FgaSyncQueue = "lfx.fga-sync.queue"
)

// Generic NATS subjects for resource-agnostic FGA operations.
// These subjects accept a GenericFGAMessage envelope and route based on object_type.
const (
	// GenericUpdateAccessSubject is the subject for generic access control updates.
	// The subject is of the form: lfx.fga-sync.update_access
	GenericUpdateAccessSubject = "lfx.fga-sync.update_access"

	// GenericDeleteAccessSubject is the subject for generic access control deletions.
	// The subject is of the form: lfx.fga-sync.delete_access
	GenericDeleteAccessSubject = "lfx.fga-sync.delete_access"

	// GenericMemberPutSubject is the subject for generic member add operations.
	// The subject is of the form: lfx.fga-sync.member_put
	GenericMemberPutSubject = "lfx.fga-sync.member_put"

	// GenericMemberRemoveSubject is the subject for generic member remove operations.
	// The subject is of the form: lfx.fga-sync.member_remove
	GenericMemberRemoveSubject = "lfx.fga-sync.member_remove"
)
