/*
 * Copyright 2023 Maritime Connectivity Platform Consortium
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

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"github.com/google/uuid"
	"github.com/hashicorp/mdns"
	"golang.org/x/crypto/ocsp"
	"google.golang.org/protobuf/proto"
	"io"
	"maritimeconnectivity.net/mms-router/generated/mmtp"
	"net/http"
	"nhooyr.io/websocket"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"
)

// Agent type representing a connected Edge Router
type Agent struct {
	Mrn            string                       // the MRN of the Agent
	Interests      []string                     // the Interests that the Agent wants to subscribe to
	Messages       map[string]*mmtp.MmtpMessage // the incoming messages for this Agent
	msgMu          *sync.RWMutex                // RWMutex for locking the Messages map
	reconnectToken string                       // token for reconnecting to a previous session
}

func (er *Agent) QueueMessage(mmtpMessage *mmtp.MmtpMessage) error {
	uUid := mmtpMessage.GetUuid()
	if uUid == "" {
		return fmt.Errorf("the message does not contain a UUID")
	}
	er.msgMu.Lock()
	er.Messages[uUid] = mmtpMessage
	er.msgMu.Unlock()
	return nil
}

func (er *Agent) BulkQueueMessages(mmtpMessages []*mmtp.MmtpMessage) {
	er.msgMu.Lock()
	for _, message := range mmtpMessages {
		er.Messages[message.Uuid] = message
	}
	er.msgMu.Unlock()
}

// Subscription type representing a subscription
type Subscription struct {
	Interest    string            // the Interest that the Subscription is based on
	Subscribers map[string]*Agent // the Agents that subscribe
	subsMu      *sync.RWMutex     // RWMutex for locking the Subscribers map
}

func NewSubscription(interest string) *Subscription {
	return &Subscription{
		Interest:    interest,
		Subscribers: make(map[string]*Agent),
		subsMu:      &sync.RWMutex{},
	}
}

func (sub *Subscription) AddSubscriber(agent *Agent) {
	sub.subsMu.Lock()
	sub.Subscribers[agent.Mrn] = agent
	sub.subsMu.Unlock()
}

func (sub *Subscription) DeleteSubscriber(agent *Agent) {
	sub.subsMu.Lock()
	delete(sub.Subscribers, agent.Mrn)
	sub.subsMu.Unlock()
}

// EdgeRouter type representing an MMS edge router
type EdgeRouter struct {
	ownMrn          string                   // The MRN of this EdgeRouter
	subscriptions   map[string]*Subscription // a mapping from Interest names to Subscription slices
	subMu           *sync.RWMutex            // a Mutex for locking the subscriptions map
	agents          map[string]*Agent        // a map of connected Agents
	agentsMu        *sync.RWMutex            // a Mutex for locking the agents map
	httpServer      *http.Server             // the http server that is used to bootstrap websocket connections
	outgoingChannel chan *mmtp.MmtpMessage   // channel for outgoing messages
	ws              *websocket.Conn          // the websocket connection to the MMS Router
	ctx             context.Context          // the main Context of the EdgeRouter
}

func NewEdgeRouter(listeningAddr string, mrn string, outgoingChannel chan *mmtp.MmtpMessage, ws *websocket.Conn, ctx context.Context) *EdgeRouter {
	subs := make(map[string]*Subscription)
	subMu := &sync.RWMutex{}
	agents := make(map[string]*Agent)
	agentsMu := &sync.RWMutex{}
	httpServer := http.Server{
		Addr:    listeningAddr,
		Handler: handleHttpConnection(outgoingChannel, subs, subMu, agents, agentsMu, ctx),
		TLSConfig: &tls.Config{
			ClientAuth:            tls.VerifyClientCertIfGiven,
			ClientCAs:             nil,
			MinVersion:            tls.VersionTLS13,
			VerifyPeerCertificate: verifyAgentCertificate(),
		},
	}

	return &EdgeRouter{
		ownMrn:          mrn,
		subscriptions:   subs,
		subMu:           subMu,
		agents:          agents,
		agentsMu:        agentsMu,
		httpServer:      &httpServer,
		outgoingChannel: outgoingChannel,
		ws:              ws,
		ctx:             ctx,
	}
}

func (er *EdgeRouter) StartEdgeRouter(ctx context.Context) {
	connect := &mmtp.MmtpMessage{
		MsgType: mmtp.MsgType_PROTOCOL_MESSAGE,
		Uuid:    uuid.NewString(),
		Body: &mmtp.MmtpMessage_ProtocolMessage{
			ProtocolMessage: &mmtp.ProtocolMessage{
				ProtocolMsgType: mmtp.ProtocolMessageType_CONNECT_MESSAGE,
				Body: &mmtp.ProtocolMessage_ConnectMessage{
					ConnectMessage: &mmtp.Connect{
						OwnMrn: &er.ownMrn,
					},
				},
			},
		},
	}
	err := writeMessage(ctx, er.ws, connect)
	if err != nil {
		fmt.Println("Could not send connect message:", err)
		return
	}

	response, err := readMessage(ctx, er.ws)
	if err != nil {
		fmt.Println("Something went wrong while receiving response from MMS Router:", err)
		return
	}

	connectResp := response.GetResponseMessage()
	if connectResp.Response != mmtp.ResponseEnum_GOOD {
		fmt.Println("MMS Router did not accept Connect:", connectResp.GetReasonText())
		return
	}

	// TODO store reconnect token and handle reconnection in case of disconnect

	go func() {
		fmt.Println("Starting edge router")
		if err := er.httpServer.ListenAndServe(); err != nil {
			fmt.Println(err)
		}
	}()

	go handleIncomingMessages(ctx, er)
	go handleOutgoingMessages(ctx, er)
	<-ctx.Done()
	fmt.Println("Shutting down edge router")
	fmt.Println("subscriptions:", er.subscriptions)
	if err := er.httpServer.Shutdown(ctx); err != nil {
		panic(err)
	}
}

func handleHttpConnection(outgoingChannel chan<- *mmtp.MmtpMessage, subs map[string]*Subscription, subMu *sync.RWMutex, agents map[string]*Agent, agentsMu *sync.RWMutex, ctx context.Context) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		c, err := websocket.Accept(writer, request, nil)
		if err != nil {
			fmt.Println("Could not establish websocket connection", err)
			return
		}
		defer func(c *websocket.Conn, code websocket.StatusCode, reason string) {
			err := c.Close(code, reason)
			if err != nil {
				fmt.Println("Could not close connection:", err)
			}
		}(c, websocket.StatusInternalError, "PANIC!!!")

		mmtpMessage, err := readMessage(request.Context(), c)
		if err != nil {
			fmt.Println(err)
			return
		}

		protoMessage := mmtpMessage.GetProtocolMessage()
		if mmtpMessage.MsgType != mmtp.MsgType_PROTOCOL_MESSAGE || protoMessage == nil {
			if err = c.Close(websocket.StatusUnsupportedData, "The first message needs to be a Protocol Message containing a Connect message with the MRN of the Edge Router"); err != nil {
				fmt.Println(err)
			}
			return
		}

		connect := protoMessage.GetConnectMessage()
		if connect == nil {
			if err = c.Close(websocket.StatusUnsupportedData, "The first message needs to contain a Connect message with the MRN of the Edge Router"); err != nil {
				fmt.Println(err)
			}
			return
		}

		agentMrn := connect.GetOwnMrn()
		if agentMrn == "" {
			if err = c.Close(websocket.StatusUnsupportedData, "The first message needs to be a Connect message with the MRN of the Edge Router"); err != nil {
				fmt.Println(err)
			}
			return
		}

		// Uncomment the following block when using TLS
		//uidOid := []int{0, 9, 2342, 19200300, 100, 1, 1}
		//
		//// https://stackoverflow.com/a/50640119
		//for _, n := range request.TLS.PeerCertificates[0].Subject.Names {
		//	if n.Type.Equal(uidOid) {
		//		if v, ok := n.Value.(string); ok {
		//			if !strings.EqualFold(v, agentMrn) {
		//				if err = c.Close(websocket.StatusUnsupportedData, "The MRN given in the Connect message does not match the one in the certificate that was used for authentication"); err != nil {
		//					fmt.Println(err)
		//				}
		//				return
		//			}
		//		}
		//	}
		//}

		var agent *Agent
		if connect.ReconnectToken != nil {
			agentsMu.RLock()
			agent = agents[agentMrn]
			agentsMu.RUnlock()
			if agent == nil {
				errorMsg := "No existing session was found for the given MRN"
				resp := &mmtp.MmtpMessage{
					MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
					Uuid:    uuid.NewString(),
					Body: &mmtp.MmtpMessage_ResponseMessage{
						ResponseMessage: &mmtp.ResponseMessage{
							ResponseToUuid: mmtpMessage.GetUuid(),
							Response:       mmtp.ResponseEnum_ERROR,
							ReasonText:     &errorMsg,
						}},
				}
				if err = writeMessage(request.Context(), c, resp); err != nil {
					fmt.Println("Could not send error message:", err)
					return
				}
			}
			if connect.GetReconnectToken() != agent.reconnectToken {
				errorMsg := "The given reconnect token does not match the one that is stored"
				resp := &mmtp.MmtpMessage{
					MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
					Uuid:    uuid.NewString(),
					Body: &mmtp.MmtpMessage_ResponseMessage{
						ResponseMessage: &mmtp.ResponseMessage{
							ResponseToUuid: mmtpMessage.GetUuid(),
							Response:       mmtp.ResponseEnum_ERROR,
							ReasonText:     &errorMsg,
						}},
				}
				if err = writeMessage(request.Context(), c, resp); err != nil {
					fmt.Println("Could not send error message:", err)
					return
				}
			}
		} else {
			agent = &Agent{
				Mrn:       agentMrn,
				Interests: make([]string, 0, 1),
				Messages:  make(map[string]*mmtp.MmtpMessage),
				msgMu:     &sync.RWMutex{},
			}
		}

		agent.reconnectToken = uuid.NewString()

		agentsMu.Lock()
		agents[agent.Mrn] = agent
		agentsMu.Unlock()

		resp := &mmtp.MmtpMessage{
			MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
			Uuid:    uuid.NewString(),
			Body: &mmtp.MmtpMessage_ResponseMessage{
				ResponseMessage: &mmtp.ResponseMessage{
					ResponseToUuid: mmtpMessage.GetUuid(),
					Response:       mmtp.ResponseEnum_GOOD,
				}},
		}
		err = writeMessage(request.Context(), c, resp)
		if err != nil {
			fmt.Println("Could not send response:", err)
			return
		}

		for {
			mmtpMessage, err = readMessage(request.Context(), c)
			if err != nil {
				fmt.Println("Something went wrong while reading message from Edge Router:", err)
				return
			}
			switch mmtpMessage.GetMsgType() {
			case mmtp.MsgType_PROTOCOL_MESSAGE:
				{
					protoMessage = mmtpMessage.GetProtocolMessage()
					if protoMessage == nil {
						continue
					}
					switch protoMessage.ProtocolMsgType {
					case mmtp.ProtocolMessageType_SUBSCRIBE_MESSAGE:
						{
							if subscribe := protoMessage.GetSubscribeMessage(); subscribe != nil {
								subject := subscribe.GetSubject()
								if subject == "" {
									continue
								}
								subMu.Lock()
								sub, exists := subs[subject]
								if !exists {
									sub = NewSubscription(subject)
									sub.AddSubscriber(agent)
									subs[subject] = sub
								} else {
									sub.AddSubscriber(agent)
								}
								subMu.Unlock()
								agent.Interests = append(agent.Interests, subject)

								resp = &mmtp.MmtpMessage{
									MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
									Uuid:    uuid.NewString(),
									Body: &mmtp.MmtpMessage_ResponseMessage{
										ResponseMessage: &mmtp.ResponseMessage{
											ResponseToUuid: mmtpMessage.GetUuid(),
											Response:       mmtp.ResponseEnum_GOOD,
										}},
								}
								if err = writeMessage(request.Context(), c, resp); err != nil {
									fmt.Println("Could not send subscribe response to Edge Router:", err)
								}
							}
							break
						}
					case mmtp.ProtocolMessageType_UNSUBSCRIBE_MESSAGE:
						{
							if unsubscribe := protoMessage.GetUnsubscribeMessage(); unsubscribe != nil {
								subject := unsubscribe.GetSubject()
								if subject == "" {
									continue
								}
								subMu.Lock()
								sub, exists := subs[subject]
								if !exists {
									continue
								}
								sub.DeleteSubscriber(agent)
								subMu.Unlock()
								interests := agent.Interests
								for i := range interests {
									if strings.EqualFold(subject, interests[i]) {
										interests[i] = interests[len(interests)-1]
										interests[len(interests)-1] = ""
										interests = interests[:len(interests)-1]
										agent.Interests = interests
									}
								}
								resp = &mmtp.MmtpMessage{
									MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
									Uuid:    uuid.NewString(),
									Body: &mmtp.MmtpMessage_ResponseMessage{
										ResponseMessage: &mmtp.ResponseMessage{
											ResponseToUuid: mmtpMessage.GetUuid(),
											Response:       mmtp.ResponseEnum_GOOD,
										}},
								}
								if err = writeMessage(request.Context(), c, resp); err != nil {
									fmt.Println("Could not write response to unsubscribe message:", err)
								}
							}
							break
						}
					case mmtp.ProtocolMessageType_SEND_MESSAGE:
						{
							if send := protoMessage.GetSendMessage(); send != nil {
								outgoingChannel <- mmtpMessage
								//
								//resp = mmtp.MmtpMessage{
								//	MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
								//	Uuid:    uuid.NewString(),
								//	Body: &mmtp.MmtpMessage_ResponseMessage{
								//		ResponseMessage: &mmtp.ResponseMessage{
								//			ResponseToUuid: mmtpMessage.GetUuid(),
								//			Response:       mmtp.ResponseEnum_GOOD,
								//		}},
								//}
								//if err = wspb.Write(request.Context(), c, &resp); err != nil {
								//	fmt.Println("Could not send Send response to Edge Router:", err)
								//}
							}
							break
						}
					case mmtp.ProtocolMessageType_RECEIVE_MESSAGE:
						{
							if receive := protoMessage.GetReceiveMessage(); receive != nil {
								if msgUuids := receive.GetFilter().GetMessageUuids(); msgUuids != nil {
									msgsLen := len(msgUuids)
									mmtpMessages := make([]*mmtp.MmtpMessage, 0, msgsLen)
									appMsgs := make([]*mmtp.ApplicationMessage, 0, msgsLen)
									agent.msgMu.Lock()
									for _, msgUuid := range msgUuids {
										msg := agent.Messages[msgUuid].GetProtocolMessage().GetSendMessage().GetApplicationMessage()
										appMsgs = append(appMsgs, msg)
										delete(agent.Messages, msgUuid)
									}
									agent.msgMu.Unlock()
									resp = &mmtp.MmtpMessage{
										MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
										Uuid:    uuid.NewString(),
										Body: &mmtp.MmtpMessage_ResponseMessage{ResponseMessage: &mmtp.ResponseMessage{
											ResponseToUuid:      mmtpMessage.GetUuid(),
											Response:            mmtp.ResponseEnum_GOOD,
											ApplicationMessages: appMsgs,
										}},
									}
									err = writeMessage(request.Context(), c, resp)
									if err != nil {
										fmt.Println("Could not send messages to Edge Router:", err)
										agent.BulkQueueMessages(mmtpMessages)
									}
								} else { // Receive all messages
									agent.msgMu.Lock()
									msgsLen := len(agent.Messages)
									appMsgs := make([]*mmtp.ApplicationMessage, 0, msgsLen)
									for _, mmtpMsg := range agent.Messages {
										msg := mmtpMsg.GetProtocolMessage().GetSendMessage().GetApplicationMessage()
										appMsgs = append(appMsgs, msg)
									}
									resp = &mmtp.MmtpMessage{
										MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
										Uuid:    uuid.NewString(),
										Body: &mmtp.MmtpMessage_ResponseMessage{ResponseMessage: &mmtp.ResponseMessage{
											ResponseToUuid:      mmtpMessage.GetUuid(),
											Response:            mmtp.ResponseEnum_GOOD,
											ApplicationMessages: appMsgs,
										}},
									}
									err = writeMessage(request.Context(), c, resp)
									if err != nil {
										fmt.Println("Could not send messages to Edge Router:", err)
									} else {
										agent.Messages = make(map[string]*mmtp.MmtpMessage)
									}
									agent.msgMu.Unlock()
								}
							}
							break
						}
					case mmtp.ProtocolMessageType_FETCH_MESSAGE:
						{
							if fetch := protoMessage.GetFetchMessage(); fetch != nil {
								agent.msgMu.RLock()
								metadata := make([]*mmtp.MessageMetadata, 0, len(agent.Messages))
								for _, msg := range agent.Messages {
									msgHeader := msg.GetProtocolMessage().GetSendMessage().GetApplicationMessage().GetHeader()
									msgMetadata := &mmtp.MessageMetadata{
										Uuid:   msg.GetUuid(),
										Header: msgHeader,
									}
									metadata = append(metadata, msgMetadata)
								}
								agent.msgMu.RUnlock()
								resp = &mmtp.MmtpMessage{
									MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
									Uuid:    uuid.NewString(),
									Body: &mmtp.MmtpMessage_ResponseMessage{
										ResponseMessage: &mmtp.ResponseMessage{
											ResponseToUuid:  mmtpMessage.GetUuid(),
											Response:        mmtp.ResponseEnum_GOOD,
											MessageMetadata: metadata,
										}},
								}
								err = writeMessage(request.Context(), c, resp)
								if err != nil {
									fmt.Println("Could not send fetch response to Edge Router:", err)
								}
							}
							break
						}
					case mmtp.ProtocolMessageType_DISCONNECT_MESSAGE:
						{
							if disconnect := protoMessage.GetDisconnectMessage(); disconnect != nil {
								resp = &mmtp.MmtpMessage{
									MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
									Uuid:    uuid.NewString(),
									Body: &mmtp.MmtpMessage_ResponseMessage{
										ResponseMessage: &mmtp.ResponseMessage{
											ResponseToUuid: mmtpMessage.GetUuid(),
											Response:       mmtp.ResponseEnum_GOOD,
										}},
								}
								if err = writeMessage(request.Context(), c, resp); err != nil {
									fmt.Println("Could not send disconnect response to Edge Router:", err)
								}

								if err = c.Close(websocket.StatusNormalClosure, "Closed connection after receiving Disconnect message"); err != nil {
									fmt.Println("Websocket could not be closed cleanly:", err)
									return
								}
							}
							break
						}
					case mmtp.ProtocolMessageType_CONNECT_MESSAGE:
						{
							reason := "Already connected"
							resp = &mmtp.MmtpMessage{
								MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
								Uuid:    uuid.NewString(),
								Body: &mmtp.MmtpMessage_ResponseMessage{
									ResponseMessage: &mmtp.ResponseMessage{
										ResponseToUuid: mmtpMessage.GetUuid(),
										Response:       mmtp.ResponseEnum_ERROR,
										ReasonText:     &reason,
									}},
							}
							if err = writeMessage(request.Context(), c, resp); err != nil {
								fmt.Println("Could not send error response:", err)
							}
							break
						}
					default:
						continue
					}
				}
			case mmtp.MsgType_RESPONSE_MESSAGE:
			default:
				continue
			}
		}

	}
}

func verifyAgentCertificate() func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
		// we did not receive a certificate from the client, so we just return early
		if len(rawCerts) == 0 || len(verifiedChains) == 0 {
			return nil
		}

		clientCert := verifiedChains[0][0]
		issuingCert := verifiedChains[0][1]

		httpClient := http.DefaultClient
		if len(clientCert.OCSPServer) > 0 {
			ocspUrl := clientCert.OCSPServer[0]
			ocspReq, err := ocsp.CreateRequest(clientCert, issuingCert, nil)
			if err != nil {
				return fmt.Errorf("could not create OCSP request for the given client cert: %w", err)
			}
			resp, err := httpClient.Post(ocspUrl, "application/ocsp-request", bytes.NewBuffer(ocspReq))
			if err != nil {
				return fmt.Errorf("could not send OCSP request: %w", err)
			}
			respBytes, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("getting OCSP response failed: %w", err)
			}
			if err = resp.Body.Close(); err != nil {
				return fmt.Errorf("could not close response body: %w", err)
			}
			ocspResp, err := ocsp.ParseResponse(respBytes, nil)
			if err != nil {
				return fmt.Errorf("parsing OCSP response failed: %w", err)
			}
			if ocspResp.SerialNumber.Cmp(clientCert.SerialNumber) != 0 {
				return fmt.Errorf("the serial number in the OCSP response does not correspond to the serial number of the certificate being checked")
			}
			if err = ocspResp.CheckSignatureFrom(issuingCert); err != nil {
				return fmt.Errorf("the signature on the OCSP response is not valid: %w", err)
			}
			if ocspResp.Status != ocsp.Good {
				return fmt.Errorf("the given client certificate has been revoked")
			}
		} else if len(clientCert.CRLDistributionPoints) > 0 {
			crlURL := clientCert.CRLDistributionPoints[0]
			resp, err := httpClient.Get(crlURL)
			if err != nil {
				return fmt.Errorf("could not send CRL request: %w", err)
			}
			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("getting CRL response body failed: %w", err)
			}
			if err = resp.Body.Close(); err != nil {
				return fmt.Errorf("failed to close CRL response: %w body", err)
			}
			crl, err := x509.ParseRevocationList(respBody)
			if err != nil {
				return fmt.Errorf("could not parse received CRL: %w", err)
			}
			if err = crl.CheckSignatureFrom(issuingCert); err != nil {
				return fmt.Errorf("signature on CRL is not valid: %w", err)
			}
			now := time.Now().UTC()
			for _, rev := range crl.RevokedCertificates {
				if (rev.SerialNumber.Cmp(clientCert.SerialNumber) == 0) && (rev.RevocationTime.UTC().Before(now)) {
					return fmt.Errorf("the given client certificate has been revoked")
				}
			}
		} else {
			return fmt.Errorf("was not able to check revocation status of client certificate")
		}

		return nil
	}
}

func handleIncomingMessages(ctx context.Context, edgeRouter *EdgeRouter) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			receiveMsg := &mmtp.MmtpMessage{
				MsgType: mmtp.MsgType_PROTOCOL_MESSAGE,
				Uuid:    uuid.NewString(),
				Body: &mmtp.MmtpMessage_ProtocolMessage{
					ProtocolMessage: &mmtp.ProtocolMessage{
						ProtocolMsgType: mmtp.ProtocolMessageType_RECEIVE_MESSAGE,
						Body: &mmtp.ProtocolMessage_ReceiveMessage{
							ReceiveMessage: &mmtp.Receive{},
						},
					},
				},
			}
			if err := writeMessage(ctx, edgeRouter.ws, receiveMsg); err != nil {
				fmt.Println("Was not able to send Receive message to MMS Router:", err)
				continue
			}

			response, err := readMessage(ctx, edgeRouter.ws)
			if err != nil {
				fmt.Println("Could not receive response from MMS Router:", err)
				continue
			}

			if _, err := uuid.Parse(response.GetUuid()); err != nil {
				// message UUID is invalid, so we discard it
				continue
			}

			switch response.GetBody().(type) {
			case *mmtp.MmtpMessage_ResponseMessage:
				{
					responseMsg := response.GetResponseMessage()
					if responseMsg.Response != mmtp.ResponseEnum_GOOD {
						fmt.Println("Received response with error:", responseMsg.GetReasonText())
						continue
					}
					for _, appMsg := range responseMsg.GetApplicationMessages() {
						if appMsg == nil {
							continue
						}
						incomingMessage := &mmtp.MmtpMessage{
							MsgType: mmtp.MsgType_PROTOCOL_MESSAGE,
							Uuid:    uuid.NewString(),
							Body: &mmtp.MmtpMessage_ProtocolMessage{
								ProtocolMessage: &mmtp.ProtocolMessage{
									ProtocolMsgType: mmtp.ProtocolMessageType_SEND_MESSAGE,
									Body: &mmtp.ProtocolMessage_SendMessage{
										SendMessage: &mmtp.Send{
											ApplicationMessage: appMsg,
										},
									},
								},
							},
						}
						switch subjectOrRecipient := appMsg.GetHeader().GetSubjectOrRecipient().(type) {
						case *mmtp.ApplicationMessageHeader_Subject:
							{
								edgeRouter.subMu.RLock()
								for _, subscriber := range edgeRouter.subscriptions[subjectOrRecipient.Subject].Subscribers {
									if err := subscriber.QueueMessage(incomingMessage); err != nil {
										fmt.Println("Could not queue message:", err)
										continue
									}
								}
								edgeRouter.subMu.RUnlock()
							}
						case *mmtp.ApplicationMessageHeader_Recipients:
							{
								edgeRouter.agentsMu.RLock()
								for _, recipient := range subjectOrRecipient.Recipients.GetRecipients() {
									err := edgeRouter.agents[recipient].QueueMessage(incomingMessage)
									if err != nil {
										fmt.Println("Could not queue message for Edge Router:", err)
									}
								}
								edgeRouter.agentsMu.RUnlock()
							}
						}
					}
				}
			default:
				continue
			}
		default:
			continue
		}
	}
}

func handleOutgoingMessages(ctx context.Context, edgeRouter *EdgeRouter) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			for outgoingMessage := range edgeRouter.outgoingChannel {
				switch outgoingMessage.GetMsgType() {
				case mmtp.MsgType_PROTOCOL_MESSAGE:
					{
						if err := writeMessage(ctx, edgeRouter.ws, outgoingMessage); err != nil {
							fmt.Println("Could not send outgoing message to MMS Router:", err)
							continue
						}
					}
				default:
					continue
				}
			}
		}
	}
}

func readMessage(ctx context.Context, c *websocket.Conn) (*mmtp.MmtpMessage, error) {
	_, b, err := c.Read(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not read message from edge router: %w", err)
	}
	mmtpMessage := &mmtp.MmtpMessage{}
	if err = proto.Unmarshal(b, mmtpMessage); err != nil {
		return nil, fmt.Errorf("could not unmarshal message: %w", err)
	}
	return mmtpMessage, nil
}

func writeMessage(ctx context.Context, c *websocket.Conn, mmtpMessage *mmtp.MmtpMessage) error {
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

func main() {
	ctx, cancel := context.WithCancel(context.Background())

	// wait for a SIGINT or SIGTERM signal
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)

	outgoingChannel := make(chan *mmtp.MmtpMessage)

	ws, _, err := websocket.Dial(ctx, "ws://localhost:8080", nil)
	if err != nil {
		fmt.Println("Could not connect to MMS Router:", err)
		return
	}

	er := NewEdgeRouter("0.0.0.0:8888", "urn:mrn:mcp:device:idp1:org1:er", outgoingChannel, ws, ctx)
	go er.StartEdgeRouter(ctx)

	hst, err := os.Hostname()
	if err != nil {
		fmt.Println("Could not get hostname, shutting down", err)
		ch <- os.Interrupt
	}
	info := []string{"MMS Edge Router"}
	mdnsService, err := mdns.NewMDNSService(hst, "_mms-edgerouter._tcp", "", "", 8888, nil, info)
	if err != nil {
		fmt.Println("Could not create mDNS service, shutting down", err)
		ch <- os.Interrupt
	}
	mdnsServer, err := mdns.NewServer(&mdns.Config{Zone: mdnsService})
	if err != nil {
		fmt.Println("Could not create mDNS server, shutting down", err)
		ch <- os.Interrupt
	}
	defer func(mdnsServer *mdns.Server) {
		err := mdnsServer.Shutdown()
		if err != nil {
			fmt.Println("Shutting down mDNS server failed", err)
		}
	}(mdnsServer)

	<-ch
	fmt.Println("Received signal, shutting down...")
	cancel()
}
