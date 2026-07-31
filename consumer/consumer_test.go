/*
 * Copyright 2026 Maritime Connectivity Platform Consortium
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package consumer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/maritimeconnectivity/MMS/mmtp"
	"github.com/maritimeconnectivity/MMS/persistence"
	"github.com/maritimeconnectivity/MMS/utils/rw"
	"github.com/stretchr/testify/require"
)

func TestQueueMessageAddsMessageAndNotification(t *testing.T) {
	t.Parallel()

	consumer := newTestConsumer(t)
	message := newStoredMessage("message-1", time.Now().Add(time.Hour).Unix())

	err := consumer.QueueMessage(message)
	require.NoError(t, err)
	require.Equal(t, []string{message.GetUuid()}, fetchUUIDs(t, consumer))
	require.Equal(t, []string{message.GetUuid()}, notificationUUIDs(t, consumer))
}

func TestQueueMessageErrors(t *testing.T) {
	t.Parallel()

	t.Run("missing UUID", func(t *testing.T) {
		t.Parallel()

		consumer := newTestConsumer(t)
		err := consumer.QueueMessage(newStoredMessage("", time.Now().Add(time.Hour).Unix()))
		require.ErrorContains(t, err, "does not contain a UUID")
	})

	t.Run("nil consumer", func(t *testing.T) {
		t.Parallel()

		var consumer *Consumer
		err := consumer.QueueMessage(newStoredMessage("message-1", time.Now().Add(time.Hour).Unix()))
		require.ErrorContains(t, err, "consumer resolved to nil")
	})
}

func TestQueueMessageStoresEveryMessage(t *testing.T) {
	t.Parallel()

	consumer := newTestConsumer(t)
	messages := []*mmtp.MmtpMessage{
		newStoredMessage("message-1", time.Now().Add(time.Hour).Unix()),
		newStoredMessage("message-2", time.Now().Add(2*time.Hour).Unix()),
		newStoredMessage("message-3", time.Now().Add(3*time.Hour).Unix()),
	}

	for _, message := range messages {
		require.NoError(t, consumer.QueueMessage(message))
	}

	expected := make([]string, 0, len(messages))
	for _, message := range messages {
		expected = append(expected, message.GetUuid())
	}
	sort.Strings(expected)
	require.Equal(t, expected, fetchUUIDs(t, consumer))
}

func TestHandleFetchReturnsQueuedMetadata(t *testing.T) {
	t.Parallel()

	consumer := newTestConsumer(t)
	active := newStoredMessage("active-message", time.Now().Add(time.Hour).Unix())
	require.NoError(t, consumer.QueueMessage(active))

	requestMessage := newFetchRequest("fetch-request")
	response, err := runHandlerOverWebsocket(t, func(request *http.Request, conn *websocket.Conn) error {
		return consumer.HandleFetch(requestMessage, request, conn)
	})
	require.NoError(t, err)
	require.Equal(t, requestMessage.GetUuid(), response.GetResponseMessage().GetResponseToUuid())
	require.Equal(t, mmtp.ResponseEnum_GOOD, response.GetResponseMessage().GetResponse())
	require.Equal(t, []string{active.GetUuid()}, sortedMetadataUUIDs(response.GetResponseMessage().GetMessageMetadata()))
	require.Equal(t, []string{active.GetUuid()}, fetchUUIDs(t, consumer))
}

func TestHandleReceiveReturnsRequestedMessagesAndRemovesThem(t *testing.T) {
	t.Parallel()

	consumer := newTestConsumer(t)
	first := newStoredMessage("message-1", time.Now().Add(time.Hour).Unix())
	second := newStoredMessage("message-2", time.Now().Add(2*time.Hour).Unix())
	third := newStoredMessage("message-3", time.Now().Add(3*time.Hour).Unix())

	for _, message := range []*mmtp.MmtpMessage{first, second, third} {
		require.NoError(t, consumer.QueueMessage(message))
	}

	requestMessage := newReceiveRequest("receive-request", []string{first.GetUuid(), third.GetUuid()})
	response, err := runHandlerOverWebsocket(t, func(request *http.Request, conn *websocket.Conn) error {
		return consumer.HandleReceive(requestMessage, request, conn)
	})
	require.NoError(t, err)
	require.Equal(t, requestMessage.GetUuid(), response.GetResponseMessage().GetResponseToUuid())
	require.ElementsMatch(t, []string{first.GetUuid(), third.GetUuid()}, sortedContentUUIDs(response.GetResponseMessage().GetMessageContent()))
	require.Equal(t, []string{second.GetUuid()}, fetchUUIDs(t, consumer))
	require.Equal(t, []string{second.GetUuid()}, notificationUUIDs(t, consumer))
}

func TestHandleReceiveRequeuesMessagesWhenWriteFails(t *testing.T) {
	t.Parallel()

	consumer := newTestConsumer(t)
	first := newStoredMessage("message-1", time.Now().Add(time.Hour).Unix())
	second := newStoredMessage("message-2", time.Now().Add(2*time.Hour).Unix())

	for _, message := range []*mmtp.MmtpMessage{first, second} {
		require.NoError(t, consumer.QueueMessage(message))
	}

	request := httptest.NewRequest(http.MethodGet, "http://example.invalid/receive", nil)
	requestMessage := newReceiveRequest("receive-request", []string{first.GetUuid(), second.GetUuid()})

	err := consumer.HandleReceive(requestMessage, request, nil)
	require.ErrorContains(t, err, "could not send messages to Consumer")
	require.Equal(t, []string{first.GetUuid(), second.GetUuid()}, fetchUUIDs(t, consumer))
}

func TestHandleDisconnectSendsResponseAndClosesConnection(t *testing.T) {
	t.Parallel()

	requestMessage := newDisconnectRequest("disconnect-request")
	response, err := runHandlerOverWebsocket(t, func(request *http.Request, conn *websocket.Conn) error {
		return newTestConsumer(t).HandleDisconnect(requestMessage, request, conn)
	})
	require.NoError(t, err)
	require.Equal(t, requestMessage.GetUuid(), response.GetResponseMessage().GetResponseToUuid())
	require.Equal(t, mmtp.ResponseEnum_GOOD, response.GetResponseMessage().GetResponse())
	require.Equal(t, mmtp.MsgType_RESPONSE_MESSAGE, response.GetMsgType())
}

func newTestConsumer(t *testing.T) *Consumer {
	t.Helper()

	store := persistence.NewMemoryStore()
	id := uuid.NewString()
	session := persistence.Session{
		ID:        id,
		Kind:      persistence.SessionKindAgent,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Hour),
	}
	require.NoError(t, store.UpsertSession(context.Background(), session, "test-token"))

	return &Consumer{
		ID:       id,
		Store:    store,
		MsgMu:    &sync.RWMutex{},
		NotifyMu: &sync.RWMutex{},
	}
}

// fetchUUIDs returns the UUIDs of every message currently queued for the consumer, sorted for
// deterministic comparison.
func fetchUUIDs(t *testing.T, consumer *Consumer) []string {
	t.Helper()
	messages, err := consumer.Store.FetchMessages(context.Background(), consumer.ID, nil)
	require.NoError(t, err)
	uuids := make([]string, 0, len(messages))
	for _, message := range messages {
		uuids = append(uuids, message.GetUuid())
	}
	sort.Strings(uuids)
	return uuids
}

// notificationUUIDs returns the UUIDs of every message still pending a NOTIFY for the consumer,
// sorted for deterministic comparison.
func notificationUUIDs(t *testing.T, consumer *Consumer) []string {
	t.Helper()
	messages, err := consumer.Store.PendingNotifications(context.Background(), consumer.ID)
	require.NoError(t, err)
	uuids := make([]string, 0, len(messages))
	for _, message := range messages {
		uuids = append(uuids, message.GetUuid())
	}
	sort.Strings(uuids)
	return uuids
}

func newStoredMessage(uuid string, expires int64) *mmtp.MmtpMessage {
	return &mmtp.MmtpMessage{
		MsgType: mmtp.MsgType_PROTOCOL_MESSAGE,
		Uuid:    uuid,
		Body: &mmtp.MmtpMessage_ProtocolMessage{
			ProtocolMessage: &mmtp.ProtocolMessage{
				ProtocolMsgType: mmtp.ProtocolMessageType_SEND_MESSAGE,
				Body: &mmtp.ProtocolMessage_SendMessage{
					SendMessage: &mmtp.Send{
						ApplicationMessage: &mmtp.ApplicationMessage{
							Header: &mmtp.ApplicationMessageHeader{
								SubjectOrRecipient: &mmtp.ApplicationMessageHeader_Subject{Subject: "nav.warn"},
								Expires:            expires,
								Sender:             "urn:mrn:mcp:device:test:sender",
								BodySizeNumBytes:   4,
							},
							Body: []byte("body"),
						},
					},
				},
			},
		},
	}
}

func newFetchRequest(uuid string) *mmtp.MmtpMessage {
	return &mmtp.MmtpMessage{
		MsgType: mmtp.MsgType_PROTOCOL_MESSAGE,
		Uuid:    uuid,
		Body: &mmtp.MmtpMessage_ProtocolMessage{
			ProtocolMessage: &mmtp.ProtocolMessage{
				ProtocolMsgType: mmtp.ProtocolMessageType_FETCH_MESSAGE,
				Body:            &mmtp.ProtocolMessage_FetchMessage{FetchMessage: &mmtp.Fetch{}},
			},
		},
	}
}

func newReceiveRequest(uuid string, messageUUIDs []string) *mmtp.MmtpMessage {
	return &mmtp.MmtpMessage{
		MsgType: mmtp.MsgType_PROTOCOL_MESSAGE,
		Uuid:    uuid,
		Body: &mmtp.MmtpMessage_ProtocolMessage{
			ProtocolMessage: &mmtp.ProtocolMessage{
				ProtocolMsgType: mmtp.ProtocolMessageType_RECEIVE_MESSAGE,
				Body:            &mmtp.ProtocolMessage_ReceiveMessage{ReceiveMessage: &mmtp.Receive{Filter: &mmtp.Filter{MessageUuids: messageUUIDs}}},
			},
		},
	}
}

func newDisconnectRequest(uuid string) *mmtp.MmtpMessage {
	return &mmtp.MmtpMessage{
		MsgType: mmtp.MsgType_PROTOCOL_MESSAGE,
		Uuid:    uuid,
		Body: &mmtp.MmtpMessage_ProtocolMessage{
			ProtocolMessage: &mmtp.ProtocolMessage{
				ProtocolMsgType: mmtp.ProtocolMessageType_DISCONNECT_MESSAGE,
				Body:            &mmtp.ProtocolMessage_DisconnectMessage{DisconnectMessage: &mmtp.Disconnect{}},
			},
		},
	}
}

func runHandlerOverWebsocket(t *testing.T, handler func(*http.Request, *websocket.Conn) error) (*mmtp.MmtpMessage, error) {
	t.Helper()

	errCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		conn, err := websocket.Accept(writer, request, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			errCh <- err
			return
		}

		errCh <- handler(request, conn)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(t, err)

	response, _, err := rw.ReadMessage(ctx, conn)
	require.NoError(t, err)

	_ = conn.Close(websocket.StatusNormalClosure, "test complete")

	return response, <-errCh
}

func sortedMetadataUUIDs(metadata []*mmtp.MessageMetadata) []string {
	uuids := make([]string, 0, len(metadata))
	for _, entry := range metadata {
		uuids = append(uuids, entry.GetUuid())
	}
	sort.Strings(uuids)
	return uuids
}

func sortedContentUUIDs(content []*mmtp.MessageContent) []string {
	uuids := make([]string, 0, len(content))
	for _, entry := range content {
		uuids = append(uuids, entry.GetUuid())
	}
	sort.Strings(uuids)
	return uuids
}
