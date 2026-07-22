// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package types contains shared message types for the fga-sync service.
package types

// creatableTypesByRelation maps object_type -> relation -> the artifact types
// creatable on a target holding that relation. This mirrors the client's
// writerGuard rules; it must never grant a type/target combination writerGuard
// would deny. The exact table is pending review with the writerGuard/client
// owner (see LFXV2-2753).
var creatableTypesByRelation = map[string]map[string][]string{
	"project": {
		"writer":              {"project", "committee", "meeting", "mailing_list", "survey", "vote"},
		"meeting_coordinator": {"meeting"},
	},
	"committee": {
		"writer": {"meeting", "survey", "vote"},
	},
}

// CreatableTypesFor returns the artifact types creatable on a target of the
// given object type where the user holds the given relation. Returns an empty
// (non-nil) slice when no mapping is defined for the pair.
func CreatableTypesFor(objectType, relation string) []string {
	if byRelation, ok := creatableTypesByRelation[objectType]; ok {
		if creatableTypes, ok := byRelation[relation]; ok {
			return creatableTypes
		}
	}
	return []string{}
}
