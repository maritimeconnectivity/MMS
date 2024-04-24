package errors

import (
	"context"
	"github.com/google/uuid"
	"github.com/maritimeconnectivity/MMS/mmtp"
	"github.com/maritimeconnectivity/MMS/utils/rw"
	"log"
	"nhooyr.io/websocket"
)

func SendErrorMessage(uid string, errorText string, ctx context.Context, c *websocket.Conn) {
	resp := &mmtp.MmtpMessage{
		MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
		Uuid:    uuid.NewString(),
		Body: &mmtp.MmtpMessage_ResponseMessage{
			ResponseMessage: &mmtp.ResponseMessage{
				ResponseToUuid: uid,
				Response:       mmtp.ResponseEnum_ERROR,
				ReasonText:     &errorText,
			}},
	}
	if err := rw.WriteMessage(ctx, c, resp); err != nil {
		log.Println("Could not send error response:", err)
	}
}
