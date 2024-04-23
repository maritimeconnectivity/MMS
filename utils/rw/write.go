package rw

import (
	"context"
	"fmt"
	"github.com/maritimeconnectivity/MMS/mmtp"
	"google.golang.org/protobuf/proto"
	"nhooyr.io/websocket"
)

func WriteMessage(ctx context.Context, c *websocket.Conn, mmtpMessage *mmtp.MmtpMessage) error {
	b, err := proto.Marshal(mmtpMessage)
	if err != nil {
		return fmt.Errorf("could not marshal message: %w", err)
	}
	err = c.Write(ctx, websocket.MessageBinary, b)
	if err != nil {
		return fmt.Errorf("could not write message: %w", err)
	}
	return nil
}
