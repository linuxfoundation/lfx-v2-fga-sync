// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	openfga "github.com/openfga/go-sdk"
	"github.com/stretchr/testify/mock"

	. "github.com/openfga/go-sdk/client"
)

// MockFgaClient is a mock implementation of the IFgaClient interface
type MockFgaClient struct {
	mock.Mock
}

// BatchCheck implements the IFgaClient interface
func (m *MockFgaClient) Read(
	ctx context.Context,
	req ClientReadRequest,
	options ClientReadOptions,
) (*ClientReadResponse, error) {
	args := m.Called(ctx, req, options)
	//nolint:errcheck // the error is passed through to the caller
	return args.Get(0).(*ClientReadResponse), args.Error(1)
}

// Write implements the IFgaClient interface
func (m *MockFgaClient) Write(
	ctx context.Context,
	req ClientWriteRequest,
) (*ClientWriteResponse, error) {
	args := m.Called(ctx, req)
	//nolint:errcheck // the error is passed through to the caller
	return args.Get(0).(*ClientWriteResponse), args.Error(1)
}

// BatchCheck implements the IFgaClient interface
func (m *MockFgaClient) BatchCheck(
	ctx context.Context,
	request ClientBatchCheckRequest,
) (*openfga.BatchCheckResponse, error) {
	args := m.Called(ctx, request)
	//nolint:errcheck // the error is passed through to the caller
	return args.Get(0).(*openfga.BatchCheckResponse), args.Error(1)
}

// ListObjects implements the IFgaClient interface
func (m *MockFgaClient) ListObjects(
	ctx context.Context,
	body ClientListObjectsRequest,
	options ClientListObjectsOptions,
) (*ClientListObjectsResponse, error) {
	args := m.Called(ctx, body, options)
	//nolint:errcheck // the error is passed through to the caller
	return args.Get(0).(*ClientListObjectsResponse), args.Error(1)
}

// MockNatsMsg is a mock implementation of the INatsMsg interface
type MockNatsMsg struct {
	mock.Mock
	reply   string
	data    []byte
	subject string
}

// Reply implements the INatsMsg interface
func (m *MockNatsMsg) Reply() string {
	return m.reply
}

// Respond implements the INatsMsg interface
func (m *MockNatsMsg) Respond(data []byte) error {
	args := m.Called(data)
	return args.Error(0)
}

// Data implements the INatsMsg interface
func (m *MockNatsMsg) Data() []byte {
	return m.data
}

// Subject implements the INatsMsg interface
func (m *MockNatsMsg) Subject() string {
	return m.subject
}

// CreateMockNatsMsg creates a mock NATS message that can be used in tests
func CreateMockNatsMsg(data []byte) *MockNatsMsg {
	msg := MockNatsMsg{
		data: data,
	}
	return &msg
}

// MockKeyValue is a mock implementation of jetstream.KeyValue for testing
type MockKeyValue struct {
	mock.Mock
	mu           sync.Mutex
	data         map[string][]byte
	createdTimes map[string]time.Time
	returnError  error
	notFoundKeys map[string]bool
}

// NewMockKeyValue creates a new MockKeyValue instance
func NewMockKeyValue() *MockKeyValue {
	return &MockKeyValue{
		data:         make(map[string][]byte),
		createdTimes: make(map[string]time.Time),
		notFoundKeys: make(map[string]bool),
	}
}

// Get implements the jetstream.KeyValue interface
func (m *MockKeyValue) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.returnError != nil {
		return nil, m.returnError
	}
	if m.notFoundKeys[key] {
		return nil, jetstream.ErrKeyNotFound
	}
	if data, exists := m.data[key]; exists {
		return &MockKeyValueEntry{
			key:     key,
			value:   data,
			created: m.createdTimes[key],
		}, nil
	}
	return nil, jetstream.ErrKeyNotFound
}

// Put implements the jetstream.KeyValue interface
func (m *MockKeyValue) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.returnError != nil {
		return 0, m.returnError
	}
	m.data[key] = value
	m.createdTimes[key] = time.Now()
	return 1, nil
}

// PutString implements the jetstream.KeyValue interface
func (m *MockKeyValue) PutString(ctx context.Context, key, value string) (uint64, error) {
	return m.Put(ctx, key, []byte(value))
}

// SetNotFound implements the jetstream.KeyValue interface
func (m *MockKeyValue) SetNotFound(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notFoundKeys[key] = true
}

// SetError implements the jetstream.KeyValue interface
func (m *MockKeyValue) SetError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.returnError = err
}

// MockKeyValueEntry is a mock implementation of jetstream.KeyValueEntry
type MockKeyValueEntry struct {
	key      string
	value    []byte
	created  time.Time
	revision uint64
}

func (m *MockKeyValueEntry) Bucket() string                  { return "test-bucket" }
func (m *MockKeyValueEntry) Key() string                     { return m.key }
func (m *MockKeyValueEntry) Value() []byte                   { return m.value }
func (m *MockKeyValueEntry) Created() time.Time              { return m.created }
func (m *MockKeyValueEntry) Revision() uint64                { return m.revision }
func (m *MockKeyValueEntry) Delta() uint64                   { return 0 }
func (m *MockKeyValueEntry) Operation() jetstream.KeyValueOp { return jetstream.KeyValuePut }
