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
	"github.com/maritimeconnectivity/MMS/mmtp"
	"github.com/maritimeconnectivity/MMS/utils/rw"
	"github.com/stretchr/testify/require"
)

func TestQueueMessageAddsMessageAndNotification(t *testing.T) {
	t.Parallel()

	consumer := newTestConsumer()
	message := newStoredMessage("message-1", time.Now().Add(time.Hour).Unix())

	err := consumer.QueueMessage(message)
	require.NoError(t, err)
	require.Same(t, message, consumer.Messages[message.GetUuid()])
	require.Same(t, message, consumer.Notifications[message.GetUuid()])
}

func TestQueueMessageErrors(t *testing.T) {
	t.Parallel()

	t.Run("missing UUID", func(t *testing.T) {
		t.Parallel()

		consumer := newTestConsumer()
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

func TestBulkQueueMessagesStoresEveryMessage(t *testing.T) {
	t.Parallel()

	consumer := newTestConsumer()
	messages := []*mmtp.MmtpMessage{
		newStoredMessage("message-1", time.Now().Add(time.Hour).Unix()),
		newStoredMessage("message-2", time.Now().Add(2*time.Hour).Unix()),
		newStoredMessage("message-3", time.Now().Add(3*time.Hour).Unix()),
	}

	consumer.BulkQueueMessages(messages)

	require.Len(t, consumer.Messages, len(messages))

	for _, message := range messages {
		require.Same(t, message, consumer.Messages[message.GetUuid()])
	}
}

func TestHandleFetchReturnsUnexpiredMetadataAndDeletesExpiredMessages(t *testing.T) {
	t.Parallel()

	consumer := newTestConsumer()
	expired := newStoredMessage("expired-message", time.Now().Add(-time.Hour).Unix())
	active := newStoredMessage("active-message", time.Now().Add(time.Hour).Unix())
	consumer.Messages[expired.GetUuid()] = expired
	consumer.Messages[active.GetUuid()] = active

	requestMessage := newFetchRequest("fetch-request")
	response, err := runHandlerOverWebsocket(t, func(request *http.Request, conn *websocket.Conn) error {
		return consumer.HandleFetch(requestMessage, request, conn)
	})
	require.NoError(t, err)
	require.Equal(t, requestMessage.GetUuid(), response.GetResponseMessage().GetResponseToUuid())
	require.Equal(t, mmtp.ResponseEnum_GOOD, response.GetResponseMessage().GetResponse())
	require.Equal(t, []string{active.GetUuid()}, sortedMetadataUUIDs(response.GetResponseMessage().GetMessageMetadata()))
	require.NotContains(t, consumer.Messages, expired.GetUuid())
	require.Same(t, active, consumer.Messages[active.GetUuid()])
}

func TestHandleReceiveReturnsRequestedMessagesAndRemovesThem(t *testing.T) {
	t.Parallel()

	consumer := newTestConsumer()
	first := newStoredMessage("message-1", time.Now().Add(time.Hour).Unix())
	second := newStoredMessage("message-2", time.Now().Add(2*time.Hour).Unix())
	third := newStoredMessage("message-3", time.Now().Add(3*time.Hour).Unix())

	for _, message := range []*mmtp.MmtpMessage{first, second, third} {
		consumer.Messages[message.GetUuid()] = message
		consumer.Notifications[message.GetUuid()] = message
	}

	requestMessage := newReceiveRequest("receive-request", []string{first.GetUuid(), third.GetUuid()})
	response, err := runHandlerOverWebsocket(t, func(request *http.Request, conn *websocket.Conn) error {
		return consumer.HandleReceive(requestMessage, request, conn)
	})
	require.NoError(t, err)
	require.Equal(t, requestMessage.GetUuid(), response.GetResponseMessage().GetResponseToUuid())
	require.ElementsMatch(t, []string{first.GetUuid(), third.GetUuid()}, sortedContentUUIDs(response.GetResponseMessage().GetMessageContent()))
	require.NotContains(t, consumer.Messages, first.GetUuid())
	require.NotContains(t, consumer.Messages, third.GetUuid())
	require.Same(t, second, consumer.Messages[second.GetUuid()])
	require.NotContains(t, consumer.Notifications, first.GetUuid())
	require.NotContains(t, consumer.Notifications, third.GetUuid())
	require.Same(t, second, consumer.Notifications[second.GetUuid()])
}

func TestHandleReceiveRequeuesMessagesWhenWriteFails(t *testing.T) {
	t.Parallel()

	consumer := newTestConsumer()
	first := newStoredMessage("message-1", time.Now().Add(time.Hour).Unix())
	second := newStoredMessage("message-2", time.Now().Add(2*time.Hour).Unix())

	for _, message := range []*mmtp.MmtpMessage{first, second} {
		consumer.Messages[message.GetUuid()] = message
		consumer.Notifications[message.GetUuid()] = message
	}

	request := httptest.NewRequest(http.MethodGet, "http://example.invalid/receive", nil)
	requestMessage := newReceiveRequest("receive-request", []string{first.GetUuid(), second.GetUuid()})

	err := consumer.HandleReceive(requestMessage, request, nil)
	require.ErrorContains(t, err, "could not send messages to Consumer")
	require.Same(t, first, consumer.Messages[first.GetUuid()])
	require.Same(t, second, consumer.Messages[second.GetUuid()])
}

func TestHandleDisconnectSendsResponseAndClosesConnection(t *testing.T) {
	t.Parallel()

	requestMessage := newDisconnectRequest("disconnect-request")
	response, err := runHandlerOverWebsocket(t, func(request *http.Request, conn *websocket.Conn) error {
		return newTestConsumer().HandleDisconnect(requestMessage, request, conn)
	})
	require.NoError(t, err)
	require.Equal(t, requestMessage.GetUuid(), response.GetResponseMessage().GetResponseToUuid())
	require.Equal(t, mmtp.ResponseEnum_GOOD, response.GetResponseMessage().GetResponse())
	require.Equal(t, mmtp.MsgType_RESPONSE_MESSAGE, response.GetMsgType())
}

func newTestConsumer() *Consumer {
	return &Consumer{
		Messages:      make(map[string]*mmtp.MmtpMessage),
		MsgMu:         &sync.RWMutex{},
		Notifications: make(map[string]*mmtp.MmtpMessage),
		NotifyMu:      &sync.RWMutex{},
	}
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
