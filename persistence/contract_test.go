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
)

func TestStoreContract(t *testing.T) {
	factories := map[string]func(*testing.T) Store{
		"memory": func(*testing.T) Store {
			return NewMemoryStore()
		},
		"sqlite": func(t *testing.T) Store {
			store, err := Open(filepath.Join(t.TempDir(), "contract.db"))
			if err != nil {
				t.Fatal(err)
			}
			return store
		},
	}

	for name, factory := range factories {
		t.Run(name, func(t *testing.T) {
			runStoreContract(t, factory(t))
		})
	}
}

func runStoreContract(t *testing.T, store Store) {
	t.Helper()
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})

	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	session := Session{
		ID:        "session-1",
		Kind:      SessionKindAgent,
		MRN:       "URN:MRN:TEST:AGENT",
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	}
	if err := store.UpsertSession(ctx, session, "first-token"); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.SessionByToken(ctx, SessionKindAgent, "first-token")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.MRN != "urn:mrn:test:agent" {
		t.Fatalf("canonical MRN = %q", recovered.MRN)
	}
	if err = store.RotateReconnectToken(ctx, session.ID, "second-token", now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SessionByToken(ctx, SessionKindAgent, "first-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old token lookup error = %v, want ErrNotFound", err)
	}

	if err = store.Subscribe(ctx, session.ID, "weather"); err != nil {
		t.Fatal(err)
	}
	if err = store.SetDirectMessages(ctx, session.ID, true); err != nil {
		t.Fatal(err)
	}
	subscriptions, err := store.Subscriptions(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(subscriptions) != 1 || subscriptions[0] != "weather" {
		t.Fatalf("subscriptions = %#v", subscriptions)
	}

	message := testMessage("message-1", now.Add(time.Hour))
	if err = store.QueueMessage(ctx, []string{session.ID}, message); err != nil {
		t.Fatal(err)
	}
	notifications, err := store.PendingNotifications(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 1 || notifications[0].GetUuid() != message.GetUuid() {
		t.Fatalf("notifications = %#v", notifications)
	}
	if err = store.MarkNotified(ctx, session.ID, []string{message.GetUuid()}); err != nil {
		t.Fatal(err)
	}
	notifications, err = store.PendingNotifications(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 0 {
		t.Fatalf("notifications after mark = %d, want 0", len(notifications))
	}
	messages, err := store.FetchMessages(ctx, session.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].GetUuid() != message.GetUuid() {
		t.Fatalf("messages = %#v", messages)
	}

	if err = store.PutOutbox(ctx, message); err != nil {
		t.Fatal(err)
	}
	outbox, err := store.Outbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(outbox) != 1 || outbox[0].GetUuid() != message.GetUuid() {
		t.Fatalf("outbox = %#v", outbox)
	}
	if err = store.DeleteOutbox(ctx, message.GetUuid()); err != nil {
		t.Fatal(err)
	}

	if err = store.SetSetting(ctx, "router-token", "value"); err != nil {
		t.Fatal(err)
	}
	value, err := store.Setting(ctx, "router-token")
	if err != nil || value != "value" {
		t.Fatalf("setting = %q, %v", value, err)
	}
	if err = store.DeleteSetting(ctx, "router-token"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Setting(ctx, "router-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted setting error = %v, want ErrNotFound", err)
	}

	if err = store.DeleteDeliveries(ctx, session.ID, []string{message.GetUuid()}); err != nil {
		t.Fatal(err)
	}
	if err = store.Unsubscribe(ctx, session.ID, "weather"); err != nil {
		t.Fatal(err)
	}
	if err = store.DeleteSession(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SessionByID(ctx, session.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted session error = %v, want ErrNotFound", err)
	}
}
