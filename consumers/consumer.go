package consumers

import (
	"fmt"
	"github.com/maritimeconnectivity/MMS/mmtp"
	"sync"
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
