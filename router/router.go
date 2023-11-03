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
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	peerstore "github.com/libp2p/go-libp2p/core/peer"
	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	dutil "github.com/libp2p/go-libp2p/p2p/discovery/util"
	"github.com/libp2p/go-libp2p/p2p/protocol/identify"
	"github.com/maritimeconnectivity/MMS/mmtp"
	"golang.org/x/crypto/ocsp"
	"google.golang.org/protobuf/proto"
	"io"
	"net/http"
	"nhooyr.io/websocket"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	WsReadLimit int64 = 1 << 20 // 1 MiB
)

// EdgeRouter type representing a connected Edge Router
type EdgeRouter struct {
	Mrn            string                       // the MRN of the EdgeRouter
	Interests      []string                     // the Interests that the EdgeRouter wants to subscribe to
	Messages       map[string]*mmtp.MmtpMessage // the incoming messages for this EdgeRouter
	msgMu          *sync.RWMutex                // RWMutex for locking the Messages map
	reconnectToken string                       // token for reconnecting to a previous session
}

func (er *EdgeRouter) QueueMessage(mmtpMessage *mmtp.MmtpMessage) error {
	if er != nil {
		uUid := mmtpMessage.GetUuid()
		if uUid == "" {
			return fmt.Errorf("the message does not contain a UUID")
		}
		er.msgMu.Lock()
		er.Messages[uUid] = mmtpMessage
		er.msgMu.Unlock()
	}
	return nil
}

func (er *EdgeRouter) BulkQueueMessages(mmtpMessages []*mmtp.MmtpMessage) {
	if er != nil {
		er.msgMu.Lock()
		for _, message := range mmtpMessages {
			er.Messages[message.Uuid] = message
		}
		er.msgMu.Unlock()
	}
}

// Subscription type representing a subscription
type Subscription struct {
	Interest    string                 // the Interest that the Subscription is based on
	Subscribers map[string]*EdgeRouter // the EdgeRouters that subscribe
	Topic       *pubsub.Topic          // The Topic for the subscription
	subsMu      *sync.RWMutex          // RWMutex for locking the Subscribers map
}

func NewSubscription(interest string) *Subscription {
	return &Subscription{
		Interest:    interest,
		Subscribers: make(map[string]*EdgeRouter),
		subsMu:      &sync.RWMutex{},
	}
}

func (sub *Subscription) AddSubscriber(edgeRouter *EdgeRouter) {
	sub.subsMu.Lock()
	sub.Subscribers[edgeRouter.Mrn] = edgeRouter
	sub.subsMu.Unlock()
}

func (sub *Subscription) DeleteSubscriber(edgeRouter *EdgeRouter) {
	sub.subsMu.Lock()
	delete(sub.Subscribers, edgeRouter.Mrn)
	sub.subsMu.Unlock()
}

// MMSRouter type representing an MMS edge router
type MMSRouter struct {
	subscriptions   map[string]*Subscription // a mapping from Interest names to Subscription slices
	subMu           *sync.RWMutex            // a Mutex for locking the subscriptions map
	edgeRouters     map[string]*EdgeRouter   // a map of connected EdgeRouters
	erMu            *sync.RWMutex            // a Mutex for locking the edgeRouters map
	httpServer      *http.Server             // the http server that is used to bootstrap websocket connections
	p2pHost         *host.Host               // the libp2p host that is used to connect to the MMS router network
	pubSub          *pubsub.PubSub           // a PubSub instance for the EdgeRouter
	topicHandles    map[string]*pubsub.Topic // a map of Topic handles
	incomingChannel chan *mmtp.MmtpMessage   // channel for incoming messages
	outgoingChannel chan *mmtp.MmtpMessage   // channel for outgoing messages
	ctx             context.Context          // the main Context of the MMSRouter
}

func NewMMSRouter(p2p *host.Host, pubSub *pubsub.PubSub, listeningAddr string, incomingChannel chan *mmtp.MmtpMessage, outgoingChannel chan *mmtp.MmtpMessage, ctx context.Context, wg *sync.WaitGroup, clientCAs *string) (*MMSRouter, error) {
	subs := make(map[string]*Subscription)
	subMu := &sync.RWMutex{}
	edgeRouters := make(map[string]*EdgeRouter)
	erMu := &sync.RWMutex{}
	topicHandles := make(map[string]*pubsub.Topic)

	var certPool *x509.CertPool = nil
	if *clientCAs != "" {
		certPool = x509.NewCertPool()
		certFile, err := os.ReadFile(*clientCAs)
		if err != nil {
			return nil, fmt.Errorf("could not read the given client CA file")
		}
		if !certPool.AppendCertsFromPEM(certFile) {
			return nil, fmt.Errorf("could not read the given client CA file")
		}
	}

	httpServer := http.Server{
		Addr:    listeningAddr,
		Handler: handleHttpConnection(p2p, pubSub, incomingChannel, outgoingChannel, subs, subMu, edgeRouters, erMu, topicHandles, ctx, wg),
		TLSConfig: &tls.Config{
			ClientAuth:            tls.RequireAndVerifyClientCert,
			ClientCAs:             certPool, // this should come from a file containing the CAs we trust
			MinVersion:            tls.VersionTLS13,
			VerifyPeerCertificate: verifyEdgeRouterCertificate(),
		},
	}

	return &MMSRouter{
		subscriptions:   subs,
		subMu:           subMu,
		edgeRouters:     edgeRouters,
		erMu:            erMu,
		httpServer:      &httpServer,
		p2pHost:         p2p,
		pubSub:          pubSub,
		topicHandles:    topicHandles,
		incomingChannel: incomingChannel,
		outgoingChannel: outgoingChannel,
		ctx:             ctx,
	}, nil
}

func (r *MMSRouter) StartRouter(ctx context.Context, wg *sync.WaitGroup, certPath *string, certKeyPath *string) {
	fmt.Println("Starting MMS Router")
	wg.Add(3)
	go func() {
		fmt.Println("Websocket listening on:", r.httpServer.Addr)
		if *certPath != "" && *certKeyPath != "" {
			if err := r.httpServer.ListenAndServeTLS(*certPath, *certKeyPath); err != nil {
				fmt.Println(err)
			}
		} else {
			if err := r.httpServer.ListenAndServe(); err != nil {
				fmt.Println(err)
			}
		}
		wg.Done()
	}()
	go handleIncomingMessages(ctx, r, wg)
	go handleOutgoingMessages(ctx, r, wg)
	<-ctx.Done()
	fmt.Println("Shutting down MMS router")
	close(r.incomingChannel)
	close(r.outgoingChannel)
	if err := r.httpServer.Shutdown(context.Background()); err != nil {
		fmt.Println(err)
	}
	wg.Done()
}

func handleHttpConnection(p2p *host.Host, pubSub *pubsub.PubSub, incomingChannel chan *mmtp.MmtpMessage, outgoingChannel chan<- *mmtp.MmtpMessage, subs map[string]*Subscription, subMu *sync.RWMutex, edgeRouters map[string]*EdgeRouter, erMu *sync.RWMutex, topicHandles map[string]*pubsub.Topic, ctx context.Context, wg *sync.WaitGroup) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		wg.Add(1)
		c, err := websocket.Accept(writer, request, nil)
		if err != nil {
			fmt.Println("Could not establish websocket connection", err)
			wg.Done()
			return
		}
		defer func(c *websocket.Conn, code websocket.StatusCode, reason string) {
			err := c.Close(code, reason)
			if err != nil {
				fmt.Println("Could not close connection:", err)
			}
			wg.Done()
		}(c, websocket.StatusInternalError, "PANIC!!!")

		// Set the read limit to 1 MiB instead of 32 KiB
		c.SetReadLimit(WsReadLimit)

		mmtpMessage := &mmtp.MmtpMessage{}
		mmtpMessage, err = readMessage(ctx, c)
		if err != nil {
			fmt.Println("Could not read message:", err)
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

		erMrn := connect.GetOwnMrn()
		if erMrn == "" {
			if err = c.Close(websocket.StatusUnsupportedData, "The first message needs to be a Connect message with the MRN of the Edge Router"); err != nil {
				fmt.Println(err)
			}
			return
		}
		erMrn = strings.ToLower(erMrn)

		// If TLS is enabled, we should verify the certificate from the Edge Router
		if request.TLS != nil {
			uidOid := []int{0, 9, 2342, 19200300, 100, 1, 1}

			if len(request.TLS.PeerCertificates) < 1 {
				if err = c.Close(websocket.StatusPolicyViolation, "A valid client certificate must be provided for authenticated connections"); err != nil {
					fmt.Println(err)
				}
				return
			}
			// https://stackoverflow.com/a/50640119
			for _, n := range request.TLS.PeerCertificates[0].Subject.Names {
				if n.Type.Equal(uidOid) {
					if v, ok := n.Value.(string); ok {
						if !strings.EqualFold(v, erMrn) {
							if err = c.Close(websocket.StatusUnsupportedData, "The MRN given in the Connect message does not match the one in the certificate that was used for authentication"); err != nil {
								fmt.Println(err)
							}
							return
						}
					}
				}
			}
		}

		var e *EdgeRouter
		if connect.ReconnectToken != nil {
			erMu.RLock()
			e = edgeRouters[erMrn]
			erMu.RUnlock()
			if e == nil {
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
				err = writeMessage(request.Context(), c, resp)
				if err != nil {
					fmt.Println("Could not send response message:", err)
					return
				}
			}
			if connect.GetReconnectToken() != e.reconnectToken {
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
				err = writeMessage(request.Context(), c, resp)
				if err != nil {
					fmt.Println("Could not send response message:", err)
					return
				}
			}
		} else {
			e = &EdgeRouter{
				Mrn:       erMrn,
				Interests: make([]string, 0, 1),
				Messages:  make(map[string]*mmtp.MmtpMessage),
				msgMu:     &sync.RWMutex{},
			}
		}

		e.reconnectToken = uuid.NewString()

		erMu.Lock()
		edgeRouters[e.Mrn] = e
		erMu.Unlock()

		resp := &mmtp.MmtpMessage{
			MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
			Uuid:    uuid.NewString(),
			Body: &mmtp.MmtpMessage_ResponseMessage{
				ResponseMessage: &mmtp.ResponseMessage{
					ResponseToUuid: mmtpMessage.GetUuid(),
					ReconnectToken: &e.reconnectToken,
					Response:       mmtp.ResponseEnum_GOOD,
				}},
		}
		err = writeMessage(request.Context(), c, resp)
		if err != nil {
			fmt.Println("Could not send response message:", err)
			return
		}

		for {
			mmtpMessage, err = readMessage(ctx, c)
			if err != nil {
				fmt.Println("Could not receive message:", err)
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
								}
								subMu.Lock()
								sub, exists := subs[subject]
								if !exists {
									sub = NewSubscription(subject)
									topic, ok := topicHandles[subject]
									if !ok {
										topic, err = pubSub.Join(subject)
										if err != nil {
											panic(err)
										}
										topicHandles[subject] = topic
									}
									sub.AddSubscriber(e)
									sub.Topic = topic
									subscription, err := topic.Subscribe()
									if err != nil {
										panic(err)
									}
									wg.Add(1)
									go handleSubscription(ctx, subscription, p2p, incomingChannel, wg)
									subs[subject] = sub
								} else {
									sub.AddSubscriber(e)
								}
								subMu.Unlock()
								e.Interests = append(e.Interests, subject)

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
							if err = handleUnsubscribe(mmtpMessage, subMu, subs, e, request, c); err != nil {
								fmt.Println("Failed handling Unsubscribe message:", err)
							}
							break
						}
					case mmtp.ProtocolMessageType_SEND_MESSAGE:
						{
							handleSend(mmtpMessage, outgoingChannel, erMu, subMu, subs, e)
							break
						}
					case mmtp.ProtocolMessageType_RECEIVE_MESSAGE:
						{
							if err = handleReceive(mmtpMessage, e, request, c); err != nil {
								fmt.Println("Failed handling Receive message:", err)
							}
							break
						}
					case mmtp.ProtocolMessageType_FETCH_MESSAGE:
						{
							if err = handleFetch(mmtpMessage, e, request, c); err != nil {
								fmt.Println("Failed handling Fetch message:", err)
							}
							break
						}
					case mmtp.ProtocolMessageType_DISCONNECT_MESSAGE:
						{
							if err = handleDisconnect(mmtpMessage, request, c); err != nil {
								fmt.Println("Failed handling Disconnect message:", err)
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

func handleUnsubscribe(mmtpMessage *mmtp.MmtpMessage, subMu *sync.RWMutex, subs map[string]*Subscription, e *EdgeRouter, request *http.Request, c *websocket.Conn) error {
	resp := &mmtp.MmtpMessage{
		MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
		Uuid:    uuid.NewString(),
		Body: &mmtp.MmtpMessage_ResponseMessage{
			ResponseMessage: &mmtp.ResponseMessage{
				ResponseToUuid: mmtpMessage.GetUuid(),
				Response:       mmtp.ResponseEnum_GOOD,
			},
		},
	}
	if unsubscribe := mmtpMessage.GetProtocolMessage().GetUnsubscribeMessage(); unsubscribe != nil {

		subject := unsubscribe.GetSubject()
		if subject == "" {
			reasonText := "Tried to unsubscribe to empty subject"
			resp.GetResponseMessage().Response = mmtp.ResponseEnum_ERROR
			resp.GetResponseMessage().ReasonText = &reasonText
			err := writeMessage(request.Context(), c, resp)
			if err != nil {
				fmt.Printf(reasonText)
				err = fmt.Errorf("could not write response to unsubscribe message: %w", err)
			}
			return err
		}
		subMu.Lock()
		sub, exists := subs[subject]
		if exists {
			sub.DeleteSubscriber(e)
		}
		subMu.Unlock()
		interests := e.Interests
		for i := range interests {
			if strings.EqualFold(subject, interests[i]) {
				interests[i] = interests[len(interests)-1]
				interests[len(interests)-1] = ""
				interests = interests[:len(interests)-1]
				e.Interests = interests
				break
			}
		}
	} else {
		resp.GetResponseMessage().Response = mmtp.ResponseEnum_ERROR
		reasonText := "Message does not contain a Unsubscribe message"
		resp.GetResponseMessage().ReasonText = &reasonText
	}

	err := writeMessage(request.Context(), c, resp)
	if err != nil {
		err = fmt.Errorf("could not write response to unsubscribe message: %w", err)
	}

	return err
}

func handleSend(mmtpMessage *mmtp.MmtpMessage, outgoingChannel chan<- *mmtp.MmtpMessage, erMu *sync.RWMutex, subMu *sync.RWMutex, subs map[string]*Subscription, e *EdgeRouter) {
	if send := mmtpMessage.GetProtocolMessage().GetSendMessage(); send != nil {
		outgoingChannel <- mmtpMessage
		header := send.GetApplicationMessage().GetHeader()
		if len(header.GetRecipients().GetRecipients()) > 0 {
			erMu.RLock()
			for _, recipient := range header.GetRecipients().Recipients {
				subMu.RLock()
				sub, exists := subs[recipient]
				subMu.RUnlock()
				if exists {
					sub.subsMu.RLock()
					for _, er := range sub.Subscribers {
						if er.Mrn != e.Mrn { // Do not send the message back to where it came from
							if err := er.QueueMessage(mmtpMessage); err != nil {
								fmt.Println("Could not queue message to Edge Router:", err)
							}
						}
					}
					sub.subsMu.RUnlock()
				}
			}
			erMu.RUnlock()
		} else if header.GetSubject() != "" {
			subMu.RLock()
			sub, exists := subs[header.GetSubject()]
			if exists {
				for _, subscriber := range sub.Subscribers {
					if subscriber.Mrn != e.Mrn {
						if err := subscriber.QueueMessage(mmtpMessage); err != nil {
							fmt.Println("Could not queue message to Edge Router:", err)
						}
					}
				}
			}
			subMu.RUnlock()
		}
	}
}

func handleReceive(mmtpMessage *mmtp.MmtpMessage, e *EdgeRouter, request *http.Request, c *websocket.Conn) error {
	if receive := mmtpMessage.GetProtocolMessage().GetReceiveMessage(); receive != nil {
		if msgUuids := receive.GetFilter().GetMessageUuids(); msgUuids != nil {
			msgsLen := len(msgUuids)
			mmtpMessages := make([]*mmtp.MmtpMessage, 0, msgsLen)
			appMsgs := make([]*mmtp.ApplicationMessage, 0, msgsLen)
			e.msgMu.Lock()
			for _, msgUuid := range msgUuids {
				msg := e.Messages[msgUuid].GetProtocolMessage().GetSendMessage().GetApplicationMessage()
				appMsgs = append(appMsgs, msg)
				delete(e.Messages, msgUuid)
			}
			e.msgMu.Unlock()
			resp := &mmtp.MmtpMessage{
				MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
				Uuid:    uuid.NewString(),
				Body: &mmtp.MmtpMessage_ResponseMessage{ResponseMessage: &mmtp.ResponseMessage{
					ResponseToUuid:      mmtpMessage.GetUuid(),
					Response:            mmtp.ResponseEnum_GOOD,
					ApplicationMessages: appMsgs,
				}},
			}
			err := writeMessage(request.Context(), c, resp)
			if err != nil {
				e.BulkQueueMessages(mmtpMessages)
				return fmt.Errorf("could not send messages to Edge Router: %w", err)
			}
		} else { // Receive all messages
			e.msgMu.Lock()
			msgsLen := len(e.Messages)
			appMsgs := make([]*mmtp.ApplicationMessage, 0, msgsLen)
			for _, mmtpMsg := range e.Messages {
				msg := mmtpMsg.GetProtocolMessage().GetSendMessage().GetApplicationMessage()
				appMsgs = append(appMsgs, msg)
			}
			resp := &mmtp.MmtpMessage{
				MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
				Uuid:    uuid.NewString(),
				Body: &mmtp.MmtpMessage_ResponseMessage{ResponseMessage: &mmtp.ResponseMessage{
					ResponseToUuid:      mmtpMessage.GetUuid(),
					Response:            mmtp.ResponseEnum_GOOD,
					ApplicationMessages: appMsgs,
				}},
			}
			defer e.msgMu.Unlock()
			err := writeMessage(request.Context(), c, resp)
			if err != nil {
				return fmt.Errorf("could not send messages to Edge Router: %w", err)
			} else {
				e.Messages = make(map[string]*mmtp.MmtpMessage)
			}
		}
	}
	return nil
}

func handleFetch(mmtpMessage *mmtp.MmtpMessage, e *EdgeRouter, request *http.Request, c *websocket.Conn) error {
	if fetch := mmtpMessage.GetProtocolMessage().GetFetchMessage(); fetch != nil {
		e.msgMu.RLock()
		metadata := make([]*mmtp.MessageMetadata, 0, len(e.Messages))
		for _, msg := range e.Messages {
			msgHeader := msg.GetProtocolMessage().GetSendMessage().GetApplicationMessage().GetHeader()
			msgMetadata := &mmtp.MessageMetadata{
				Uuid:   msg.GetUuid(),
				Header: msgHeader,
			}
			metadata = append(metadata, msgMetadata)
		}
		e.msgMu.RUnlock()
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
		err := writeMessage(request.Context(), c, resp)
		if err != nil {
			return fmt.Errorf("could not send fetch response to Edge Router: %w", err)
		}
	}
	return nil
}

func handleDisconnect(mmtpMessage *mmtp.MmtpMessage, request *http.Request, c *websocket.Conn) error {
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
		if err := writeMessage(request.Context(), c, resp); err != nil {
			return fmt.Errorf("could not send disconnect response to Edge Router: %w", err)
		}

		if err := c.Close(websocket.StatusNormalClosure, "Closed connection after receiving Disconnect message"); err != nil {
			return fmt.Errorf("websocket could not be closed cleanly: %w", err)
		}
	}
	return nil
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

func verifyEdgeRouterCertificate() func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
		// we did not receive a certificate from the client, so we just return early
		if len(rawCerts) == 0 || len(verifiedChains) == 0 {
			return fmt.Errorf("client did not send a valid certificate")
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
			ocspResp, err := ocsp.ParseResponse(respBytes, issuingCert)
			if err != nil {
				return fmt.Errorf("parsing OCSP response failed: %w", err)
			}
			if ocspResp.SerialNumber.Cmp(clientCert.SerialNumber) != 0 {
				return fmt.Errorf("the serial number in the OCSP response does not correspond to the serial number of the certificate being checked")
			}
			if ocspResp.Certificate == nil {
				if err = ocspResp.CheckSignatureFrom(issuingCert); err != nil {
					return fmt.Errorf("the signature on the OCSP response is not valid: %w", err)
				}
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
			for _, rev := range crl.RevokedCertificateEntries {
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

func handleSubscription(ctx context.Context, sub *pubsub.Subscription, host *host.Host, incomingChannel chan<- *mmtp.MmtpMessage, wg *sync.WaitGroup) {
	defer func() {
		wg.Done()
	}()
	for {
		select {
		case <-ctx.Done():
			sub.Cancel()
			return
		default:
			m, err := sub.Next(ctx)
			if err != nil {
				fmt.Println("Could not get message from subscription:", err)
				continue
			}
			if m.GetFrom() != (*host).ID() {
				var mmtpMessage mmtp.MmtpMessage
				if err = proto.Unmarshal(m.Data, &mmtpMessage); err != nil {
					fmt.Println("Could not unmarshal received message as an mmtp message:", err)
					continue
				}
				uid, err := uuid.Parse(mmtpMessage.GetUuid())
				if err != nil || uid.Version() != 4 {
					fmt.Println("The UUID of the message is not a valid version 4 UUID")
					continue
				}
				switch mmtpMessage.GetMsgType() {
				case mmtp.MsgType_PROTOCOL_MESSAGE:
					{
						if sendMsg := mmtpMessage.GetProtocolMessage().GetSendMessage(); sendMsg != nil {
							incomingChannel <- &mmtpMessage
						}
						break
					}
				case mmtp.MsgType_RESPONSE_MESSAGE:
				default:
					continue
				}
			}
		}
	}
}

func handleIncomingMessages(ctx context.Context, router *MMSRouter, wg *sync.WaitGroup) {
	defer func() {
		wg.Done()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		default:
			for incomingMessage := range router.incomingChannel {
				switch incomingMessage.GetMsgType() {
				case mmtp.MsgType_PROTOCOL_MESSAGE:
					{
						appMsg := incomingMessage.GetProtocolMessage().GetSendMessage().GetApplicationMessage()
						if appMsg == nil {
							continue
						}
						switch subjectOrRecipient := appMsg.GetHeader().GetSubjectOrRecipient().(type) {
						case *mmtp.ApplicationMessageHeader_Subject:
							{
								router.subMu.RLock()
								for _, subscriber := range router.subscriptions[subjectOrRecipient.Subject].Subscribers {
									if err := subscriber.QueueMessage(incomingMessage); err != nil {
										fmt.Println("Could not queue message:", err)
										continue
									}
								}
								router.subMu.RUnlock()
							}
						case *mmtp.ApplicationMessageHeader_Recipients:
							{
								for _, recipient := range subjectOrRecipient.Recipients.GetRecipients() {
									router.subMu.RLock()
									sub := router.subscriptions[recipient]
									for _, er := range sub.Subscribers {
										err := er.QueueMessage(incomingMessage)
										if err != nil {
											fmt.Println("Could not queue message for Edge Router:", err)
										}
									}
									router.subMu.RUnlock()
								}
							}
						}
					}
				default:
					continue
				}
			}
		}
	}
}

func handleOutgoingMessages(ctx context.Context, router *MMSRouter, wg *sync.WaitGroup) {
	defer func() {
		wg.Done()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		default:
			for outgoingMessage := range router.outgoingChannel {
				switch outgoingMessage.GetMsgType() {
				case mmtp.MsgType_PROTOCOL_MESSAGE:
					{
						appMsg := outgoingMessage.GetProtocolMessage().GetSendMessage().GetApplicationMessage()
						if appMsg == nil {
							continue
						}
						msgBytes, err := proto.Marshal(outgoingMessage)
						if err != nil {
							fmt.Println("Could not marshal outgoing message:", err)
							continue
						}
						switch subjectOrRecipient := appMsg.GetHeader().GetSubjectOrRecipient().(type) {
						case *mmtp.ApplicationMessageHeader_Subject:
							{
								topic, ok := router.topicHandles[subjectOrRecipient.Subject]
								if !ok {
									topic, err = router.pubSub.Join(subjectOrRecipient.Subject)
									if err != nil {
										fmt.Println("Could not join topic:", err)
										continue
									}
									router.topicHandles[subjectOrRecipient.Subject] = topic
								}
								err = topic.Publish(ctx, msgBytes)
								if err != nil {
									fmt.Println("Could not public message to topic:", err)
									continue
								}
							}
						case *mmtp.ApplicationMessageHeader_Recipients:
							{
								for _, recipient := range subjectOrRecipient.Recipients.GetRecipients() {
									topic, ok := router.topicHandles[recipient]
									if !ok {
										topic, err = router.pubSub.Join(recipient)
										if err != nil {
											fmt.Println("Could not join topic:", err)
											continue
										}
										router.topicHandles[recipient] = topic
									}
									err = topic.Publish(ctx, msgBytes)
									if err != nil {
										fmt.Println("Could not public message to topic:", err)
										continue
									}
								}
							}
						}
					}
				default:
					continue
				}
			}
		}
	}
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())

	listeningPort := flag.Int("port", 8080, "The port number that this Router should listen on.")
	libp2pPort := flag.Int("libp2p-port", 0, "The port number that this Router should use to "+
		"open up to the Router Network. If not set, a random port is chosen.")
	privKeyFilePath := flag.String("privkey", "", "Path to a file containing a private key. If none is provided, a new private key will be generated every time the program is run.")
	certPath := flag.String("cert-path", "", "Path to a TLS certificate file. If none is provided, TLS will be disabled.")
	certKeyPath := flag.String("cert-key-path", "", "Path to a TLS certificate private key. If none is provided, TLS will be disabled.")
	clientCAs := flag.String("client-ca", "", "Path to a file containing a list of client CAs that can connect to this Router.")

	flag.Parse()

	node, rd, err := setupLibP2P(ctx, libp2pPort, privKeyFilePath)
	if err != nil {
		fmt.Println("Could not setup the libp2p backend:", err)
		return
	}

	pubSub, err := pubsub.NewGossipSub(ctx, node)
	if err != nil {
		panic(err)
	}

	// Look for others who have announced and attempt to connect to them
	anyConnected := false
	attempts := 0
	for !anyConnected && attempts < 10 {
		fmt.Println("Searching for peers...")
		peerChan, err := rd.FindPeers(ctx, "over here")
		if err != nil {
			panic(err)
		}
		for p := range peerChan {
			if p.ID == node.ID() {
				continue // No self connection
			}
			fmt.Println("Peer:", p)
			err := node.Connect(ctx, p)
			if err != nil {
				fmt.Println("Failed connecting to ", p.ID.String(), ", error:", err)
			} else {
				fmt.Println("Connected to:", p.ID.String())
				anyConnected = true
			}
		}
		attempts++
		time.Sleep(2 * time.Second)
	}
	fmt.Println("Peer discovery complete")

	incomingChannel := make(chan *mmtp.MmtpMessage)
	outgoingChannel := make(chan *mmtp.MmtpMessage)

	wg := &sync.WaitGroup{}

	router, err := NewMMSRouter(&node, pubSub, ":"+strconv.Itoa(*listeningPort), incomingChannel, outgoingChannel, ctx, wg, clientCAs)
	if err != nil {
		fmt.Println("Could not create MMS Router instance:", err)
		return
	}

	wg.Add(1)
	go router.StartRouter(ctx, wg, certPath, certKeyPath)

	// wait for a SIGINT or SIGTERM signal
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
	fmt.Println("Received signal, shutting down...")

	cancel()
	wg.Wait()
	// shut the libp2p node down
	if err = node.Close(); err != nil {
		fmt.Println("libp2p node could not be shut down correctly")
	}
}

func setupLibP2P(ctx context.Context, libp2pPort *int, privKeyFilePath *string) (host.Host, *drouting.RoutingDiscovery, error) {
	port := *libp2pPort
	addrStrings := make([]string, 2)
	if port != 0 {
		addrStrings[0] = fmt.Sprintf("/ip4/0.0.0.0/udp/%d/quic-v1", port)
		addrStrings[1] = fmt.Sprintf("/ip6/::/udp/%d/quic-v1", port)
	} else {
		addrStrings[0] = "/ip4/0.0.0.0/udp/0/quic-v1"
		addrStrings[1] = "/ip6/::/udp/0/quic-v1"
	}
	// TODO make the router discover its public IP address so it can be published

	beacons := make([]peerstore.AddrInfo, 0, 1)
	beaconsFile, err := os.Open("beacons.txt")
	if err == nil {
		fileScanner := bufio.NewScanner(beaconsFile)
		for fileScanner.Scan() {
			addrInfo, err := peerstore.AddrInfoFromString(fileScanner.Text())
			if err != nil {
				fmt.Println("Failed to parse beacon address:", err)
				continue
			}
			beacons = append(beacons, *addrInfo)
		}
	}

	identify.ActivationThresh = 3

	var node host.Host
	if *privKeyFilePath != "" {
		privKeyFile, err := os.ReadFile(*privKeyFilePath)
		if err != nil {
			return nil, nil, fmt.Errorf("could not open the provided private key file: %w", err)
		}
		keyData, _ := pem.Decode(privKeyFile)
		privKey, err := x509.ParseECPrivateKey(keyData.Bytes)
		if err != nil {
			return nil, nil, fmt.Errorf("could not parse the provided private key file as an ECDSA key: %w", err)
		}

		privEc, _, err := crypto.ECDSAKeyPairFromKey(privKey)
		if err != nil {
			return nil, nil, fmt.Errorf("could not parse the ECDSA private key from the file: %w", err)
		}

		// start a libp2p node with the given private key
		node, err = libp2p.New(
			libp2p.ListenAddrStrings(addrStrings...),
			libp2p.Identity(privEc),
			libp2p.EnableNATService(),
			libp2p.EnableRelayService(),
			libp2p.EnableAutoRelayWithStaticRelays(beacons),
			libp2p.EnableHolePunching(),
		)
		if err != nil {
			return nil, nil, fmt.Errorf("could not create a libp2p node: %w", err)
		}
	} else {
		var err error
		// start a libp2p node with default settings
		node, err = libp2p.New(
			libp2p.ListenAddrStrings(addrStrings...),
			libp2p.EnableNATService(),
			libp2p.EnableRelayService(),
			libp2p.EnableAutoRelayWithStaticRelays(beacons),
			libp2p.EnableHolePunching(),
		)
		if err != nil {
			return nil, nil, fmt.Errorf("could not create a libp2p node: %w", err)
		}
	}

	kademlia, err := dht.New(ctx, node, dht.Mode(dht.ModeAutoServer), dht.BootstrapPeers(beacons...))
	if err != nil {
		panic(err)
	}

	if err = kademlia.Bootstrap(ctx); err != nil {
		panic(err)
	}

	rd := drouting.NewRoutingDiscovery(kademlia)

	dutil.Advertise(ctx, rd, "over here")

	// print the node's PeerInfo in multiaddr format
	peerInfo := peerstore.AddrInfo{
		ID:    node.ID(),
		Addrs: node.Addrs(),
	}
	addrs, err := peerstore.AddrInfoToP2pAddrs(&peerInfo)
	fmt.Println("libp2p node addresses:", addrs)
	return node, rd, nil
}
