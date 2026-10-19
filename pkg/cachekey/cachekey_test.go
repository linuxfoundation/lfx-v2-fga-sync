// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package cachekey

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInvalidation(t *testing.T) {
	tests := []struct {
		name     string
		object   string
		relation string
	}{
		{name: "simple pair", object: "project:1", relation: "viewer"},
		{name: "different relation same object", object: "project:1", relation: "writer"},
		{name: "v1 object type", object: "v1_meeting:79915658043", relation: "viewer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := Invalidation(tt.object, tt.relation)
			assert.True(t, strings.HasPrefix(key, "inv."))
			assert.NotContains(t, key, tt.object)
			assert.NotContains(t, key, tt.relation)
		})
	}

	assert.Equal(t, Invalidation("project:1", "viewer"), Invalidation("project:1", "viewer"))
	assert.NotEqual(t, Invalidation("project:1", "viewer"), Invalidation("project:1", "writer"))
	assert.NotEqual(t, Invalidation("project:1", "viewer"), Invalidation("project:2", "viewer"))
}
