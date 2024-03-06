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
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/libp2p/zeroconf/v2"
	"github.com/maritimeconnectivity/MMS/mmtp"
	"golang.org/x/crypto/ocsp"
	"google.golang.org/protobuf/proto"
	"nhooyr.io/websocket"
)

const (
	WsReadLimit      int64 = 1 << 20        // 1 MiB = 1048576 B
	MessageSizeLimit int   = 50 * (1 << 10) // 50 KiB = 51200 B
)

// Agent type representing a connected Edge Router
type Agent struct {
	Mrn            string                       // the MRN of the Agent
	Interests      []string                     // the Interests that the Agent wants to subscribe to
	Messages       map[string]*mmtp.MmtpMessage // the incoming messages for this Agent
	msgMu          *sync.RWMutex                // RWMutex for locking the Messages map
	reconnectToken string                       // token for reconnecting to a previous session
	agentUuid      string                       // UUID for uniquely identifying this Agent
	directMessages bool                         // bool indicating whether the Agent is subscribing to direct messages
}

func (a *Agent) QueueMessage(mmtpMessage *mmtp.MmtpMessage) error {
	if a != nil {
		uUid := mmtpMessage.GetUuid()
		if uUid == "" {
			return fmt.Errorf("the message does not contain a UUID")
		}
		a.msgMu.Lock()
		a.Messages[uUid] = mmtpMessage
		a.msgMu.Unlock()
	}
	return nil
}

func (a *Agent) BulkQueueMessages(mmtpMessages []*mmtp.MmtpMessage) {
	if a != nil {
		a.msgMu.Lock()
		for _, message := range mmtpMessages {
			a.Messages[message.Uuid] = message
		}
		a.msgMu.Unlock()
	}
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
	sub.Subscribers[agent.agentUuid] = agent
	sub.subsMu.Unlock()
}

func (sub *Subscription) DeleteSubscriber(agent *Agent) {
	sub.subsMu.Lock()
	delete(sub.Subscribers, agent.agentUuid)
	sub.subsMu.Unlock()
}

// EdgeRouter type representing an MMS edge router
type EdgeRouter struct {
	ownMrn          string                   // The MRN of this EdgeRouter
	subscriptions   map[string]*Subscription // a mapping from Interest names to Subscription slices
	subMu           *sync.RWMutex            // a Mutex for locking the subscriptions map
	agents          map[string]*Agent        // a map of connected Agents
	agentsMu        *sync.RWMutex            // a Mutex for locking the agents map
	mrnToAgent      map[string]*Agent        // a mapping from an Agent MRN to a UUID
	mrnToAgentMu    *sync.RWMutex            // a Mutex for locking the mrnToAgent map
	httpServer      *http.Server             // the http server that is used to bootstrap websocket connections
	outgoingChannel chan *mmtp.MmtpMessage   // channel for outgoing messages
	routerWs        *websocket.Conn          // the websocket connection to the MMS Router
	routerConnMu    *sync.Mutex              // a Mutex for taking a hold on the connection to the router
}

func NewEdgeRouter(listeningAddr string, mrn string, outgoingChannel chan *mmtp.MmtpMessage, routerWs *websocket.Conn, ctx context.Context, wg *sync.WaitGroup, clientCAs *string) (*EdgeRouter, error) {
	subs := make(map[string]*Subscription)
	subMu := &sync.RWMutex{}
	agents := make(map[string]*Agent)
	agentsMu := &sync.RWMutex{}
	mrnToAgent := make(map[string]*Agent)
	mrnToAgentMu := &sync.RWMutex{}

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
		Handler: handleHttpConnection(outgoingChannel, subs, subMu, agents, agentsMu, mrnToAgent, mrnToAgentMu, ctx, wg),
		TLSConfig: &tls.Config{
			ClientAuth:            tls.VerifyClientCertIfGiven,
			ClientCAs:             certPool,
			MinVersion:            tls.VersionTLS12,
			VerifyPeerCertificate: verifyAgentCertificate(),
		},
	}

	return &EdgeRouter{
		ownMrn:          mrn,
		subscriptions:   subs,
		subMu:           subMu,
		agents:          agents,
		agentsMu:        agentsMu,
		mrnToAgent:      mrnToAgent,
		mrnToAgentMu:    mrnToAgentMu,
		httpServer:      &httpServer,
		outgoingChannel: outgoingChannel,
		routerWs:        routerWs,
		routerConnMu:    &sync.Mutex{},
	}, nil
}

func (er *EdgeRouter) StartEdgeRouter(ctx context.Context, wg *sync.WaitGroup, certPath *string, certKeyPath *string) {
	defer func() {
		close(er.outgoingChannel)
		wg.Done()
	}()

	log.Println("Starting edge router")

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
	err := writeMessage(ctx, er.routerWs, connect)
	if err != nil {
		log.Println("Could not send connect message:", err)
		return
	}

	response, _, err := readMessage(ctx, er.routerWs)
	if err != nil {
		log.Println("Something went wrong while receiving response from MMS Router:", err)
		return
	}

	connectResp := response.GetResponseMessage()
	if connectResp.Response != mmtp.ResponseEnum_GOOD {
		log.Println("MMS Router did not accept Connect:", connectResp.GetReasonText())
		return
	}

	// TODO store reconnect token and handle reconnection in case of disconnect

	wg.Add(4)
	go func() {
		log.Println("Websocket listening on:", er.httpServer.Addr)
		if *certPath != "" && *certKeyPath != "" {
			if err := er.httpServer.ListenAndServeTLS(*certPath, *certKeyPath); err != nil {
				log.Println(err)
			}
		} else {
			if err := er.httpServer.ListenAndServe(); err != nil {
				log.Println(err)
			}
			wg.Done()
		}
	}()

	go handleIncomingMessages(ctx, er, wg)
	go handleOutgoingMessages(ctx, er, wg)
	go er.messageGC(ctx, wg)

	<-ctx.Done()
	log.Println("Shutting down edge router")

	disconnectMsg := &mmtp.MmtpMessage{
		MsgType: mmtp.MsgType_PROTOCOL_MESSAGE,
		Uuid:    uuid.NewString(),
		Body: &mmtp.MmtpMessage_ProtocolMessage{
			ProtocolMessage: &mmtp.ProtocolMessage{
				ProtocolMsgType: mmtp.ProtocolMessageType_DISCONNECT_MESSAGE,
				Body: &mmtp.ProtocolMessage_DisconnectMessage{
					DisconnectMessage: &mmtp.Disconnect{},
				},
			},
		},
	}
	er.routerConnMu.Lock()
	if err = writeMessage(context.Background(), er.routerWs, disconnectMsg); err != nil {
		log.Println("Could not send disconnect to Router:", err)
	}

	response, _, err = readMessage(context.Background(), er.routerWs)
	if err != nil || response.GetResponseMessage().Response != mmtp.ResponseEnum_GOOD {
		log.Println("Graceful disconnect from Router failed")
	}

	if err = er.httpServer.Shutdown(context.Background()); err != nil {
		log.Println(err)
	}
	er.routerConnMu.Unlock()
}

// Function for garbage collection of expired messages
func (er *EdgeRouter) messageGC(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Minute): // run every 10 minutes
			er.agentsMu.RLock()
			now := time.Now().UTC()
			for _, a := range er.agents {
				a.msgMu.Lock()
				for _, m := range a.Messages {
					expires := m.GetProtocolMessage().GetSendMessage().GetApplicationMessage().GetHeader().GetExpires()
					if (expires <= 0) || now.After(time.UnixMilli(expires)) {
						delete(a.Messages, m.Uuid)
					}
				}
				a.msgMu.Unlock()
			}
			er.agentsMu.RUnlock()
		}
	}
}

func handleHttpConnection(outgoingChannel chan<- *mmtp.MmtpMessage, subs map[string]*Subscription, subMu *sync.RWMutex, agents map[string]*Agent, agentsMu *sync.RWMutex, mrnToAgent map[string]*Agent, mrnToAgentMu *sync.RWMutex, ctx context.Context, wg *sync.WaitGroup) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		wg.Add(1)
		c, err := websocket.Accept(writer, request, &websocket.AcceptOptions{OriginPatterns: []string{"*"}, CompressionMode: websocket.CompressionContextTakeover})
		if err != nil {
			log.Println("Could not establish websocket connection", err)
			wg.Done()
			return
		}
		defer func(c *websocket.Conn, code websocket.StatusCode, reason string) {
			err := c.Close(code, reason)
			if err != nil {
				log.Println("Could not close connection:", err)
			}
			wg.Done()
		}(c, websocket.StatusInternalError, "PANIC!!!")

		// Set the read limit to 1 MiB instead of 32 KiB
		c.SetReadLimit(WsReadLimit)

		mmtpMessage, _, err := readMessage(ctx, c)
		if err != nil {
			log.Println(err)
			return
		}

		protoMessage := mmtpMessage.GetProtocolMessage()
		if mmtpMessage.MsgType != mmtp.MsgType_PROTOCOL_MESSAGE || protoMessage == nil {
			if err = c.Close(websocket.StatusUnsupportedData, "The first message needs to be a Protocol Message containing a Connect message"); err != nil {
				log.Println(err)
			}
			return
		}

		connect := protoMessage.GetConnectMessage()
		if connect == nil {
			if err = c.Close(websocket.StatusUnsupportedData, "The first message needs to contain a Connect message"); err != nil {
				log.Println(err)
			}
			return
		}

		agentMrn := connect.GetOwnMrn()
		agentMrn = strings.ToLower(agentMrn)

		var signatureAlgorithm x509.SignatureAlgorithm

		// If TLS is enabled, we should verify the certificate from the Agent
		if request.TLS != nil && agentMrn != "" {
			uidOid := []int{0, 9, 2342, 19200300, 100, 1, 1}

			if len(request.TLS.PeerCertificates) < 1 {
				if err = c.Close(websocket.StatusPolicyViolation, "A valid client certificate must be provided for authenticated connections"); err != nil {
					log.Println(err)
				}
				return
			}

			pubKeyLen := 0

			switch pubKey := request.TLS.PeerCertificates[0].PublicKey.(type) {
			case *ecdsa.PublicKey:
				if pubKeyLen = pubKey.Params().BitSize; pubKeyLen < 256 {
					if err = c.Close(websocket.StatusPolicyViolation, "The public key length of the provided client certificate cannot be less than 256 bits"); err != nil {
						log.Println(err)
					}
					return
				}
				break
			default:
				if err = c.Close(websocket.StatusPolicyViolation, "The provided client certificate does not use an allowed public key algorithm"); err != nil {
					log.Println(err)
				}
				return
			}

			switch pubKeyLen {
			case 256:
				signatureAlgorithm = x509.ECDSAWithSHA256
				break
			case 384:
				signatureAlgorithm = x509.ECDSAWithSHA384
				break
			case 512:
				signatureAlgorithm = x509.ECDSAWithSHA512
				break
			default:
				if err = c.Close(websocket.StatusPolicyViolation, "The public key length of the provided client certificate is not supported"); err != nil {
					log.Println(err)
				}
				return
			}

			// https://stackoverflow.com/a/50640119
			for _, n := range request.TLS.PeerCertificates[0].Subject.Names {
				if n.Type.Equal(uidOid) {
					if v, ok := n.Value.(string); ok {
						if !strings.EqualFold(v, agentMrn) {
							if err = c.Close(websocket.StatusUnsupportedData, "The MRN given in the Connect message does not match the one in the certificate that was used for authentication"); err != nil {
								log.Println(err)
							}
							return
						}
					}
				}
			}
		}

		var agent *Agent
		if connect.ReconnectToken != nil {
			agentsMu.RLock()
			agent = agents[connect.GetReconnectToken()]
			agentsMu.RUnlock()
			if agent == nil {
				errorMsg := "No existing session was found for the given reconnect token"
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
					log.Println("Could not send error message:", err)
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
					log.Println("Could not send error message:", err)
					return
				}
			}
			agentsMu.Lock()
			delete(agents, agent.reconnectToken)
			agentsMu.Unlock()
		} else {
			agent = &Agent{
				Mrn:       agentMrn,
				Interests: make([]string, 0, 1),
				Messages:  make(map[string]*mmtp.MmtpMessage),
				msgMu:     &sync.RWMutex{},
				agentUuid: uuid.NewString(),
			}
			if agentMrn != "" {
				mrnToAgentMu.Lock()
				mrnToAgent[agentMrn] = agent
				mrnToAgentMu.Unlock()
			}
		}

		agent.reconnectToken = uuid.NewString()

		agentsMu.Lock()
		agents[agent.reconnectToken] = agent
		agentsMu.Unlock()

		resp := &mmtp.MmtpMessage{
			MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
			Uuid:    uuid.NewString(),
			Body: &mmtp.MmtpMessage_ResponseMessage{
				ResponseMessage: &mmtp.ResponseMessage{
					ResponseToUuid: mmtpMessage.GetUuid(),
					ReconnectToken: &agent.reconnectToken,
					Response:       mmtp.ResponseEnum_GOOD,
				}},
		}
		err = writeMessage(request.Context(), c, resp)
		if err != nil {
			log.Println("Could not send response:", err)
			return
		}

		for {
			mmtpMessage, n, err := readMessage(ctx, c)
			if err != nil {
				log.Println("Something went wrong while reading message from Agent:", err)
				reasonText := "Message could not be correctly parsed"
				resp = &mmtp.MmtpMessage{
					MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
					Uuid:    uuid.NewString(),
					Body: &mmtp.MmtpMessage_ResponseMessage{
						ResponseMessage: &mmtp.ResponseMessage{
							ResponseToUuid: mmtpMessage.GetUuid(),
							Response:       mmtp.ResponseEnum_ERROR,
							ReasonText:     &reasonText,
						},
					},
				}
				if err = writeMessage(request.Context(), c, resp); err != nil {
					return
				}
				if err = c.Close(websocket.StatusUnsupportedData, reasonText); err != nil {
					log.Println("Closing websocket failed after sending error response:", err)
				}
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
							if err = handleSubscribe(mmtpMessage, agent, subMu, subs, outgoingChannel, request, c); err != nil {
								log.Println("Failed handling Subscribe message:", err)
							}
							break
						}
					case mmtp.ProtocolMessageType_UNSUBSCRIBE_MESSAGE:
						{
							if err = handleUnsubscribe(mmtpMessage, subMu, subs, agent, request, c, outgoingChannel); err != nil {
								log.Println("Failed handling Unsubscribe message:", err)
							}
							break
						}
					case mmtp.ProtocolMessageType_SEND_MESSAGE:
						{
							if n > MessageSizeLimit {
								sendErrorMessage(mmtpMessage.GetUuid(), "The message size exceeds the allowed 50 KiB", request.Context(), c)
								break
							}
							handleSend(mmtpMessage, outgoingChannel, request, c, signatureAlgorithm, mrnToAgent, mrnToAgentMu, subMu, subs, agent)
							break
						}
					case mmtp.ProtocolMessageType_RECEIVE_MESSAGE:
						{
							if err = handleReceive(mmtpMessage, agent, request, c); err != nil {
								log.Println("Failed handling Receive message:", err)
							}
							break
						}
					case mmtp.ProtocolMessageType_FETCH_MESSAGE:
						{
							if err = handleFetch(mmtpMessage, agent, request, c); err != nil {
								log.Println("Failed handling Fetch message:", err)
							}
							break
						}
					case mmtp.ProtocolMessageType_DISCONNECT_MESSAGE:
						{
							if err = handleDisconnect(mmtpMessage, request, c); err != nil {
								log.Println("Failed handling Disconnect message:", err)
							}
							break
						}
					case mmtp.ProtocolMessageType_CONNECT_MESSAGE:
						{
							sendErrorMessage(mmtpMessage.GetUuid(), "Already connected", request.Context(), c)
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

func handleSubscribe(mmtpMessage *mmtp.MmtpMessage, agent *Agent, subMu *sync.RWMutex, subs map[string]*Subscription, outgoingChannel chan<- *mmtp.MmtpMessage, request *http.Request, c *websocket.Conn) error {
	if subscribe := mmtpMessage.GetProtocolMessage().GetSubscribeMessage(); subscribe != nil {
		switch subscribe.GetSubjectOrDirectMessages().(type) {
		case *mmtp.Subscribe_Subject:
			return handleSubscribeSubject(mmtpMessage, agent, subMu, subs, outgoingChannel, request, c)
		case *mmtp.Subscribe_DirectMessages:
			return handleSubscribeDirect(mmtpMessage, agent, subscribe, request, c, outgoingChannel)
		}
	}
	return fmt.Errorf("something went wrong while handling subscribe message")
}

func handleSubscribeSubject(mmtpMessage *mmtp.MmtpMessage, agent *Agent, subMu *sync.RWMutex, subs map[string]*Subscription, outgoingChannel chan<- *mmtp.MmtpMessage, request *http.Request, c *websocket.Conn) error {
	subject := mmtpMessage.GetProtocolMessage().GetSubscribeMessage().GetSubject()
	if subject == "" {
		sendErrorMessage(mmtpMessage.GetUuid(), "Cannot subscribe to empty subject", request.Context(), c)
		return nil
	}
	subMu.Lock()
	sub, exists := subs[subject]
	if !exists {
		sub = NewSubscription(subject)
		sub.AddSubscriber(agent)
		subs[subject] = sub
		outgoingChannel <- mmtpMessage
	} else {
		sub.AddSubscriber(agent)
	}
	subMu.Unlock()
	agent.Interests = append(agent.Interests, subject)

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
		return fmt.Errorf("could not send subscribe response to Edge Router: %w", err)
	}
	return nil
}

func handleSubscribeDirect(mmtpMessage *mmtp.MmtpMessage, agent *Agent, subscribe *mmtp.Subscribe, request *http.Request, c *websocket.Conn, outgoingChannel chan<- *mmtp.MmtpMessage) error {
	directMessages := subscribe.GetDirectMessages()
	if !directMessages {
		sendErrorMessage(mmtpMessage.GetUuid(), "The directMessages flag needs to be true to be able to subscribe to direct messages", request.Context(), c)
		return nil
	}
	if (agent.Mrn == "") || (len(request.TLS.PeerCertificates) == 0) {
		sendErrorMessage(mmtpMessage.GetUuid(), "You need to be authenticated to be able to subscribe to direct messages", request.Context(), c)
		return nil
	}
	// Subscribe on direct messages to the Agent
	sub := &mmtp.MmtpMessage{
		MsgType: mmtp.MsgType_PROTOCOL_MESSAGE,
		Uuid:    uuid.NewString(),
		Body: &mmtp.MmtpMessage_ProtocolMessage{
			ProtocolMessage: &mmtp.ProtocolMessage{
				ProtocolMsgType: mmtp.ProtocolMessageType_SUBSCRIBE_MESSAGE,
				Body: &mmtp.ProtocolMessage_SubscribeMessage{
					SubscribeMessage: &mmtp.Subscribe{
						SubjectOrDirectMessages: &mmtp.Subscribe_Subject{
							Subject: agent.Mrn,
						},
					},
				},
			},
		},
	}
	outgoingChannel <- sub

	agent.directMessages = true

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
		return fmt.Errorf("could not send subscribe response to Edge Router: %w", err)
	}
	return nil
}

func handleUnsubscribe(mmtpMessage *mmtp.MmtpMessage, subMu *sync.RWMutex, subs map[string]*Subscription, agent *Agent, request *http.Request, c *websocket.Conn, outgoingChannel chan<- *mmtp.MmtpMessage) error {
	if unsubscribe := mmtpMessage.GetProtocolMessage().GetUnsubscribeMessage(); unsubscribe != nil {
		switch unsubscribe.GetSubjectOrDirectMessages().(type) {
		case *mmtp.Unsubscribe_Subject:
			return handleUnsubscribeSubject(mmtpMessage, subMu, subs, agent, request, c, unsubscribe)
		case *mmtp.Unsubscribe_DirectMessages:
			return handleUnsubscribeDirect(mmtpMessage, unsubscribe, request, c, outgoingChannel, agent)
		}
	}
	return fmt.Errorf("something went wrong while handling unsubscribe message")
}

func handleUnsubscribeSubject(mmtpMessage *mmtp.MmtpMessage, subMu *sync.RWMutex, subs map[string]*Subscription, agent *Agent, request *http.Request, c *websocket.Conn, unsubscribe *mmtp.Unsubscribe) error {
	subject := unsubscribe.GetSubject()
	if subject == "" {
		reasonText := "Tried to unsubscribe to empty subject"
		sendErrorMessage(mmtpMessage.GetUuid(), reasonText, request.Context(), c)
		return nil
	}
	subMu.Lock()
	sub, exists := subs[subject]
	if exists {
		sub.DeleteSubscriber(agent)
	}
	subMu.Unlock()
	interests := agent.Interests
	for i := range interests {
		if strings.EqualFold(subject, interests[i]) {
			interests[i] = interests[len(interests)-1]
			interests[len(interests)-1] = ""
			interests = interests[:len(interests)-1]
			agent.Interests = interests
			break
		}
	}
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
		return fmt.Errorf("could not write response to unsubscribe message: %w", err)
	}
	return nil
}

func handleUnsubscribeDirect(mmtpMessage *mmtp.MmtpMessage, unsubscribe *mmtp.Unsubscribe, request *http.Request, c *websocket.Conn, outgoingChannel chan<- *mmtp.MmtpMessage, agent *Agent) error {
	directMessages := unsubscribe.GetDirectMessages()
	if !directMessages {
		reason := "The directMessages flag needs to be true to be able to unsubscribe from direct messages"
		sendErrorMessage(mmtpMessage.GetUuid(), reason, request.Context(), c)
		return nil
	}
	if agent.Mrn != "" {
		unsub := &mmtp.MmtpMessage{
			MsgType: mmtp.MsgType_PROTOCOL_MESSAGE,
			Uuid:    uuid.NewString(),
			Body: &mmtp.MmtpMessage_ProtocolMessage{
				ProtocolMessage: &mmtp.ProtocolMessage{
					ProtocolMsgType: mmtp.ProtocolMessageType_UNSUBSCRIBE_MESSAGE,
					Body: &mmtp.ProtocolMessage_UnsubscribeMessage{
						UnsubscribeMessage: &mmtp.Unsubscribe{
							SubjectOrDirectMessages: &mmtp.Unsubscribe_Subject{
								Subject: agent.Mrn,
							},
						},
					},
				},
			},
		}
		outgoingChannel <- unsub

		agent.directMessages = false

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
			return fmt.Errorf("could not send unsubscribe response to Agent: %w", err)
		}
	}
	return nil
}

func handleSend(mmtpMessage *mmtp.MmtpMessage, outgoingChannel chan<- *mmtp.MmtpMessage, request *http.Request, c *websocket.Conn, signatureAlgorithm x509.SignatureAlgorithm, mrnToAgent map[string]*Agent, mrnToAgentMu *sync.RWMutex, subMu *sync.RWMutex, subs map[string]*Subscription, agent *Agent) {
	if send := mmtpMessage.GetProtocolMessage().GetSendMessage(); send != nil {
		if agent.Mrn == "" || request.TLS == nil || len(request.TLS.PeerCertificates) == 0 {
			sendErrorMessage(mmtpMessage.GetUuid(), "Unauthenticated agents cannot send messages", request.Context(), c)
			return
		}
		err := verifySignatureOnMessage(mmtpMessage, signatureAlgorithm, request)
		if err != nil {
			log.Println("Verification of signature on message failed:", err)
			return
		}

		outgoingChannel <- mmtpMessage
		header := send.GetApplicationMessage().GetHeader()
		if len(header.GetRecipients().GetRecipients()) > 0 {
			mrnToAgentMu.RLock()
			for _, recipient := range header.GetRecipients().Recipients {
				a, exists := mrnToAgent[recipient]
				if exists && a.directMessages {
					if err = a.QueueMessage(mmtpMessage); err != nil {
						log.Println("Could not queue message to agent:", err)
					}
				}
			}
			mrnToAgentMu.RUnlock()
		} else if header.GetSubject() != "" {
			subMu.RLock()
			sub, exists := subs[header.GetSubject()]
			if exists {
				for _, subscriber := range sub.Subscribers {
					if subscriber.Mrn != agent.Mrn {
						if err = subscriber.QueueMessage(mmtpMessage); err != nil {
							log.Println("Could not queue message to agent:", err)
						}
					}
				}
			}
			subMu.RUnlock()
		}
	}
}

func verifySignatureOnMessage(mmtpMessage *mmtp.MmtpMessage, signatureAlgorithm x509.SignatureAlgorithm, request *http.Request) error {
	appMessage := mmtpMessage.GetProtocolMessage().GetSendMessage().GetApplicationMessage()

	// verify signature on message
	signatureBytes, err := base64.StdEncoding.DecodeString(appMessage.GetSignature())
	if err != nil {
		return fmt.Errorf("signature could be not decoded from base64: %w", err)
	}

	toBeVerified := make([]byte, 0)
	switch content := appMessage.GetHeader().GetSubjectOrRecipient().(type) {
	case *mmtp.ApplicationMessageHeader_Subject:
		toBeVerified = append(toBeVerified, content.Subject...)
	case *mmtp.ApplicationMessageHeader_Recipients:
		for _, r := range content.Recipients.GetRecipients() {
			toBeVerified = append(toBeVerified, r...)
		}
	}

	toBeVerified = append(toBeVerified, strconv.FormatInt(appMessage.GetHeader().GetExpires(), 10)...)
	toBeVerified = append(toBeVerified, appMessage.GetHeader().GetSender()...)

	if appMessage.GetHeader().GetQosProfile() != "" {
		toBeVerified = append(toBeVerified, appMessage.Header.GetQosProfile()...)
	}

	toBeVerified = append(toBeVerified, strconv.Itoa(int(appMessage.GetHeader().GetBodySizeNumBytes()))...)
	toBeVerified = append(toBeVerified, appMessage.GetBody()...)

	if signatureAlgorithm == x509.UnknownSignatureAlgorithm {
		return fmt.Errorf("a suitable signature algorithm could not be found for verifying signature on message")
	}

	if err = request.TLS.PeerCertificates[0].CheckSignature(signatureAlgorithm, toBeVerified, signatureBytes); err != nil {
		// return an error saying that the signature is not valid over the body of the message
		return fmt.Errorf("the signature on the message could not be verified: %w", err)
	}
	return nil
}

func handleReceive(mmtpMessage *mmtp.MmtpMessage, agent *Agent, request *http.Request, c *websocket.Conn) error {
	if receive := mmtpMessage.GetProtocolMessage().GetReceiveMessage(); receive != nil {
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
				agent.BulkQueueMessages(mmtpMessages)
				return fmt.Errorf("could not send messages to Agent: %w", err)
			}
		} else { // Receive all messages
			agent.msgMu.Lock()
			msgsLen := len(agent.Messages)
			appMsgs := make([]*mmtp.ApplicationMessage, 0, msgsLen)
			for _, mmtpMsg := range agent.Messages {
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
			defer agent.msgMu.Unlock()
			err := writeMessage(request.Context(), c, resp)
			if err != nil {
				return fmt.Errorf("could not send messages to Agent: %w", err)
			} else {
				agent.Messages = make(map[string]*mmtp.MmtpMessage)
			}
		}
	}
	return nil
}

func handleFetch(mmtpMessage *mmtp.MmtpMessage, agent *Agent, request *http.Request, c *websocket.Conn) error {
	if fetch := mmtpMessage.GetProtocolMessage().GetFetchMessage(); fetch != nil {
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

func sendErrorMessage(uid string, errorText string, ctx context.Context, c *websocket.Conn) {
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
	if err := writeMessage(ctx, c, resp); err != nil {
		log.Println("Could not send error response:", err)
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
			err := performOCSPCheck(clientCert, issuingCert, httpClient)
			if err != nil {
				return err
			}
		} else if len(clientCert.CRLDistributionPoints) > 0 {
			err := performCRLCheck(clientCert, httpClient, issuingCert)
			if err != nil {
				return err
			}
		} else {
			return fmt.Errorf("was not able to check revocation status of client certificate")
		}

		return nil
	}
}

func performOCSPCheck(clientCert *x509.Certificate, issuingCert *x509.Certificate, httpClient *http.Client) error {
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
	if ocspResp.Certificate == nil {
		if err = ocspResp.CheckSignatureFrom(issuingCert); err != nil {
			return fmt.Errorf("the signature on the OCSP response is not valid: %w", err)
		}
	}
	if (ocspResp.Certificate != nil) && !ocspResp.Certificate.Equal(issuingCert) {
		return fmt.Errorf("the certificate embedded in the OCSP response does not match the configured issuing CA")
	}
	if ocspResp.Status != ocsp.Good {
		return fmt.Errorf("the given client certificate has been revoked")
	}
	return nil
}

func performCRLCheck(clientCert *x509.Certificate, httpClient *http.Client, issuingCert *x509.Certificate) error {
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
		return fmt.Errorf("failed to close CRL response body: %w", err)
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
	return nil
}

func handleIncomingMessages(ctx context.Context, edgeRouter *EdgeRouter, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
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
			edgeRouter.routerConnMu.Lock()
			if err := writeMessage(ctx, edgeRouter.routerWs, receiveMsg); err != nil {
				log.Println("Was not able to send Receive message to MMS Router:", err)
				edgeRouter.routerConnMu.Unlock()
				continue
			}

			response, _, err := readMessage(ctx, edgeRouter.routerWs)
			edgeRouter.routerConnMu.Unlock()
			if err != nil {
				log.Println("Could not receive response from MMS Router:", err)
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
						log.Println("Received response with error:", responseMsg.GetReasonText())
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
									if err = subscriber.QueueMessage(incomingMessage); err != nil {
										log.Println("Could not queue message:", err)
									}
								}
								edgeRouter.subMu.RUnlock()
								break
							}
						case *mmtp.ApplicationMessageHeader_Recipients:
							{
								edgeRouter.mrnToAgentMu.RLock()
								for _, recipient := range subjectOrRecipient.Recipients.GetRecipients() {
									agent := edgeRouter.mrnToAgent[recipient]
									if agent.directMessages {
										err = agent.QueueMessage(incomingMessage)
										if err != nil {
											log.Println("Could not queue message for Edge Router:", err)
										}
									}
								}
								edgeRouter.mrnToAgentMu.RUnlock()
								break
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

func handleOutgoingMessages(ctx context.Context, edgeRouter *EdgeRouter, wg *sync.WaitGroup) {
	defer func() {
		wg.Done()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		default:
			for outgoingMessage := range edgeRouter.outgoingChannel {
				switch outgoingMessage.GetMsgType() {
				case mmtp.MsgType_PROTOCOL_MESSAGE:
					{
						edgeRouter.routerConnMu.Lock()
						if err := writeMessage(ctx, edgeRouter.routerWs, outgoingMessage); err != nil {
							log.Println("Could not send outgoing message to MMS Router, will try again later:", err)
							edgeRouter.outgoingChannel <- outgoingMessage
							edgeRouter.routerConnMu.Unlock()
							continue
						}
						protoMsgType := outgoingMessage.GetProtocolMessage().GetProtocolMsgType()
						if protoMsgType == mmtp.ProtocolMessageType_SUBSCRIBE_MESSAGE ||
							protoMsgType == mmtp.ProtocolMessageType_UNSUBSCRIBE_MESSAGE {
							resp, _, err := readMessage(ctx, edgeRouter.routerWs)
							if err != nil {
								log.Println("Could not get response from router:", err)
								edgeRouter.outgoingChannel <- outgoingMessage
								edgeRouter.routerConnMu.Unlock()
								continue
							}
							response := resp.GetResponseMessage()
							if response.GetResponse() != mmtp.ResponseEnum_GOOD {
								log.Println("Response from Router did not contain a GOOD status:", response.GetReasonText())
								edgeRouter.outgoingChannel <- outgoingMessage
							}
						}
						edgeRouter.routerConnMu.Unlock()
					}
				default:
					continue
				}
			}
		}
	}
}

func readMessage(ctx context.Context, c *websocket.Conn) (*mmtp.MmtpMessage, int, error) {
	_, b, err := c.Read(ctx)
	if err != nil {
		return nil, -1, fmt.Errorf("could not read message from edge router: %w", err)
	}
	mmtpMessage := &mmtp.MmtpMessage{}
	if err = proto.Unmarshal(b, mmtpMessage); err != nil {
		return nil, -1, fmt.Errorf("could not unmarshal message: %w", err)
	}
	return mmtpMessage, len(b), nil
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

	routerAddr := flag.String("raddr", "ws://localhost:8080", "The websocket URL of the Router to connect to.")
	listeningPort := flag.Int("port", 8888, "The port number that this Edge Router should listen on.")
	ownMrn := flag.String("mrn", "urn:mrn:mcp:device:idp1:org1:er", "The MRN of this Edge Router")
	clientCertPath := flag.String("client-cert", "", "Path to a client certificate which will be used to authenticate towards Router. If none is provided, mutual TLS will be disabled.")
	clientCertKeyPath := flag.String("client-cert-key", "", "Path to a client certificate private key which will be used to authenticate towards Router. If none is provided, mutual TLS will be disabled.")
	certPath := flag.String("cert-path", "", "Path to a TLS certificate file. If none is provided, TLS will be disabled.")
	certKeyPath := flag.String("cert-key-path", "", "Path to a TLS certificate private key. If none is provided, TLS will be disabled.")
	clientCAs := flag.String("client-ca", "", "Path to a file containing a list of client CAs that can connect to this Edge Router.")

	flag.Parse()

	outgoingChannel := make(chan *mmtp.MmtpMessage)

	certificates := make([]tls.Certificate, 0, 1)

	if *clientCertPath != "" && *clientCertKeyPath != "" {
		cert, err := tls.LoadX509KeyPair(*clientCertPath, *clientCertKeyPath)
		if err != nil {
			log.Println("Could not read the provided client certificate:", err)
			return
		}
		certificates = append(certificates, cert)
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates: certificates,
			},
		},
	}

	routerWs, _, err := websocket.Dial(ctx, *routerAddr, &websocket.DialOptions{HTTPClient: httpClient, CompressionMode: websocket.CompressionContextTakeover})
	if err != nil {
		log.Println("Could not connect to MMS Router:", err)
		return
	}

	// Set the read limit to 1 MiB instead of 32 KiB
	routerWs.SetReadLimit(WsReadLimit)

	wg := &sync.WaitGroup{}

	er, err := NewEdgeRouter(":"+strconv.Itoa(*listeningPort), *ownMrn, outgoingChannel, routerWs, ctx, wg, clientCAs)
	if err != nil {
		log.Println("Could not create MMS Edge Router instance:", err)
		return
	}

	wg.Add(1)
	go er.StartEdgeRouter(ctx, wg, certPath, certKeyPath)

	mdnsServer, err := zeroconf.Register("MMS Edge Router", "_mms-edgerouter._tcp", "local.", *listeningPort, nil, nil)
	if err != nil {
		log.Println("Could not create mDNS service, shutting down", err)
		ch <- os.Interrupt
	}
	defer mdnsServer.Shutdown()

	<-ch
	log.Println("Received signal, shutting down...")
	cancel()
	wg.Wait()
}
