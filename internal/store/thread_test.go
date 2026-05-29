package store

import (
	"context"
	"errors"
	"testing"
)

func TestGetThread_LinearChain(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	a, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "ping"})
	b, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "bob", ToAgent: "alice", ReplyTo: a.PublicID, Body: "pong"})
	c, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", ReplyTo: b.PublicID, Body: "thanks"})

	// Walk from any node should yield the whole chain.
	for _, start := range []string{a.PublicID, b.PublicID, c.PublicID} {
		got, err := s.GetThread(ctx, start)
		if err != nil {
			t.Fatalf("from %s: %v", start, err)
		}
		if len(got) != 3 {
			t.Fatalf("from %s: len = %d, want 3", start, len(got))
		}
		want := []string{a.PublicID, b.PublicID, c.PublicID}
		for i, m := range got {
			if m.PublicID != want[i] {
				t.Errorf("from %s [%d] = %s, want %s", start, i, m.PublicID, want[i])
			}
		}
	}
}

func TestGetThread_BranchingReplies(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	root, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "all", Body: "ping"})
	r1, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "bob", ToAgent: "alice", ReplyTo: root.PublicID, Body: "reply1"})
	r2, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "carol", ToAgent: "alice", ReplyTo: root.PublicID, Body: "reply2"})

	got, err := s.GetThread(ctx, root.PublicID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	// Order is by id ascending; root then the two replies in insert order.
	want := []string{root.PublicID, r1.PublicID, r2.PublicID}
	for i, m := range got {
		if m.PublicID != want[i] {
			t.Errorf("[%d] = %s, want %s", i, m.PublicID, want[i])
		}
	}
}

func TestGetThread_StartingFromUnknownID(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetThread(context.Background(), "deadbeef")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestGetThread_SingletonNoReplies(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	root, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "one message"})

	got, err := s.GetThread(ctx, root.PublicID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if len(got) != 1 || got[0].PublicID != root.PublicID {
		t.Errorf("got %v, want only %s", got, root.PublicID)
	}
}

func TestGetThread_DeepChain(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	prev, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "a", ToAgent: "b", Body: "0"})
	for i := 1; i < 10; i++ {
		next, _ := s.InsertMessage(ctx, InsertParams{
			FromAgent: "a", ToAgent: "b",
			ReplyTo: prev.PublicID,
			Body:    "msg",
		})
		prev = next
	}

	got, err := s.GetThread(ctx, prev.PublicID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if len(got) != 10 {
		t.Errorf("len = %d, want 10", len(got))
	}
}
