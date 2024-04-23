package rw

import (
	"context"
	"fmt"
	"github.com/maritimeconnectivity/MMS/mmtp"
	"google.golang.org/protobuf/proto"
	"nhooyr.io/websocket"
)

func ReadMessage(ctx context.Context, c *websocket.Conn) (*mmtp.MmtpMessage, int, error) {
	_, b, err := c.Read(ctx)
	if err != nil {
		return nil, -1, fmt.Errorf("could not read message from Agent: %w", err)
	}
	mmtpMessage := &mmtp.MmtpMessage{}
	if err = proto.Unmarshal(b, mmtpMessage); err != nil {
		return nil, -1, fmt.Errorf("could not unmarshal message: %w", err)
	}
	return mmtpMessage, len(b), nil
}
