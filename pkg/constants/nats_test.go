// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package constants

import "testing"

func TestFgaSyncJetStreamNames(t *testing.T) {
	t.Parallel()

	if FgaSyncStreamName != "fga-sync-events" {
		t.Fatalf("unexpected stream name %q", FgaSyncStreamName)
	}
	if FgaSyncAccessMutationConsumerName != "fga-sync-access-mutation-consumer" {
		t.Fatalf("unexpected consumer name %q", FgaSyncAccessMutationConsumerName)
	}
}

func TestAccessMutationSubjectsRemainStable(t *testing.T) {
	t.Parallel()

	if GenericUpdateAccessSubject != "lfx.fga-sync.update_access" {
		t.Fatalf("unexpected update subject %q", GenericUpdateAccessSubject)
	}
	if GenericDeleteAccessSubject != "lfx.fga-sync.delete_access" {
		t.Fatalf("unexpected delete subject %q", GenericDeleteAccessSubject)
	}
}
