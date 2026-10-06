// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package main provides the fga-sync service entry point and supporting types.
package main

import (
	nats "github.com/nats-io/nats.go"
)

// INatsMsg is an interface for [nats.Msg] that allows for mocking.
type INatsMsg interface {
	Reply() string
	Respond(data []byte) error
	Data() []byte
	Subject() string
	Header() nats.Header
}

// NatsMsg is a wrapper around [nats.Msg] that implements [INatsMsg].
type NatsMsg struct {
	*nats.Msg
}

// Reply implements [INatsMsg.Reply].
func (m *NatsMsg) Reply() string {
	return m.Msg.Reply
}

// Respond implements [INatsMsg.Respond].
func (m *NatsMsg) Respond(data []byte) error {
	return m.Msg.Respond(data)
}

// Data implements [INatsMsg.Data].
func (m *NatsMsg) Data() []byte {
	return m.Msg.Data
}

// Subject implements [INatsMsg.Subject].
func (m *NatsMsg) Subject() string {
	return m.Msg.Subject
}

// Header implements [INatsMsg.Header].
func (m *NatsMsg) Header() nats.Header {
	return m.Msg.Header
}

// jetStreamMessage is the subset of [jetstream.Msg] needed to adapt a JetStream
// message to [INatsMsg]. It is satisfied by [jetstream.Msg] and by test doubles.
type jetStreamMessage interface {
	Data() []byte
	Headers() nats.Header
	Subject() string
}

// jetStreamNatsMsg adapts a JetStream message to [INatsMsg]. Unlike [NatsMsg], it
// hides the ACK reply subject: JetStream acknowledgment is handled explicitly by
// the access mutation consumer, not via a reply-subject publish, so Reply always
// returns empty and Respond is a no-op.
type jetStreamNatsMsg struct {
	message jetStreamMessage
}

var _ INatsMsg = (*jetStreamNatsMsg)(nil)

// newJetStreamNatsMsg wraps a JetStream message as an [INatsMsg].
func newJetStreamNatsMsg(message jetStreamMessage) INatsMsg {
	return &jetStreamNatsMsg{message: message}
}

// Data implements [INatsMsg.Data].
func (m *jetStreamNatsMsg) Data() []byte {
	return m.message.Data()
}

// Header implements [INatsMsg.Header].
func (m *jetStreamNatsMsg) Header() nats.Header {
	return m.message.Headers()
}

// Reply implements [INatsMsg.Reply]. JetStream access mutations send no reply.
func (m *jetStreamNatsMsg) Reply() string {
	return ""
}

// Respond implements [INatsMsg.Respond] as a no-op; acknowledgment happens via
// Ack/Term on the underlying JetStream message, not a reply-subject publish.
func (m *jetStreamNatsMsg) Respond(_ []byte) error {
	return nil
}

// Subject implements [INatsMsg.Subject].
func (m *jetStreamNatsMsg) Subject() string {
	return m.message.Subject()
}
