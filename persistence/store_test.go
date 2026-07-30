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

package persistence

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/maritimeconnectivity/MMS/mmtp"
)

func TestSessionTokenRotationAndRecovery(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	session := Session{
		ID:        "session-1",
		Kind:      SessionKindAgent,
		MRN:       "urn:mrn:test:agent",
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	}
	if err := store.UpsertSession(ctx, session, "first-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SessionByToken(ctx, SessionKindAgent, "first-token"); err != nil {
		t.Fatalf("look up initial token: %v", err)
	}
	if err := store.RotateReconnectToken(ctx, session.ID, "second-token", now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SessionByToken(ctx, SessionKindAgent, "first-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old token lookup error = %v, want ErrNotFound", err)
	}
	recovered, err := store.SessionByToken(ctx, SessionKindAgent, "second-token")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ID != session.ID {
		t.Fatalf("recovered ID = %q, want %q", recovered.ID, session.ID)
	}
}

func TestDurableDeliveryLifecycle(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	session := Session{
		ID:        "session-1",
		Kind:      SessionKindAgent,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	}
	if err := store.UpsertSession(ctx, session, "token"); err != nil {
		t.Fatal(err)
	}
	message := testMessage("message-1", now.Add(time.Hour))
	if err := store.QueueMessage(ctx, []string{session.ID}, message); err != nil {
		t.Fatal(err)
	}
	notifications, err := store.PendingNotifications(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 1 || notifications[0].GetUuid() != message.GetUuid() {
		t.Fatalf("pending notifications = %#v", notifications)
	}
	if err = store.MarkNotified(ctx, session.ID, []string{message.GetUuid()}); err != nil {
		t.Fatal(err)
	}
	notifications, err = store.PendingNotifications(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 0 {
		t.Fatalf("pending notifications after mark = %d, want 0", len(notifications))
	}
	messages, err := store.FetchMessages(ctx, session.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("fetched messages = %d, want 1", len(messages))
	}
	if err = store.DeleteDeliveries(ctx, session.ID, []string{message.GetUuid()}); err != nil {
		t.Fatal(err)
	}
	messages, err = store.FetchMessages(ctx, session.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Fatalf("fetched messages after delivery = %d, want 0", len(messages))
	}
}

func TestStateSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	session := Session{
		ID:        "session-1",
		Kind:      SessionKindEdgeRouter,
		MRN:       "urn:mrn:test:edge-router",
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	}
	if err = store.UpsertSession(ctx, session, "token"); err != nil {
		t.Fatal(err)
	}
	if err = store.Subscribe(ctx, session.ID, "weather"); err != nil {
		t.Fatal(err)
	}
	message := testMessage("message-1", now.Add(time.Hour))
	if err = store.QueueMessage(ctx, []string{session.ID}, message); err != nil {
		t.Fatal(err)
	}
	if err = store.PutOutbox(ctx, message); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	recovered, err := store.SessionByToken(ctx, SessionKindEdgeRouter, "token")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ID != session.ID {
		t.Fatalf("recovered session ID = %q, want %q", recovered.ID, session.ID)
	}
	subscriptions, err := store.Subscriptions(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(subscriptions) != 1 || subscriptions[0] != "weather" {
		t.Fatalf("subscriptions = %#v", subscriptions)
	}
	messages, err := store.FetchMessages(ctx, session.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].GetUuid() != message.GetUuid() {
		t.Fatalf("messages after reopen = %#v", messages)
	}
	outbox, err := store.Outbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(outbox) != 1 || outbox[0].GetUuid() != message.GetUuid() {
		t.Fatalf("outbox after reopen = %#v", outbox)
	}
}

func openTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err = store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func testMessage(id string, expires time.Time) *mmtp.MmtpMessage {
	return &mmtp.MmtpMessage{
		MsgType: mmtp.MsgType_PROTOCOL_MESSAGE,
		Uuid:    id,
		Body: &mmtp.MmtpMessage_ProtocolMessage{
			ProtocolMessage: &mmtp.ProtocolMessage{
				ProtocolMsgType: mmtp.ProtocolMessageType_SEND_MESSAGE,
				Body: &mmtp.ProtocolMessage_SendMessage{
					SendMessage: &mmtp.Send{
						ApplicationMessage: &mmtp.ApplicationMessage{
							Header: &mmtp.ApplicationMessageHeader{
								Expires: expires.Unix(),
							},
						},
					},
				},
			},
		},
	}
}
