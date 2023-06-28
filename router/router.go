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
	"fmt"
	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
	peerstore "github.com/libp2p/go-libp2p/core/peer"
	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	dutil "github.com/libp2p/go-libp2p/p2p/discovery/util"
	"golang.org/x/crypto/ocsp"
	"google.golang.org/protobuf/proto"
	"io"
	"maritimeconnectivity.net/mms-router/generated/mmtp"
	"net/http"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wspb"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"
)

// EdgeRouter type representing a connected Edge Router
type EdgeRouter struct {
	Mrn       string                       // the MRN of the EdgeRouter
	Interests []string                     // the Interests that the EdgeRouter wants to subscribe to
	Messages  map[string]*mmtp.MmtpMessage // the incoming messages for this EdgeRouter
	msgMu     *sync.RWMutex                // RWMutex for locking the Messages map
	Ws        *websocket.Conn              // the websocket connection to this EdgeRouter
}

func (er *EdgeRouter) QueueMessage(mmtpMessage *mmtp.MmtpMessage) error {
	uUid := mmtpMessage.GetUuid()
	if uUid == "" {
		return fmt.Errorf("the message does not contain a UUID")
	}
	er.msgMu.Lock()
	er.Messages[uUid] = mmtpMessage
	er.msgMu.Unlock()
	return nil
}

func (er *EdgeRouter) BulkQueueMessages(mmtpMessages []*mmtp.MmtpMessage) {
	er.msgMu.Lock()
	for _, message := range mmtpMessages {
		er.Messages[message.Uuid] = message
	}
	er.msgMu.Unlock()
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
	ctx             context.Context          // the main Context of the EdgeRouter
}

func NewMMSRouter(p2p *host.Host, pubSub *pubsub.PubSub, listeningAddr string, incomingChannel chan *mmtp.MmtpMessage, outgoingChannel chan *mmtp.MmtpMessage, ctx context.Context) *MMSRouter {
	subs := make(map[string]*Subscription)
	mu := &sync.RWMutex{}
	edgeRouters := make(map[string]*EdgeRouter)
	erMu := &sync.RWMutex{}
	topicHandles := make(map[string]*pubsub.Topic)
	httpServer := http.Server{
		Addr:    listeningAddr,
		Handler: handleHttpConnection(p2p, pubSub, incomingChannel, outgoingChannel, subs, mu, edgeRouters, erMu, topicHandles, ctx),
		TLSConfig: &tls.Config{
			ClientAuth:            tls.RequireAndVerifyClientCert,
			ClientCAs:             nil, // this should come from a file containing the CAs we trust
			MinVersion:            tls.VersionTLS13,
			VerifyPeerCertificate: verifyEdgeRouterCertificate(),
		},
	}

	return &MMSRouter{
		subscriptions:   subs,
		subMu:           mu,
		edgeRouters:     edgeRouters,
		erMu:            erMu,
		httpServer:      &httpServer,
		p2pHost:         p2p,
		pubSub:          pubSub,
		topicHandles:    topicHandles,
		incomingChannel: incomingChannel,
		outgoingChannel: outgoingChannel,
		ctx:             ctx,
	}
}

func (r *MMSRouter) StartRouter(ctx context.Context) {
	go func() {
		fmt.Println("Starting MMS Router")
		if err := r.httpServer.ListenAndServe(); err != nil {
			fmt.Println(err)
		}
	}()
	go handleIncomingMessages(ctx, r)
	go handleOutgoingMessages(ctx, r)
	<-ctx.Done()
	fmt.Println("Shutting down MMS router")
	close(r.incomingChannel)
	close(r.outgoingChannel)
	if err := r.httpServer.Shutdown(ctx); err != nil {
		panic(err)
	}
}

func handleHttpConnection(p2p *host.Host, pubSub *pubsub.PubSub, incomingChannel chan *mmtp.MmtpMessage, outgoingChannel chan<- *mmtp.MmtpMessage, subs map[string]*Subscription, mu *sync.RWMutex, edgeRouters map[string]*EdgeRouter, erMu *sync.RWMutex, topicHandles map[string]*pubsub.Topic, ctx context.Context) http.HandlerFunc {
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

		var mmtpMessage mmtp.MmtpMessage
		err = wspb.Read(request.Context(), c, &mmtpMessage)
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

		erMrn := connect.GetOwnMrn()
		if erMrn == "" {
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
		//			if !strings.EqualFold(v, erMrn) {
		//				if err = c.Close(websocket.StatusUnsupportedData, "The MRN given in the Connect message does not match the one in the certificate that was used for authentication"); err != nil {
		//					fmt.Println(err)
		//				}
		//				return
		//			}
		//		}
		//	}
		//}

		e := &EdgeRouter{
			Mrn:       erMrn,
			Interests: make([]string, 0, 1),
			Messages:  make(map[string]*mmtp.MmtpMessage),
			msgMu:     &sync.RWMutex{},
			Ws:        c,
		}

		erMu.Lock()
		edgeRouters[e.Mrn] = e
		erMu.Unlock()

		resp := mmtp.MmtpMessage{
			MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
			Uuid:    uuid.NewString(),
			Body: &mmtp.MmtpMessage_ResponseMessage{
				ResponseMessage: &mmtp.ResponseMessage{
					ResponseToUuid: mmtpMessage.GetUuid(),
					Response:       mmtp.ResponseEnum_GOOD,
				}}}
		err = wspb.Write(request.Context(), c, &resp)
		if err != nil {
			fmt.Println(err)
			return
		}

		for {
			err = wspb.Read(request.Context(), c, &mmtpMessage)
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
								mu.Lock()
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
									go handleSubscription(ctx, subscription, p2p, incomingChannel)
									subs[subject] = sub
								} else {
									sub.AddSubscriber(e)
								}
								mu.Unlock()
								e.Interests = append(e.Interests, subject)

								resp = mmtp.MmtpMessage{
									MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
									Uuid:    uuid.NewString(),
									Body: &mmtp.MmtpMessage_ResponseMessage{
										ResponseMessage: &mmtp.ResponseMessage{
											ResponseToUuid: mmtpMessage.GetUuid(),
											Response:       mmtp.ResponseEnum_GOOD,
										}},
								}
								if err = wspb.Write(request.Context(), c, &resp); err != nil {
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
								mu.Lock()
								sub, exists := subs[subject]
								if !exists {
									continue
								}
								sub.DeleteSubscriber(e)
								mu.Unlock()
								interests := e.Interests
								for i := range interests {
									if strings.EqualFold(subject, interests[i]) {
										interests[i] = interests[len(interests)-1]
										interests[len(interests)-1] = ""
										interests = interests[:len(interests)-1]
										e.Interests = interests
									}
								}
								resp = mmtp.MmtpMessage{
									MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
									Uuid:    uuid.NewString(),
									Body: &mmtp.MmtpMessage_ResponseMessage{
										ResponseMessage: &mmtp.ResponseMessage{
											ResponseToUuid: mmtpMessage.GetUuid(),
											Response:       mmtp.ResponseEnum_GOOD,
										}},
								}
								if err = wspb.Write(request.Context(), c, &resp); err != nil {
									fmt.Println("Could not write response to unsubscribe message:", err)
								}
							}
							break
						}
					case mmtp.ProtocolMessageType_SEND_MESSAGE:
						{
							if send := protoMessage.GetSendMessage(); send != nil {
								outgoingChannel <- &mmtpMessage

								resp = mmtp.MmtpMessage{
									MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
									Uuid:    uuid.NewString(),
									Body: &mmtp.MmtpMessage_ResponseMessage{
										ResponseMessage: &mmtp.ResponseMessage{
											ResponseToUuid: mmtpMessage.GetUuid(),
											Response:       mmtp.ResponseEnum_GOOD,
										}},
								}
								if err = wspb.Write(request.Context(), c, &resp); err != nil {
									fmt.Println("Could not send Send response to Edge Router:", err)
								}
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
									e.msgMu.Lock()
									for _, msgUuid := range msgUuids {
										msg := e.Messages[msgUuid].GetProtocolMessage().GetSendMessage().GetApplicationMessage()
										appMsgs = append(appMsgs, msg)
										delete(e.Messages, msgUuid)
									}
									e.msgMu.Unlock()
									resp = mmtp.MmtpMessage{
										MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
										Uuid:    uuid.NewString(),
										Body: &mmtp.MmtpMessage_ResponseMessage{ResponseMessage: &mmtp.ResponseMessage{
											ResponseToUuid:      mmtpMessage.GetUuid(),
											Response:            mmtp.ResponseEnum_GOOD,
											ApplicationMessages: appMsgs,
										}},
									}
									err = wspb.Write(request.Context(), c, &resp)
									if err != nil {
										fmt.Println("Could not send messages to Edge Router:", err)
										e.BulkQueueMessages(mmtpMessages)
									}
								}
							}
							break
						}
					case mmtp.ProtocolMessageType_FETCH_MESSAGE:
						{
							if fetch := protoMessage.GetFetchMessage(); fetch != nil {
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
								resp = mmtp.MmtpMessage{
									MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
									Uuid:    uuid.NewString(),
									Body: &mmtp.MmtpMessage_ResponseMessage{
										ResponseMessage: &mmtp.ResponseMessage{
											ResponseToUuid:  mmtpMessage.GetUuid(),
											Response:        mmtp.ResponseEnum_GOOD,
											MessageMetadata: metadata,
										}},
								}
								err = wspb.Write(request.Context(), c, &resp)
								if err != nil {
									fmt.Println("Could not send fetch response to Edge Router:", err)
								}
							}
							break
						}
					case mmtp.ProtocolMessageType_DISCONNECT_MESSAGE:
						{
							if disconnect := protoMessage.GetDisconnectMessage(); disconnect != nil {
								resp = mmtp.MmtpMessage{
									MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
									Uuid:    uuid.NewString(),
									Body: &mmtp.MmtpMessage_ResponseMessage{
										ResponseMessage: &mmtp.ResponseMessage{
											ResponseToUuid: mmtpMessage.GetUuid(),
											Response:       mmtp.ResponseEnum_GOOD,
										}},
								}
								if err = wspb.Write(request.Context(), c, &resp); err != nil {
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
							resp = mmtp.MmtpMessage{
								MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
								Uuid:    uuid.NewString(),
								Body: &mmtp.MmtpMessage_ResponseMessage{
									ResponseMessage: &mmtp.ResponseMessage{
										ResponseToUuid: mmtpMessage.GetUuid(),
										Response:       mmtp.ResponseEnum_ERROR,
										ReasonText:     &reason,
									}},
							}
							if err = wspb.Write(request.Context(), c, &resp); err != nil {
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

func handleSubscription(ctx context.Context, sub *pubsub.Subscription, host *host.Host, incomingChannel chan<- *mmtp.MmtpMessage) {
	for {
		select {
		case <-ctx.Done():
			sub.Cancel()
			break
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

func handleIncomingMessages(ctx context.Context, router *MMSRouter) {
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
								router.erMu.RLock()
								for _, recipient := range subjectOrRecipient.Recipients.GetRecipients() {
									err := router.edgeRouters[recipient].QueueMessage(incomingMessage)
									if err != nil {
										fmt.Println("Could not queue message for Edge Router:", err)
									}
								}
								router.erMu.RUnlock()
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

func handleOutgoingMessages(ctx context.Context, router *MMSRouter) {
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
	defer cancel()

	node, rd := setupLibP2P(ctx)

	pubSub, err := pubsub.NewGossipSub(ctx, node)
	if err != nil {
		panic(err)
	}

	// Look for others who have announced and attempt to connect to them
	anyConnected := false
	attempts := 0
	for !anyConnected && attempts < 20 {
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

	router := NewMMSRouter(&node, pubSub, "0.0.0.0:8080", incomingChannel, outgoingChannel, ctx)
	go router.StartRouter(ctx)

	// wait for a SIGINT or SIGTERM signal
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	<-ch
	fmt.Println("Received signal, shutting down...")

	// shut the node down
	if err := node.Close(); err != nil {
		fmt.Println("libp2p node could not be shut down correctly")
	}
}

func setupLibP2P(ctx context.Context) (host.Host, *drouting.RoutingDiscovery) {
	// start a libp2p node with default settings
	node, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/0.0.0.0/udp/0/quic-v1", "/ip6/::/udp/0/quic-v1"))
	if err != nil {
		panic(err)
	}

	// print the node's listening addresses
	fmt.Println("Listen addresses:", node.Addrs())

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
	return node, rd
}
