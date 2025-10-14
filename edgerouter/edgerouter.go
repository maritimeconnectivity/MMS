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
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/log"
	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/libp2p/zeroconf/v2"
	"github.com/maritimeconnectivity/MMS/consumer"
	"github.com/maritimeconnectivity/MMS/mmtp"
	"github.com/maritimeconnectivity/MMS/utils/auth"
	"github.com/maritimeconnectivity/MMS/utils/errMsg"
	"github.com/maritimeconnectivity/MMS/utils/revocation"
	"github.com/maritimeconnectivity/MMS/utils/rw"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	WsReadLimit      int64 = -1                  // 1 MiB = 1048576 B
	MessageSizeLimit int   = 50 * (1 << 10)      // 50 KiB = 51200 B
	ExpirationLimit        = time.Hour * 24 * 30 // 30 days
	ChannelBufSize   int   = 1048576
)

// Agent type representing a connected Edge Router
type Agent struct {
	consumer.Consumer        // A base struct that applies both to Agent and Edge Router consumers
	agentUuid         string // UUID for uniquely identifying this Agent
	directMessages    bool   // bool indicating whether the Agent is subscribing to direct messages
	authenticated     bool   // bool indicating whther the Agent is authenticated
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

// EdgeRouter type representing an MMS Edge Router
type EdgeRouter struct {
	ownMrn          string                       // The MRN of this EdgeRouter
	subscriptions   map[string]*Subscription     // a mapping from Interest names to Subscription slices
	subMu           *sync.RWMutex                // a Mutex for locking the subscriptions map
	agents          map[string]*Agent            // a map of connected Agents
	agentsMu        *sync.RWMutex                // a Mutex for locking the agents map
	mrnToAgent      map[string]*Agent            // a mapping from an Agent MRN to a UUID
	mrnToAgentMu    *sync.RWMutex                // a Mutex for locking the mrnToAgent map
	httpServer      *http.Server                 // the http server that is used to bootstrap websocket connections
	outgoingChannel chan *mmtp.MmtpMessage       // channel for outgoing messages
	routerWs        *websocket.Conn              // the websocket connection to the MMS Router
	awaitResponse   map[string]*mmtp.MmtpMessage // a mapping from uuid to sent messages which we expect an answer to
	responseMu      *sync.RWMutex                // a Mutex locking the awaitResponse map
	routerAddr      *string                      // Address of initial router to connect to, if provided
	httpClient      *http.Client                 // The EdgeRouters http client if provided
	wsMu            *sync.RWMutex                // Mutex for locking the WS upon send/recv, to properly handle if socket was closed
	geoLocation     string                       //Lookup code for the actual position of the running instance
}

func NewEdgeRouter(listeningAddr string, mrn string, outgoingChannel chan *mmtp.MmtpMessage, routerWs *websocket.Conn, ctx context.Context, wg *sync.WaitGroup, clientCAs *string, routerAddr *string, httpClient *http.Client, geoLoc string) (*EdgeRouter, error) {
	subs := make(map[string]*Subscription)
	subMu := &sync.RWMutex{}
	agents := make(map[string]*Agent)
	agentsMu := &sync.RWMutex{}
	mrnToAgent := make(map[string]*Agent)
	mrnToAgentMu := &sync.RWMutex{}
	awaitResponse := make(map[string]*mmtp.MmtpMessage)
	responseMu := &sync.RWMutex{}
	wsMu := &sync.RWMutex{}

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
		awaitResponse:   awaitResponse,
		responseMu:      responseMu,
		routerAddr:      routerAddr,
		httpClient:      httpClient,
		wsMu:            wsMu,
		geoLocation:     geoLoc,
	}, nil
}

func (er *EdgeRouter) connectMMTPToRouter(ctx context.Context) error {
	log.Debugf("Own mrn is %v", er.ownMrn)

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
	err := rw.WriteMessage(ctx, er.routerWs, connect)
	if err != nil {
		return fmt.Errorf("could not send connect message: %w", err)
	}

	response, _, err := rw.ReadMessage(ctx, er.routerWs)
	if err != nil {
		return fmt.Errorf("something went wrong while receiving response from MMS Router: %w", err)
	}

	connectResp := response.GetResponseMessage()
	if connectResp.Response != mmtp.ResponseEnum_GOOD {
		return fmt.Errorf("the MMS Router did not accept Connect: %s", connectResp.GetReasonText())
	}

	return nil
}

func (er *EdgeRouter) StartEdgeRouter(ctx context.Context, wg *sync.WaitGroup, certPath *string, certKeyPath *string) {
	defer func() {
		close(er.outgoingChannel)
		wg.Done()
	}()

	log.Info("Starting Edge Router")

	// TODO store reconnect token and handle reconnection in case of disconnect
	if er.routerWs != nil {
		err := er.connectMMTPToRouter(ctx)
		if err != nil {
			log.Error(err)
		}
	}

	wg.Add(2)
	go func() {
		log.Infof("Websocket listening on %s", er.httpServer.Addr)
		if *certPath != "" && *certKeyPath != "" {
			er.httpServer.TLSConfig.GetCertificate = func(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
				cert, err := tls.LoadX509KeyPair(*certPath, *certKeyPath)
				if err != nil {
					return nil, err
				}
				return &cert, nil
			}
			if err := er.httpServer.ListenAndServeTLS("", ""); err != nil {
				log.Warn(err)
			}
		} else {
			if err := er.httpServer.ListenAndServe(); err != nil {
				log.Warn(err)
			}
		}
		wg.Done()
	}()

	go er.messageGC(ctx, wg)

	wg.Add(2)
	log.Debug("Spawning message threads")
	go handleIncomingMessages(ctx, er, wg)
	go handleOutgoingMessages(ctx, er, wg)

	<-ctx.Done()
	log.Warn("Shutting down Edge Router")

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

	if er.routerWs != nil {
		if err := rw.WriteMessage(context.Background(), er.routerWs, disconnectMsg); err != nil {
			log.Warn("Could not send disconnect to Router:", err)
		}

		response, _, err := rw.ReadMessage(context.Background(), er.routerWs)
		if err != nil || response.GetResponseMessage().Response != mmtp.ResponseEnum_GOOD {
			log.Warn("Graceful disconnect from Router failed")
		}

		<-er.routerWs.CloseRead(context.Background()).Done()
	}

	if err := er.httpServer.Shutdown(context.Background()); err != nil {
		log.Error(err)
	}
}

func (er *EdgeRouter) TryConnectRouter(ctx context.Context) {
	if er.routerWs != nil {
		wsCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := er.routerWs.Ping(wsCtx); err == nil { //If there is no errors, we already have a conn
			log.Info("Connection to router was already established")
			return
		}
	}
	if er.routerWs != nil {
		if err := er.routerWs.CloseNow(); err != nil {
			log.Warn(err)
		}
	}

	//Make sure old connection is completely cleaned up

	//Runs until a router has been found
	log.Info("Probing for a router connection")
	for {
		log.Debugf("Attempt connect to %v", *er.routerAddr)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
			routerWs, _, err := websocket.Dial(ctx, *er.routerAddr, &websocket.DialOptions{HTTPClient: er.httpClient, CompressionMode: websocket.CompressionContextTakeover})
			if err != nil {
				log.Debugf("Could not connect to router: %v", err)
				continue
			}
			er.routerWs = routerWs
			er.routerWs.SetReadLimit(WsReadLimit)
			err = er.connectMMTPToRouter(ctx)
			if err != nil {
				log.Error(err)
			}
			log.Info("Router connected")
			return
		}
	}
}

// TryConnectRouterRoutine is a wrapper function called when starting TryConnectRouter in a new thread
func (er *EdgeRouter) TryConnectRouterRoutine(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	er.TryConnectRouter(ctx)
}

// Function for garbage collection of expired messages
func (er *EdgeRouter) messageGC(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Minute): // run every 5 minutes
			er.agentsMu.RLock()
			now := time.Now().Unix()
			for _, a := range er.agents {
				a.MsgMu.Lock()
				for _, m := range a.Messages {
					expires := m.GetProtocolMessage().GetSendMessage().GetApplicationMessage().GetHeader().GetExpires()
					if now > expires {
						delete(a.Messages, m.Uuid)
					}
				}
				a.MsgMu.Unlock()
			}
			er.agentsMu.RUnlock()
		}
	}
}

// This function adds a request to the outgoing ch to receive messages from the router upon receiving a notify from the router
func (er *EdgeRouter) handleNotify(metadata []*mmtp.MessageMetadata) error {

	//Filter such that we only request to receive messages we were notified about
	filter := make([]string, 0, len(metadata))
	for elem := range metadata {
		uuId := metadata[elem].GetUuid()
		filter = append(filter, uuId)
	}

	receiveMsg := &mmtp.MmtpMessage{
		MsgType: mmtp.MsgType_PROTOCOL_MESSAGE,
		Uuid:    uuid.NewString(),
		Body: &mmtp.MmtpMessage_ProtocolMessage{
			ProtocolMessage: &mmtp.ProtocolMessage{
				ProtocolMsgType: mmtp.ProtocolMessageType_RECEIVE_MESSAGE,
				Body: &mmtp.ProtocolMessage_ReceiveMessage{
					ReceiveMessage: &mmtp.Receive{
						Filter: &mmtp.Filter{
							MessageUuids: filter,
						},
					},
				},
			},
		},
	}

	er.outgoingChannel <- receiveMsg
	return nil
}

func handleHttpConnection(outgoingChannel chan<- *mmtp.MmtpMessage, subs map[string]*Subscription, subMu *sync.RWMutex, agents map[string]*Agent, agentsMu *sync.RWMutex, mrnToAgent map[string]*Agent, mrnToAgentMu *sync.RWMutex, ctx context.Context, wg *sync.WaitGroup) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		wg.Add(1)
		c, err := websocket.Accept(writer, request, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			log.Error("Could not establish websocket connection", err)
			wg.Done()
			return
		}
		defer func(c *websocket.Conn, code websocket.StatusCode, reason string) {
			err := c.Close(code, reason)
			if err != nil && !errors.Is(err, net.ErrClosed) {
				log.Errorf("Could not close connection: %s", err.Error())
			}

			wg.Done()
		}(c, websocket.StatusInternalError, "PANIC!!!")

		// Set the read limit to 1 MiB instead of 32 KiB
		c.SetReadLimit(WsReadLimit)

		mmtpMessage, _, err := rw.ReadMessage(ctx, c)
		if err != nil {
			log.Warn("Could not read message:", err)
			if err = c.Close(websocket.StatusUnsupportedData, "The first message could not be parsed as an MMTP message"); err != nil {
				log.Error(err)
			}
			return
		}

		protoMessage := mmtpMessage.GetProtocolMessage()
		if mmtpMessage.MsgType != mmtp.MsgType_PROTOCOL_MESSAGE || protoMessage == nil {
			if err = c.Close(websocket.StatusUnsupportedData, "The first message needs to be a Protocol Message containing a Connect message"); err != nil {
				log.Error(err)
			}
			return
		}

		connect := protoMessage.GetConnectMessage()
		if connect == nil {
			if err = c.Close(websocket.StatusUnsupportedData, "The first message needs to contain a Connect message"); err != nil {
				log.Error(err)
			}
			return
		}

		agentMrn := connect.GetOwnMrn()
		agentMrn = strings.ToLower(agentMrn)

		//Authenticate agent
		signatureAlgorithm, authenticated, err := auth.AuthenticateConsumer(request, agentMrn)
		if err != nil {
			var certErr *auth.CertValErr
			var sigAlgErr *auth.SigAlgErr
			var mrnErr *auth.MrnMismatchErr
			switch {
			case errors.As(err, &certErr):
			case errors.As(err, &sigAlgErr):
				if wsErr := c.Close(websocket.StatusPolicyViolation, err.Error()); wsErr != nil {
					log.Error(wsErr)
				}
			case errors.As(err, &mrnErr):
				if wsErr := c.Close(websocket.StatusUnsupportedData, err.Error()); wsErr != nil {
					log.Error(wsErr)
				}
			default:
				log.Errorf("Unknown error occured: %s\n", err.Error())
			}
			return
		}

		var agent *Agent
		agentsMu.RLock()
		savedAgent, ok := agents[connect.GetReconnectToken()]
		agentsMu.RUnlock()
		if ok {
			agent = savedAgent
		} else {
			agent = &Agent{
				Consumer: consumer.Consumer{
					Mrn:           agentMrn,
					Interests:     make([]string, 0, 1),
					Messages:      make(map[string]*mmtp.MmtpMessage),
					MsgMu:         &sync.RWMutex{},
					Notifications: make(map[string]*mmtp.MmtpMessage),
					NotifyMu:      &sync.RWMutex{},
				},
				agentUuid:      uuid.NewString(),
				authenticated:  authenticated,
				directMessages: false,
			}
			if agentMrn != "" {
				mrnToAgentMu.Lock()
				mrnToAgent[agentMrn] = agent
				mrnToAgentMu.Unlock()
			}
		}
		agent.ReconnectToken = uuid.NewString()
		agentsMu.Lock()
		agents[agent.ReconnectToken] = agent
		agentsMu.Unlock()

		resp := &mmtp.MmtpMessage{
			MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
			Uuid:    uuid.NewString(),
			Body: &mmtp.MmtpMessage_ResponseMessage{
				ResponseMessage: &mmtp.ResponseMessage{
					ResponseToUuid: mmtpMessage.GetUuid(),
					ReconnectToken: &agent.ReconnectToken,
					Response:       mmtp.ResponseEnum_GOOD,
				}},
		}
		err = rw.WriteMessage(request.Context(), c, resp)
		if err != nil {
			log.Error("Could not send response:", err)
			return
		}

		//Start thread that checks for incoming messages and notfies agents
		wg.Add(1)
		agCtx, cancel := context.WithCancel(ctx)
		defer cancel() //When done handling client
		go agent.CheckNewMessages(agCtx, c, wg)

		for {
			mmtpMessage, n, err := rw.ReadMessage(ctx, c)
			if err != nil {
				log.Warn("Something went wrong while reading message from Agent:", err)
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
				if err = rw.WriteMessage(request.Context(), c, resp); err != nil {
					return
				}
				if err = c.Close(websocket.StatusUnsupportedData, reasonText); err != nil {
					log.Error("Closing websocket failed after sending error response:", err)
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
								log.Error("Failed handling Subscribe message:", err)
							}
						}
					case mmtp.ProtocolMessageType_UNSUBSCRIBE_MESSAGE:
						{
							if err = handleUnsubscribe(mmtpMessage, subMu, subs, agent, request, c, outgoingChannel); err != nil {
								log.Error("Failed handling Unsubscribe message:", err)
							}
						}
					case mmtp.ProtocolMessageType_SEND_MESSAGE:
						{
							if n > MessageSizeLimit {
								errMsg.SendErrorMessage(mmtpMessage.GetUuid(), "The message size exceeds the allowed 50 KiB", request.Context(), c)
								break
							}
							handleSend(mmtpMessage, outgoingChannel, request, c, signatureAlgorithm, mrnToAgent, mrnToAgentMu, subMu, subs, agent)
						}
					case mmtp.ProtocolMessageType_RECEIVE_MESSAGE:
						{
							if err = agent.HandleReceive(mmtpMessage, request, c); err != nil {
								log.Error("Failed handling Receive message:", err)
							}
						}
					case mmtp.ProtocolMessageType_FETCH_MESSAGE:
						{
							if err = agent.HandleFetch(mmtpMessage, request, c); err != nil {
								log.Error("Failed handling Fetch message:", err)
							}
						}
					case mmtp.ProtocolMessageType_DISCONNECT_MESSAGE:
						{
							log.Info("Disconnect")
							if err = agent.HandleDisconnect(mmtpMessage, request, c); err != nil {
								log.Error("Failed handling Disconnect message:", err)
							} else {
								//Sucess, remove agent from edgerouters map of agents and list of reconnecttokens
								mrnToAgentMu.Lock()
								delete(mrnToAgent, agent.Mrn)
								mrnToAgentMu.Unlock()

								agentsMu.Lock()
								delete(agents, agent.ReconnectToken)
								agentsMu.Unlock()
							}
							return
						}
					case mmtp.ProtocolMessageType_CONNECT_MESSAGE:
						{
							errMsg.SendErrorMessage(mmtpMessage.GetUuid(), "Already connected", request.Context(), c)
						}
					default:
						continue
					}
				}
			case mmtp.MsgType_RESPONSE_MESSAGE:

			case mmtp.MsgType_UNSPECIFIED_MESSAGE:
				{
					reasonText := "Message type was unspecified"
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
					if err = rw.WriteMessage(request.Context(), c, resp); err != nil {
						log.Error("Could not send error message:", err)
						return
					}
				}

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
		errMsg.SendErrorMessage(mmtpMessage.GetUuid(), "Cannot subscribe to empty subject", request.Context(), c)
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
	if err := rw.WriteMessage(request.Context(), c, resp); err != nil {
		return fmt.Errorf("could not send subscribe response to Agent: %w", err)
	}
	return nil
}

func handleSubscribeDirect(mmtpMessage *mmtp.MmtpMessage, agent *Agent, subscribe *mmtp.Subscribe, request *http.Request, c *websocket.Conn, outgoingChannel chan<- *mmtp.MmtpMessage) error {
	directMessages := subscribe.GetDirectMessages()
	if !directMessages {
		errMsg.SendErrorMessage(mmtpMessage.GetUuid(), "The directMessages flag needs to be true to be able to subscribe to direct messages", request.Context(), c)
		return nil
	}
	if (agent.Mrn == "") || (len(request.TLS.PeerCertificates) == 0) {
		errMsg.SendErrorMessage(mmtpMessage.GetUuid(), "You need to be authenticated to be able to subscribe to direct messages", request.Context(), c)
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
	if err := rw.WriteMessage(request.Context(), c, resp); err != nil {
		return fmt.Errorf("could not send subscribe response to Agent: %w", err)
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
		errMsg.SendErrorMessage(mmtpMessage.GetUuid(), reasonText, request.Context(), c)
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
	if err := rw.WriteMessage(request.Context(), c, resp); err != nil {
		return fmt.Errorf("could not write response to unsubscribe message: %w", err)
	}
	return nil
}

func handleUnsubscribeDirect(mmtpMessage *mmtp.MmtpMessage, unsubscribe *mmtp.Unsubscribe, request *http.Request, c *websocket.Conn, outgoingChannel chan<- *mmtp.MmtpMessage, agent *Agent) error {
	directMessages := unsubscribe.GetDirectMessages()
	if !directMessages {
		reason := "The directMessages flag needs to be true to be able to unsubscribe from direct messages"
		errMsg.SendErrorMessage(mmtpMessage.GetUuid(), reason, request.Context(), c)
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
		if err := rw.WriteMessage(request.Context(), c, resp); err != nil {
			return fmt.Errorf("could not send unsubscribe response to Agent: %w", err)
		}
	}
	return nil
}

func handleSend(mmtpMessage *mmtp.MmtpMessage, outgoingChannel chan<- *mmtp.MmtpMessage, request *http.Request, c *websocket.Conn, signatureAlgorithm x509.SignatureAlgorithm, mrnToAgent map[string]*Agent, mrnToAgentMu *sync.RWMutex, subMu *sync.RWMutex, subs map[string]*Subscription, agent *Agent) {
	if send := mmtpMessage.GetProtocolMessage().GetSendMessage(); send != nil {
		if !agent.authenticated {
			errMsg.SendErrorMessage(mmtpMessage.GetUuid(), "Unauthenticated agents cannot send messages", request.Context(), c)
			return
		}

		if agent.Mrn != send.GetApplicationMessage().GetHeader().GetSender() {
			errMsg.SendErrorMessage(mmtpMessage.GetUuid(), "Sender MRN must match Agent MRN", request.Context(), c)
			return
		}

		err := auth.VerifySignatureOnMessage(mmtpMessage, signatureAlgorithm, request)
		if err != nil {
			log.Warn("Verification of signature on message failed:", err)
			errMsg.SendErrorMessage(mmtpMessage.GetUuid(), "Could not authenticate message signature", request.Context(), c)
			return
		}

		if len(send.GetApplicationMessage().GetHeader().GetSubject()) > 100 {
			errMsg.SendErrorMessage(mmtpMessage.GetUuid(), "Subject string may not exceed 100 characters", request.Context(), c)
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
						log.Error("Could not queue message to agent:", err)
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
							log.Error("Could not queue message to agent:", err)
						}
					}
				}
			}
			subMu.RUnlock()
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
		if err := rw.WriteMessage(request.Context(), c, resp); err != nil {
			log.Error("Could not send Send OK response:", err)
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
			err := revocation.PerformOCSPCheck(clientCert, issuingCert, httpClient)
			if err != nil {
				return err
			}
		} else if len(clientCert.CRLDistributionPoints) > 0 {
			err := revocation.PerformCRLCheck(clientCert, httpClient, issuingCert)
			if err != nil {
				return err
			}
		} else {
			return fmt.Errorf("was not able to check revocation status of client certificate")
		}

		return nil
	}
}

func handleIncomingMessages(ctx context.Context, edgeRouter *EdgeRouter, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		default:
			response, _, err := rw.ReadMessage(ctx, edgeRouter.routerWs) //Block until receive from socket
			if err != nil {
				log.Warn("Could not receive response from MMS Router:", err)
				edgeRouter.wsMu.Lock()
				edgeRouter.TryConnectRouter(ctx)
				edgeRouter.wsMu.Unlock()
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
					edgeRouter.responseMu.Lock()
					msg, exists := edgeRouter.awaitResponse[responseMsg.GetResponseToUuid()]
					delete(edgeRouter.awaitResponse, responseMsg.GetResponseToUuid())
					edgeRouter.responseMu.Unlock()
					if exists {
						if response.GetResponseMessage().GetResponse() != mmtp.ResponseEnum_GOOD {
							edgeRouter.outgoingChannel <- msg //Attempt re-send
							continue
						}
					} else {
						if responseMsg.Response != mmtp.ResponseEnum_GOOD {
							log.Warn("Received response with error:", responseMsg.GetReasonText())
							continue
						}
					}

					now := time.Now()
					nowSeconds := now.Unix()
					for _, msgContent := range responseMsg.GetMessageContent() {
						appMsg := msgContent.GetMsg()
						msgExpires := appMsg.GetHeader().GetExpires()
						if appMsg == nil || nowSeconds > msgExpires || msgExpires > now.Add(ExpirationLimit).Unix() {
							// message is nil, expired or has a too long expiration, so we discard it
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
										log.Error("Could not queue message:", err)
									}
								}
								edgeRouter.subMu.RUnlock()
							}
						case *mmtp.ApplicationMessageHeader_Recipients:
							{
								edgeRouter.mrnToAgentMu.RLock()
								for _, recipient := range subjectOrRecipient.Recipients.GetRecipients() {
									log.Debugf("recipient %s, hex: %x", recipient, recipient)
									agent, ok := edgeRouter.mrnToAgent[recipient]
									log.Debugf("agent: %v, ok: %t", agent, ok)
									if ok && agent.directMessages {
										err = agent.QueueMessage(incomingMessage)
										if err != nil {
											log.Error("Could not queue message for Agent:", err)
										}
									}
								}
								edgeRouter.mrnToAgentMu.RUnlock()
							}
						}
					}
				}
			case *mmtp.MmtpMessage_ProtocolMessage:
				{
					protocolMsgType := response.GetProtocolMessage().GetProtocolMsgType()
					if protocolMsgType == mmtp.ProtocolMessageType_NOTIFY_MESSAGE {
						if err = edgeRouter.handleNotify(response.GetProtocolMessage().GetNotifyMessage().GetMessageMetadata()); err != nil {
							log.Error("Could not handle Notify received from router", err)
							continue
						}
					} else {
						log.Error("ERR, received ProtocolMsg with type:", protocolMsgType.String())
						continue
					}
				}

			default:
				continue

			}
		}
	}
}

func handleOutgoingMessages(ctx context.Context, edgeRouter *EdgeRouter, wg *sync.WaitGroup) {
	defer wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		default:

			for outgoingMessage := range edgeRouter.outgoingChannel {
				switch outgoingMessage.GetMsgType() {
				case mmtp.MsgType_PROTOCOL_MESSAGE:
					{
						if err := rw.WriteMessage(ctx, edgeRouter.routerWs, outgoingMessage); err != nil {
							log.Warn("Could not send outgoing message to MMS Router, will try again later:", err)
							edgeRouter.outgoingChannel <- outgoingMessage
							edgeRouter.wsMu.Lock()
							edgeRouter.TryConnectRouter(ctx)
							edgeRouter.wsMu.Unlock()
							continue
						}

						protoMsgType := outgoingMessage.GetProtocolMessage().GetProtocolMsgType()
						if protoMsgType == mmtp.ProtocolMessageType_SUBSCRIBE_MESSAGE ||
							protoMsgType == mmtp.ProtocolMessageType_UNSUBSCRIBE_MESSAGE {
							edgeRouter.responseMu.Lock()
							edgeRouter.awaitResponse[outgoingMessage.GetUuid()] = outgoingMessage
							edgeRouter.responseMu.Unlock()
						}
					}
				default:
					continue
				}
			}

		}
	}
}

func recordMetrics(ctx context.Context, wg *sync.WaitGroup, reg *prometheus.Registry, er *EdgeRouter) {
	defer wg.Done()
	connAgents := promauto.NewGauge(prometheus.GaugeOpts{
		Name: "edgerouter_num_connected_agents",
		Help: "The total number of agents connected to the edgerouter",
	})
	rcTokens := promauto.NewGauge(prometheus.GaugeOpts{
		Name: "edgerouter_num_agent_stored_reconnectTokens",
		Help: "The total number of stored reconnectTokens",
	})
	memAlloc := promauto.NewGauge(prometheus.GaugeOpts{
		Name: "edgerouter_heap_mem_alloc",
		Help: "The amount of heap allocated memory in KB",
	})

	geo := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "loc_export",                                // Name of the metric
			Help: "A metric to store geo-location as a label", // Description of the metric
		},
		[]string{"lookup"}, // Define the label key (in this case "location")
	)

	geoDataFlow := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "loc_export2",                                       // Name of the metric
			Help: "A metric mapping geo_Location and active dataflow", // Description of the metric
		},
		[]string{"lookup2"}, // Define the label key (in this case "location")
	)

	numConnClientsFlow := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "loc_export3",                                                // Name of the metric
			Help: "A metric mapping geo_Location and number of clients served", // Description of the metric
		},
		[]string{"lookup3"}, // Define the label key (in this case "location")
	)

	reg.MustRegister(connAgents)
	reg.MustRegister(rcTokens)
	reg.MustRegister(memAlloc)
	if er.geoLocation != "" {
		reg.MustRegister(geo)
		reg.MustRegister(geoDataFlow)
		reg.MustRegister(numConnClientsFlow)
	} else {
		log.Warn("Geolocation not set")
	}
	geo.With(prometheus.Labels{"lookup": er.geoLocation}).Set(1)

	var m runtime.MemStats
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
			er.agentsMu.Lock()
			rcTokens.Set(float64(len(er.agents)))
			er.agentsMu.Unlock()

			runtime.ReadMemStats(&m)
			memAlloc.Set(float64(m.Alloc / 1024))
			geoDataFlow.With(prometheus.Labels{"lookup2": er.geoLocation}).Set(float64(m.Alloc / 1024))

			er.mrnToAgentMu.Lock()
			connAgents.Set(float64(len(er.mrnToAgent)))
			numConnClientsFlow.With(prometheus.Labels{"lookup3": er.geoLocation}).Set(float64(len(er.mrnToAgent)))
			er.mrnToAgentMu.Unlock()
		}
	}
}

func runPrometheusMetricsServer(ctx context.Context, wg *sync.WaitGroup, reg *prometheus.Registry) {
	defer wg.Done()

	// Start the Prometheus metrics server
	server := &http.Server{
		Addr:    ":2112",
		Handler: http.DefaultServeMux,
	}

	http.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Errorf("ListenAndServe(): %s", err)
		}
	}()

	// Listen for context cancellation to gracefully shutdown the server
	<-ctx.Done()
	log.Warn("Shutting down Prometheus server...")

	if err := server.Shutdown(context.Background()); err != nil {
		log.Errorf("Server Shutdown: %s", err)
	}
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())

	// wait for a SIGINT or SIGTERM signal
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)

	routerAddr := flag.String("raddr", "ws://localhost:8080", "The websocket URL of the Router to connect to.")
	listeningPort := flag.Int("port", 8888, "The port number that this Edge Router should listen on.")
	ownMrn := flag.String("mrn", "urn:mrn:mcp:device:idp1:org1:er", "The MRN of this Edge Router")
	clientCertPath := flag.String("client-cert", "", "Path to a client certificate which will be used to authenticate towards Router. If none is provided, mutual TLS will be disabled.")
	clientCertKeyPath := flag.String("client-cert-key", "", "Path to a client certificate private key which will be used to authenticate towards Router. If none is provided, mutual TLS will be disabled.")
	certPath := flag.String("cert-path", "", "Path to a TLS certificate file. If none is provided, TLS will be disabled.")
	certKeyPath := flag.String("cert-key-path", "", "Path to a TLS certificate private key. If none is provided, TLS will be disabled.")
	clientCAs := flag.String("client-ca", "", "Path to a file containing a list of client CAs that can connect to this Edge Router.")
	debug := flag.Bool("d", false, "Indicates whether debugging should be enabled, false by default")
	geoLoc := flag.String("l", "", "Lookup code indicating the geo location of the running instance")
	insecure := flag.Bool("i", false, "Allow insecure TLS (No validation of certificate CA)")

	flag.Parse()
	if *debug {
		log.SetLevel(log.DebugLevel)
		log.Info("Operating in Debug mode")
	} else {
		log.SetLevel(log.InfoLevel)
	}

	outgoingChannel := make(chan *mmtp.MmtpMessage, ChannelBufSize)

	var clientCertFunc func(*tls.CertificateRequestInfo) (*tls.Certificate, error)

	if *clientCertPath != "" && *clientCertKeyPath != "" {
		clientCertFunc = func(_ *tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert, err := tls.LoadX509KeyPair(*clientCertPath, *clientCertKeyPath)
			if err != nil {
				log.Error("Could not read the provided client certificate:", err)
				return nil, err
			}
			parsedCert, err := x509.ParseCertificate(cert.Certificate[0])
			if err != nil {
				log.Error("Could not parse the provided client certificate:", err)
				*ownMrn = auth.GetMrnFromCertificate(parsedCert)
			}
			return &cert, nil
		}
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				GetClientCertificate: clientCertFunc,
			},
		},
	}

	if *insecure {
		log.Warn("Insecure TLS Allowed for this Session")
		transport := httpClient.Transport.(*http.Transport)
		transport.TLSClientConfig.InsecureSkipVerify = true
	}

	routerWs, _, err := websocket.Dial(ctx, *routerAddr, &websocket.DialOptions{HTTPClient: httpClient, CompressionMode: websocket.CompressionContextTakeover})
	if err != nil {
		log.Warn("Could not connect to MMS Router")
		log.Warn("Starting EdgeRouter without connection to Router")
	} else {
		// Set the read limit to 1 MiB instead of 32 KiB
		routerWs.SetReadLimit(WsReadLimit)
	}

	wg := &sync.WaitGroup{}

	er, err := NewEdgeRouter(":"+strconv.Itoa(*listeningPort), *ownMrn, outgoingChannel, routerWs, ctx, wg, clientCAs, routerAddr, httpClient, *geoLoc)
	if err != nil {
		log.Error("Could not create MMS Edge Router instance:", err)
		return
	}

	wg.Add(1)
	go er.StartEdgeRouter(ctx, wg, certPath, certKeyPath)

	//Start monitoring
	wg.Add(2)
	reg := prometheus.NewRegistry()
	go recordMetrics(ctx, wg, reg, er)
	go runPrometheusMetricsServer(ctx, wg, reg)

	mdnsServer, err := zeroconf.Register("MMS Edge Router", "_mms-edgerouter._tcp", "local.", *listeningPort, nil, nil)
	if err != nil {
		log.Error("Could not create mDNS service, shutting down", err)
		ch <- os.Interrupt
	}
	defer mdnsServer.Shutdown()

	<-ch
	log.Warn("Received signal, shutting down...")
	cancel()
	wg.Wait()
}
