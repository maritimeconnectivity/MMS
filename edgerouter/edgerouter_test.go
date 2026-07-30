/*
 * Copyright 2026 Maritime Connectivity Platform Consortium
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
	"crypto/x509"
	"flag"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/maritimeconnectivity/MMS/mmtp"
	"github.com/maritimeconnectivity/MMS/utils/rw"
	"github.com/stretchr/testify/require"
)

type testAgentConnection struct {
	ctx          context.Context
	conn         *websocket.Conn
	receive      chan *mmtp.MmtpMessage
	subs         map[string]*Subscription
	subMu        *sync.RWMutex
	agents       map[string]*Agent
	agentsMu     *sync.RWMutex
	mrnToAgent   map[string]*Agent
	mrnToAgentMu *sync.RWMutex
}

func newConnectMessage() *mmtp.MmtpMessage {
	return &mmtp.MmtpMessage{
		MsgType: mmtp.MsgType_PROTOCOL_MESSAGE,
		Uuid:    uuid.NewString(),
		Body: &mmtp.MmtpMessage_ProtocolMessage{
			ProtocolMessage: &mmtp.ProtocolMessage{
				ProtocolMsgType: mmtp.ProtocolMessageType_CONNECT_MESSAGE,
				Body: &mmtp.ProtocolMessage_ConnectMessage{
					ConnectMessage: &mmtp.Connect{},
				},
			},
		},
	}
}

func newSubscribeMessage(subject string) *mmtp.MmtpMessage {
	return &mmtp.MmtpMessage{
		MsgType: mmtp.MsgType_PROTOCOL_MESSAGE,
		Uuid:    uuid.NewString(),
		Body: &mmtp.MmtpMessage_ProtocolMessage{
			ProtocolMessage: &mmtp.ProtocolMessage{
				ProtocolMsgType: mmtp.ProtocolMessageType_SUBSCRIBE_MESSAGE,
				Body: &mmtp.ProtocolMessage_SubscribeMessage{
					SubscribeMessage: &mmtp.Subscribe{
						SubjectOrDirectMessages: &mmtp.Subscribe_Subject{Subject: subject},
					},
				},
			},
		},
	}
}

func newNotifyMessage(messageUUID string, header *mmtp.ApplicationMessageHeader) *mmtp.MmtpMessage {
	return &mmtp.MmtpMessage{
		MsgType: mmtp.MsgType_PROTOCOL_MESSAGE,
		Uuid:    uuid.NewString(),
		Body: &mmtp.MmtpMessage_ProtocolMessage{
			ProtocolMessage: &mmtp.ProtocolMessage{
				ProtocolMsgType: mmtp.ProtocolMessageType_NOTIFY_MESSAGE,
				Body: &mmtp.ProtocolMessage_NotifyMessage{
					NotifyMessage: &mmtp.Notify{
						MessageMetadata: []*mmtp.MessageMetadata{{
							Uuid:   messageUUID,
							Header: header,
						}},
					},
				},
			},
		},
	}
}

func newGoodResponseMessage(responseToUUID, messageUUID string, appMessage *mmtp.ApplicationMessage) *mmtp.MmtpMessage {
	return &mmtp.MmtpMessage{
		MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
		Uuid:    uuid.NewString(),
		Body: &mmtp.MmtpMessage_ResponseMessage{
			ResponseMessage: &mmtp.ResponseMessage{
				ResponseToUuid: responseToUUID,
				Response:       mmtp.ResponseEnum_GOOD,
				MessageContent: []*mmtp.MessageContent{{
					Uuid: messageUUID,
					Msg:  appMessage,
				}},
			},
		},
	}
}

func TestNewSubscriptionInitializesState(t *testing.T) {
	t.Parallel()

	sub := NewSubscription("nav.warn")

	require.Equal(t, "nav.warn", sub.Interest)
	require.NotNil(t, sub.Subscribers)
	require.Empty(t, sub.Subscribers)
	require.NotNil(t, sub.subsMu)
}

func TestSubscriptionAddAndDeleteSubscriber(t *testing.T) {
	t.Parallel()

	sub := NewSubscription("nav.warn")
	agent := &Agent{agentUuid: "agent-1"}

	sub.AddSubscriber(agent)
	require.Same(t, agent, sub.Subscribers[agent.agentUuid])

	sub.DeleteSubscriber(agent)
	require.Empty(t, sub.Subscribers)
}

func TestHandleNotifyEnqueuesReceiveMessageForNotifiedUUIDs(t *testing.T) {
	t.Parallel()

	receiveChannel := make(chan *mmtp.MmtpMessage, 1)
	er := &EdgeRouter{outgoingChannel: receiveChannel}
	metadata := []*mmtp.MessageMetadata{{Uuid: "first"}, {Uuid: "second"}}

	err := er.handleNotify(metadata)
	require.NoError(t, err)

	select {
	case msg := <-receiveChannel:
		require.Equal(t, mmtp.MsgType_PROTOCOL_MESSAGE, msg.GetMsgType())
		require.Equal(t, mmtp.ProtocolMessageType_RECEIVE_MESSAGE, msg.GetProtocolMessage().GetProtocolMsgType())
		receive := msg.GetProtocolMessage().GetReceiveMessage()
		require.NotNil(t, receive)
		require.Equal(t, []string{"first", "second"}, receive.GetFilter().GetMessageUuids())
	default:
		t.Fatal("expected handleNotify to enqueue a receive message")
	}
}

func openTestAgentConnection(t *testing.T) *testAgentConnection {
	t.Helper()

	serverCtx, cancel := context.WithCancel(context.Background())
	ioCtx, ioCancel := context.WithTimeout(context.Background(), 5*time.Second)
	receiveChannel := make(chan *mmtp.MmtpMessage, 1)
	subs := make(map[string]*Subscription)
	agents := make(map[string]*Agent)
	mrnToAgent := make(map[string]*Agent)
	subMu := &sync.RWMutex{}
	agentsMu := &sync.RWMutex{}
	mrnToAgentMu := &sync.RWMutex{}
	wg := &sync.WaitGroup{}

	server := httptest.NewServer(handleHttpConnection(receiveChannel, subs, subMu, agents, agentsMu, mrnToAgent, mrnToAgentMu, serverCtx, wg, nil))
	t.Cleanup(server.Close)
	t.Cleanup(ioCancel)
	t.Cleanup(cancel)
	t.Cleanup(func() {
		waitDone := make(chan struct{})
		go func() {
			wg.Wait()
			close(waitDone)
		}()

		select {
		case <-waitDone:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for websocket handler shutdown")
		}
	})

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.Dial(ioCtx, wsURL, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = conn.Close(websocket.StatusNormalClosure, "test complete")
	})

	return &testAgentConnection{
		ctx:          ioCtx,
		conn:         conn,
		receive:      receiveChannel,
		subs:         subs,
		subMu:        subMu,
		agents:       agents,
		agentsMu:     agentsMu,
		mrnToAgent:   mrnToAgent,
		mrnToAgentMu: mrnToAgentMu,
	}
}

func openTestRouterConnection(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()

	routerConnCh := make(chan *websocket.Conn, 1)
	errCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		conn, err := websocket.Accept(writer, request, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			errCh <- err
			return
		}

		routerConnCh <- conn
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	edgeConn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(t, err)

	var routerConn *websocket.Conn
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case routerConn = <-routerConnCh:
	case <-ctx.Done():
		t.Fatal("timed out waiting for router websocket connection")
	}

	t.Cleanup(func() {
		_ = edgeConn.Close(websocket.StatusNormalClosure, "test complete")
		_ = routerConn.Close(websocket.StatusNormalClosure, "test complete")
	})

	return edgeConn, routerConn
}

func TestHandleHttpConnectionRespondsGoodToInitialConnectMessage(t *testing.T) {
	agent := openTestAgentConnection(t)

	connectMsg := newConnectMessage()

	err := rw.WriteMessage(agent.ctx, agent.conn, connectMsg)
	require.NoError(t, err)

	response, _, err := rw.ReadMessage(agent.ctx, agent.conn)
	require.NoError(t, err)
	require.Equal(t, mmtp.MsgType_RESPONSE_MESSAGE, response.GetMsgType())
	require.Equal(t, connectMsg.GetUuid(), response.GetResponseMessage().GetResponseToUuid())
	require.Equal(t, mmtp.ResponseEnum_GOOD, response.GetResponseMessage().GetResponse())
}

func TestSubscribeAndNotifyQueuesRouterMessageForSubscribedAgent(t *testing.T) {
	agentConn := openTestAgentConnection(t)
	edgeRouterConn, routerConn := openTestRouterConnection(t)

	connectMsg := newConnectMessage()
	require.NoError(t, rw.WriteMessage(agentConn.ctx, agentConn.conn, connectMsg))

	connectResp, _, err := rw.ReadMessage(agentConn.ctx, agentConn.conn)
	require.NoError(t, err)
	require.Equal(t, mmtp.ResponseEnum_GOOD, connectResp.GetResponseMessage().GetResponse())

	const subject = "urn:mrn:mcp:msr:search:global"
	subscribeMsg := newSubscribeMessage(subject)
	require.NoError(t, rw.WriteMessage(agentConn.ctx, agentConn.conn, subscribeMsg))

	subscribeResp, _, err := rw.ReadMessage(agentConn.ctx, agentConn.conn)
	require.NoError(t, err)
	require.Equal(t, mmtp.ResponseEnum_GOOD, subscribeResp.GetResponseMessage().GetResponse())
	require.Equal(t, subscribeMsg.GetUuid(), subscribeResp.GetResponseMessage().GetResponseToUuid())

	var forwardedSubscribe *mmtp.MmtpMessage
	select {
	case forwardedSubscribe = <-agentConn.receive:
	case <-agentConn.ctx.Done():
		t.Fatal("timed out waiting for forwarded subscribe message")
	}
	require.Equal(t, mmtp.ProtocolMessageType_SUBSCRIBE_MESSAGE, forwardedSubscribe.GetProtocolMessage().GetProtocolMsgType())
	require.Equal(t, subject, forwardedSubscribe.GetProtocolMessage().GetSubscribeMessage().GetSubject())

	edgeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wg := &sync.WaitGroup{}
	wg.Add(1)
	t.Cleanup(func() {
		waitDone := make(chan struct{})
		go func() {
			wg.Wait()
			close(waitDone)
		}()

		select {
		case <-waitDone:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for incoming router handler shutdown")
		}
	})

	edgeRouter := &EdgeRouter{
		subscriptions:   agentConn.subs,
		subMu:           agentConn.subMu,
		agents:          agentConn.agents,
		agentsMu:        agentConn.agentsMu,
		mrnToAgent:      agentConn.mrnToAgent,
		mrnToAgentMu:    agentConn.mrnToAgentMu,
		outgoingChannel: agentConn.receive,
		routerWs:        edgeRouterConn,
		awaitResponse:   make(map[string]*mmtp.MmtpMessage),
		responseMu:      &sync.RWMutex{},
		wsMu:            &sync.RWMutex{},
	}
	go handleIncomingMessages(edgeCtx, edgeRouter, wg)

	expires := time.Now().Add(time.Minute).Unix()
	messageUUID := uuid.NewString()
	appMessage := &mmtp.ApplicationMessage{
		Header: &mmtp.ApplicationMessageHeader{
			SubjectOrRecipient: &mmtp.ApplicationMessageHeader_Subject{Subject: subject},
			Expires:            expires,
			Sender:             "urn:mrn:mcp:device:test:sender",
			BodySizeNumBytes:   4,
		},
		Body: []byte("body"),
	}
	notifyMsg := newNotifyMessage(messageUUID, appMessage.GetHeader())
	require.NoError(t, rw.WriteMessage(agentConn.ctx, routerConn, notifyMsg))

	var receiveMsg *mmtp.MmtpMessage
	select {
	case receiveMsg = <-agentConn.receive:
	case <-agentConn.ctx.Done():
		t.Fatal("timed out waiting for receive message")
	}
	require.Equal(t, mmtp.ProtocolMessageType_RECEIVE_MESSAGE, receiveMsg.GetProtocolMessage().GetProtocolMsgType())
	require.Equal(t, []string{messageUUID}, receiveMsg.GetProtocolMessage().GetReceiveMessage().GetFilter().GetMessageUuids())

	routerResponse := newGoodResponseMessage(receiveMsg.GetUuid(), messageUUID, appMessage)
	require.NoError(t, rw.WriteMessage(agentConn.ctx, routerConn, routerResponse))

	var subscribedAgent *Agent
	require.Eventually(t, func() bool {
		agentConn.subMu.RLock()
		defer agentConn.subMu.RUnlock()
		for _, sub := range agentConn.subs {
			for _, candidate := range sub.SnapshotSubscribers() {
				subscribedAgent = candidate
				return true
			}
		}
		return false
	}, time.Second, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		subscribedAgent.MsgMu.RLock()
		defer subscribedAgent.MsgMu.RUnlock()
		return len(subscribedAgent.Messages) == 1
	}, time.Second, 10*time.Millisecond)

	subscribedAgent.MsgMu.RLock()
	var queuedMessage *mmtp.MmtpMessage
	for _, message := range subscribedAgent.Messages {
		queuedMessage = message
		break
	}
	subscribedAgent.MsgMu.RUnlock()
	require.NotNil(t, queuedMessage)
	require.NotEmpty(t, queuedMessage.GetUuid())
	require.Equal(t, mmtp.MsgType_PROTOCOL_MESSAGE, queuedMessage.GetMsgType())
	require.Equal(t, mmtp.ProtocolMessageType_SEND_MESSAGE, queuedMessage.GetProtocolMessage().GetProtocolMsgType())
	require.Equal(t, subject, queuedMessage.GetProtocolMessage().GetSendMessage().GetApplicationMessage().GetHeader().GetSubject())
	require.Equal(t, appMessage.GetBody(), queuedMessage.GetProtocolMessage().GetSendMessage().GetApplicationMessage().GetBody())
}

func TestVerifyAgentCertificate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                  string
		skipRevocationCheck   bool
		rawCerts              [][]byte
		verifiedChains        [][]*x509.Certificate
		expectedErrorContains string
	}{
		{
			name:                "returns nil without certificates",
			skipRevocationCheck: false,
		},
		{
			name:                "allows missing revocation endpoints when skipped",
			skipRevocationCheck: true,
			rawCerts:            [][]byte{{1}},
			verifiedChains:      [][]*x509.Certificate{{&x509.Certificate{}, &x509.Certificate{}}},
		},
		{
			name:                  "rejects missing revocation endpoints by default",
			skipRevocationCheck:   false,
			rawCerts:              [][]byte{{1}},
			verifiedChains:        [][]*x509.Certificate{{&x509.Certificate{}, &x509.Certificate{}}},
			expectedErrorContains: "was not able to check revocation status of client certificate",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			verifier := verifyAgentCertificate(test.skipRevocationCheck)
			err := verifier(test.rawCerts, test.verifiedChains)

			if test.expectedErrorContains == "" {
				require.NoError(t, err)
				return
			}

			require.ErrorContains(t, err, test.expectedErrorContains)
		})
	}
}

func TestRunReturnsErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                  string
		args                  func(*testing.T) []string
		expectedErrorContains string
	}{
		{
			name: "invalid TLS CA path",
			args: func(t *testing.T) []string {
				return []string{"-tlsca", filepath.Join(t.TempDir(), "missing-ca.pem")}
			},
			expectedErrorContains: "could not load configured TLS CAs",
		},
		{
			name: "invalid client CA path",
			args: func(t *testing.T) []string {
				return []string{
					"-raddr", "ws://127.0.0.1:1",
					"-client-ca", filepath.Join(t.TempDir(), "missing-client-ca.pem"),
				}
			},
			expectedErrorContains: "could not create MMS Edge Router instance",
		},
		{
			name: "unknown flag",
			args: func(*testing.T) []string {
				return []string{"-unknown-flag"}
			},
			expectedErrorContains: "flag provided but not defined: -unknown-flag",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := run(test.args(t))
			require.Error(t, err)
			require.ErrorContains(t, err, test.expectedErrorContains)
		})
	}
}

func TestRunHelpReturnsErrHelpAndUsage(t *testing.T) {
	var stderr bytes.Buffer

	err := runWithStderr([]string{"-h"}, &stderr)
	require.ErrorIs(t, err, flag.ErrHelp)

	require.Contains(t, stderr.String(), "Usage of edgerouter:")
	require.Contains(t, stderr.String(), "-port int")
}

func TestRunMainTreatsErrHelpAsSuccess(t *testing.T) {
	require.Equal(t, 0, runMain([]string{"-h"}))
}
