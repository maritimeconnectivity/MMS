/*
 * Copyright 2024 Maritime Connectivity Platform Consortium
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
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/maritimeconnectivity/MMS/mmtp"
	"github.com/maritimeconnectivity/MMS/persistence"
	"github.com/maritimeconnectivity/MMS/utils/errMsg"
	"github.com/maritimeconnectivity/MMS/utils/rw"
)

// queueMessageTimeout bounds how long a single QueueMessage write may block.
// It must not be raised without also considering that persistence.Open caps
// SQLiteStore to a single connection, so a stalled write blocks every other
// Store call in the process until this deadline is hit.
const queueMessageTimeout = 10 * time.Second

type Consumer struct {
	ID             string            // stable identifier used by the state store
	Store          persistence.Store // message and session state backend
	Mrn            string            // the MRN of the Consumer
	Interests      []string          // the Interests that the Consumer wants to subscribe to
	MsgMu          *sync.RWMutex     // serializes fetch and receive operations
	ReconnectToken string            // token for reconnecting to a previous session
	NotifyMu       *sync.RWMutex     // serializes notification operations
}

func (c *Consumer) QueueMessage(mmtpMessage *mmtp.MmtpMessage) error {
	if c == nil {
		return fmt.Errorf("consumer resolved to nil while trying to queue message")
	}
	if mmtpMessage.GetUuid() == "" {
		return fmt.Errorf("the message does not contain a UUID")
	}
	if c.ID == "" {
		return fmt.Errorf("consumer does not have a session ID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), queueMessageTimeout)
	defer cancel()
	return c.Store.QueueMessage(ctx, []string{c.ID}, mmtpMessage)
}

func (c *Consumer) notify(ctx context.Context, conn *websocket.Conn) error {
	messages, err := c.Store.PendingNotifications(ctx, c.ID)
	if err != nil {
		return fmt.Errorf("load pending notifications: %w", err)
	}
	if len(messages) == 0 {
		return nil
	}
	notifications := make([]*mmtp.MessageMetadata, 0, len(messages))
	uuids := make([]string, 0, len(messages))
	for _, message := range messages {
		notifications = append(notifications, &mmtp.MessageMetadata{
			Uuid:   message.GetUuid(),
			Header: message.GetProtocolMessage().GetSendMessage().GetApplicationMessage().GetHeader(),
		})
		uuids = append(uuids, message.GetUuid())
	}
	notifyMessage := newNotifyMessage(notifications)
	if err = rw.WriteMessage(ctx, conn, notifyMessage); err != nil {
		return fmt.Errorf("could not send Notify to Consumer: %w", err)
	}
	if err = c.Store.MarkNotified(ctx, c.ID, uuids); err != nil {
		return fmt.Errorf("mark notifications sent: %w", err)
	}
	return nil
}

func newNotifyMessage(notifications []*mmtp.MessageMetadata) *mmtp.MmtpMessage {
	return &mmtp.MmtpMessage{
		MsgType: mmtp.MsgType_PROTOCOL_MESSAGE,
		Uuid:    uuid.NewString(),
		Body: &mmtp.MmtpMessage_ProtocolMessage{
			ProtocolMessage: &mmtp.ProtocolMessage{
				ProtocolMsgType: mmtp.ProtocolMessageType_NOTIFY_MESSAGE,
				Body: &mmtp.ProtocolMessage_NotifyMessage{
					NotifyMessage: &mmtp.Notify{
						MessageMetadata: notifications,
					},
				},
			},
		},
	}
}

// CheckNewMessages Checks if there are messages the Agent has not been notified about and notifies about these
func (c *Consumer) CheckNewMessages(ctx context.Context, conn *websocket.Conn, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
			c.NotifyMu.Lock()
			if err := c.notify(ctx, conn); err != nil {
				log.Println("Failed Notifying Agent:", err)
			}
			c.NotifyMu.Unlock()
			continue
		}
	}
}

// HandleReceive handles request from consumer to receive messages, i.e. lookups buffered messages for the consumer and
// sends these messages to that consumer
func (c *Consumer) HandleReceive(mmtpMessage *mmtp.MmtpMessage, request *http.Request, conn *websocket.Conn) error {
	receive := mmtpMessage.GetProtocolMessage().GetReceiveMessage()
	if receive == nil {
		return nil
	}
	c.MsgMu.Lock()
	defer c.MsgMu.Unlock()
	c.NotifyMu.Lock()
	defer c.NotifyMu.Unlock()

	var requested []string
	if receive.GetFilter() != nil {
		requested = receive.GetFilter().GetMessageUuids()
	}
	messages, err := c.Store.FetchMessages(request.Context(), c.ID, requested)
	if err != nil {
		return fmt.Errorf("load messages: %w", err)
	}
	contents := make([]*mmtp.MessageContent, 0, len(messages))
	delivered := make([]string, 0, len(messages))
	for _, message := range messages {
		appMessage := message.GetProtocolMessage().GetSendMessage().GetApplicationMessage()
		if appMessage == nil {
			continue
		}
		contents = append(contents, &mmtp.MessageContent{Uuid: message.GetUuid(), Msg: appMessage})
		delivered = append(delivered, message.GetUuid())
	}
	response := &mmtp.MmtpMessage{
		MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
		Uuid:    uuid.NewString(),
		Body: &mmtp.MmtpMessage_ResponseMessage{ResponseMessage: &mmtp.ResponseMessage{
			ResponseToUuid: mmtpMessage.GetUuid(),
			Response:       mmtp.ResponseEnum_GOOD,
			MessageContent: contents,
		}},
	}
	if err = rw.WriteMessage(request.Context(), conn, response); err != nil {
		return fmt.Errorf("could not send messages to Consumer: %w", err)
	}
	if err = c.Store.DeleteDeliveries(request.Context(), c.ID, delivered); err != nil {
		return fmt.Errorf("delete delivered messages: %w", err)
	}
	return nil
}

// HandleDisconnect handles a request from a consumer to disconnect, by responding to the consumer and closing the socket
func (c *Consumer) HandleDisconnect(mmtpMessage *mmtp.MmtpMessage, request *http.Request, conn *websocket.Conn) error {
	if disconnect := mmtpMessage.GetProtocolMessage().GetDisconnectMessage(); disconnect != nil {
		resp := &mmtp.MmtpMessage{
			MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
			Uuid:    uuid.NewString(),
			Body: &mmtp.MmtpMessage_ResponseMessage{
				ResponseMessage: &mmtp.ResponseMessage{
					ResponseToUuid: mmtpMessage.GetUuid(),
					Response:       mmtp.ResponseEnum_GOOD,
				}},
		}
		if err := rw.WriteMessage(request.Context(), conn, resp); err != nil {
			return fmt.Errorf("could not send disconnect response to Agent: %w", err)
		}

		if err := conn.Close(websocket.StatusNormalClosure, "Closed connection after receiving Disconnect message"); err != nil {
			return fmt.Errorf("websocket could not be closed cleanly: %w", err)
		}
		return nil
	}
	errMsg.SendErrorMessage(mmtpMessage.GetUuid(), "Mismatch between protocol message type and message body", request.Context(), conn)
	return fmt.Errorf("message did not contain a Disconnect message in the body")
}

// HandleFetch fetches message metadata for messages addressed to consumer, and informs consumer about these (metadata only)
func (c *Consumer) HandleFetch(mmtpMessage *mmtp.MmtpMessage, request *http.Request, conn *websocket.Conn) error {
	if fetch := mmtpMessage.GetProtocolMessage().GetFetchMessage(); fetch != nil {
		c.MsgMu.Lock()
		defer c.MsgMu.Unlock()
		messages, err := c.Store.FetchMessages(request.Context(), c.ID, nil)
		if err != nil {
			return fmt.Errorf("load message metadata: %w", err)
		}
		metadata := make([]*mmtp.MessageMetadata, 0, len(messages))
		for _, message := range messages {
			metadata = append(metadata, &mmtp.MessageMetadata{
				Uuid: message.GetUuid(),
				Header: message.GetProtocolMessage().GetSendMessage().
					GetApplicationMessage().GetHeader(),
			})
		}
		resp := &mmtp.MmtpMessage{
			MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
			Uuid:    uuid.NewString(),
			Body: &mmtp.MmtpMessage_ResponseMessage{
				ResponseMessage: &mmtp.ResponseMessage{
					ResponseToUuid:  mmtpMessage.GetUuid(),
					Response:        mmtp.ResponseEnum_GOOD,
					MessageMetadata: metadata,
				}},
		}
		if err = rw.WriteMessage(request.Context(), conn, resp); err != nil {
			return fmt.Errorf("could not send fetch response to Consumer: %w", err)
		}
	}
	return nil
}
