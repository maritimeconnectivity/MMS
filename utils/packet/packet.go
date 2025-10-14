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

package packet

import (
	"fmt"

	"github.com/maritimeconnectivity/MMS/mmtp"
)

// Inspect prints the content of a packet flowing through an MMS Edgerouter or MMS router
func Inspect(packet *mmtp.MmtpMessage) {
	switch packet.GetMsgType() {
	case mmtp.MsgType_PROTOCOL_MESSAGE:
		if packet.GetProtocolMessage().GetProtocolMsgType() == 3 {
			fmt.Println("-----PROTOCOL MSG-----")
			header := packet.GetProtocolMessage().GetSendMessage().GetApplicationMessage().GetHeader()
			body := packet.GetProtocolMessage().GetSendMessage().GetApplicationMessage().GetBody()
			if len(header.GetSubject()) > 0 {
				fmt.Printf("Sender: %s, Subject: %s\n", header.GetSender(), header.GetSubject())
			} else {
				fmt.Printf("Sender: %s Recipients:\n", header.GetSender())
				recipients := header.GetRecipients().GetRecipients()
				for _, recipient := range recipients {
					fmt.Println(recipient)
				}
			}
			fmt.Println("---MSG PAYLOAD---")
			fmt.Println(string(body))
		}
	case mmtp.MsgType_RESPONSE_MESSAGE:
		fmt.Println("Response MSG")
	}
	fmt.Println("-----------------------")
}
