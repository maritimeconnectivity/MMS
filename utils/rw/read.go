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

package rw

import (
	"context"
	"errors"
	"fmt"

	"github.com/coder/websocket"
	"github.com/maritimeconnectivity/MMS/mmtp"
	"google.golang.org/protobuf/proto"
)

func ReadMessage(ctx context.Context, c *websocket.Conn) (*mmtp.MmtpMessage, int, error) {
	if c == nil {
		return nil, -1, errors.New("No websocket connection")
	}

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
