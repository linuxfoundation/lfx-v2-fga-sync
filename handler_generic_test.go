// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"net"
	"syscall"
	"testing"

	"github.com/openfga/go-sdk/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestGenericAccessHandlersMarkLocalValidationErrorsTerminal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		handler  func(*HandlerService, context.Context, INatsMsg) error
		payload  string
		redacted string
	}{
		{
			name:    "update malformed JSON",
			handler: (*HandlerService).genericUpdateAccessHandler,
			payload: `{`,
		},
		{
			name:    "update missing object type",
			handler: (*HandlerService).genericUpdateAccessHandler,
			payload: `{"operation":"update_access","data":{"uid":"resource-1"}}`,
		},
		{
			name:    "update wrong operation",
			handler: (*HandlerService).genericUpdateAccessHandler,
			payload: `{"object_type":"committee","operation":"delete_access","data":{"uid":"resource-1"}}`,
		},
		{
			name:    "update missing UID",
			handler: (*HandlerService).genericUpdateAccessHandler,
			payload: `{"object_type":"committee","operation":"update_access","data":{}}`,
		},
		{
			name:     "update invalid reference format",
			handler:  (*HandlerService).genericUpdateAccessHandler,
			payload:  `{"object_type":"committee","operation":"update_access","data":{"uid":"resource-1","references":{"project":[":invalid"]}}}`,
			redacted: ":invalid",
		},
		{
			name:    "delete malformed JSON",
			handler: (*HandlerService).genericDeleteAccessHandler,
			payload: `{`,
		},
		{
			name:    "delete missing object type",
			handler: (*HandlerService).genericDeleteAccessHandler,
			payload: `{"operation":"delete_access","data":{"uid":"resource-1"}}`,
		},
		{
			name:    "delete wrong operation",
			handler: (*HandlerService).genericDeleteAccessHandler,
			payload: `{"object_type":"committee","operation":"update_access","data":{"uid":"resource-1"}}`,
		},
		{
			name:    "delete missing UID",
			handler: (*HandlerService).genericDeleteAccessHandler,
			payload: `{"object_type":"committee","operation":"delete_access","data":{}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			service := setupService()
			err := tt.handler(service, context.Background(), CreateMockNatsMsg([]byte(tt.payload)))

			require.Error(t, err)
			assert.True(t, isTerminalValidationError(err), "expected terminal validation error: %v", err)
			if tt.redacted != "" {
				assert.NotContains(t, err.Error(), tt.redacted)
			}
		})
	}
}

func TestGenericAccessHandlersLeaveFgaErrorsTransient(t *testing.T) {
	t.Parallel()

	handlers := []struct {
		name    string
		handler func(*HandlerService, context.Context, INatsMsg) error
		payload string
	}{
		{
			name:    "update",
			handler: (*HandlerService).genericUpdateAccessHandler,
			payload: `{"object_type":"committee","operation":"update_access","data":{"uid":"resource-1"}}`,
		},
		{
			name:    "delete",
			handler: (*HandlerService).genericDeleteAccessHandler,
			payload: `{"object_type":"committee","operation":"delete_access","data":{"uid":"resource-1"}}`,
		},
	}
	fgaErrors := []struct {
		name string
		err  error
	}{
		{name: "unknown SDK error", err: assert.AnError},
		{
			name: "connection refused",
			err:  &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
		},
		{name: "context deadline", err: context.DeadlineExceeded},
		{name: "validation error", err: makeValidationError("request validation failed")},
		{name: "HTTP 400", err: fakeStatusErr{code: 400}},
		{name: "HTTP 401", err: fakeStatusErr{code: 401}},
		{name: "HTTP 403", err: fakeStatusErr{code: 403}},
		{name: "HTTP 408", err: fakeStatusErr{code: 408}},
		{name: "HTTP 409", err: fakeStatusErr{code: 409}},
		{name: "HTTP 429", err: fakeStatusErr{code: 429}},
		{name: "HTTP 500", err: fakeStatusErr{code: 500}},
	}

	for _, handler := range handlers {
		for _, fgaErr := range fgaErrors {
			t.Run(handler.name+"/"+fgaErr.name, func(t *testing.T) {
				t.Parallel()

				service := setupService()
				service.fgaService.client.(*MockFgaClient).
					On("Read", mock.Anything, mock.Anything, client.ClientReadOptions{}).
					Return((*client.ClientReadResponse)(nil), fgaErr.err)

				err := handler.handler(
					service,
					context.Background(),
					CreateMockNatsMsg([]byte(handler.payload)),
				)

				require.Error(t, err)
				assert.False(t, isTerminalValidationError(err))
			})
		}
	}
}
