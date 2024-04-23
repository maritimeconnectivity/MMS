package consumer

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"github.com/maritimeconnectivity/MMS/mmtp"
	"github.com/maritimeconnectivity/MMS/utils/rw"
	"log"
	"nhooyr.io/websocket"
	"sync"
	"time"
)

type Consumer struct {
	Mrn            string                       // the MRN of the Consumer
	Interests      []string                     // the Interests that the Consumer wants to subscribe to
	Messages       map[string]*mmtp.MmtpMessage // the incoming messages for this Consumer
	MsgMu          *sync.RWMutex                // RWMutex for locking the Messages map
	ReconnectToken string                       // token for reconnecting to a previous session
	Notifications  map[string]*mmtp.MmtpMessage // Map containing pointers to messages, which the Consumer should be notified about
	NotifyMu       *sync.RWMutex                // a Mutex for Notifications map
}

func (c *Consumer) QueueMessage(mmtpMessage *mmtp.MmtpMessage) error {
	if c != nil {
		uUid := mmtpMessage.GetUuid()
		if uUid == "" {
			return fmt.Errorf("the message does not contain a UUID")
		}
		c.MsgMu.Lock()
		c.Messages[uUid] = mmtpMessage
		c.MsgMu.Unlock()
		c.NotifyMu.Lock()
		c.Notifications[uUid] = mmtpMessage
		c.NotifyMu.Unlock()
	} else {
		return fmt.Errorf("consumer resolved to nil while trying to queue message")
	}
	return nil
}

func (c *Consumer) BulkQueueMessages(mmtpMessages []*mmtp.MmtpMessage) {
	if c != nil {
		c.MsgMu.Lock()
		for _, message := range mmtpMessages {
			c.Messages[message.Uuid] = message
		}
		c.MsgMu.Unlock()
	}
}

func (c *Consumer) notify(ctx context.Context, conn *websocket.Conn) error {
	notifications := make([]*mmtp.MessageMetadata, 0, len(c.Notifications))
	for msgUuid, mmtpMsg := range c.Notifications {
		msgMetadata := &mmtp.MessageMetadata{
			Uuid:   mmtpMsg.GetUuid(),
			Header: mmtpMsg.GetProtocolMessage().GetSendMessage().GetApplicationMessage().GetHeader(),
		}
		notifications = append(notifications, msgMetadata)
		delete(c.Notifications, msgUuid)
	}

	notifyMsg := &mmtp.MmtpMessage{
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
	err := rw.WriteMessage(ctx, conn, notifyMsg)
	if err != nil {
		return fmt.Errorf("could not send Notify to Producer: %w", err)
	}
	return nil
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
			if len(c.Notifications) > 0 {
				if err := c.notify(ctx, conn); err != nil {
					log.Println("Failed Notifying Agent:", err)
				}
			}
			c.NotifyMu.Unlock()
			continue
		}
	}
}
