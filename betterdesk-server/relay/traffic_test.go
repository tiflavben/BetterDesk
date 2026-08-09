package relay

import (
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/unitronix/betterdesk-server/codec"
	"github.com/unitronix/betterdesk-server/config"
	pb "github.com/unitronix/betterdesk-server/proto"
)

// TestCountingConn verifies that countingConn tallies bytes in both the read
// and write directions through a real net.Conn.
func TestCountingConn(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	cc := &countingConn{Conn: a}

	// Read direction: bytes arriving from the peer are counted.
	writeErr := make(chan error, 1)
	go func() {
		_, err := b.Write([]byte("hello")) // 5 bytes
		writeErr <- err
	}()
	buf := make([]byte, 16)
	if _, err := io.ReadFull(cc, buf[:5]); err != nil {
		t.Fatalf("read through countingConn: %v", err)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("peer write: %v", err)
	}
	if got := cc.Bytes(); got != 5 {
		t.Fatalf("bytes after read: got %d, want 5", got)
	}

	// Write direction: bytes sent to the peer are counted.
	go func() {
		_, err := cc.Write([]byte("world")) // 5 bytes
		writeErr <- err
	}()
	if _, err := io.ReadFull(b, buf[:5]); err != nil {
		t.Fatalf("peer read: %v", err)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("write through countingConn: %v", err)
	}
	if got := cc.Bytes(); got != 10 {
		t.Fatalf("bytes after write: got %d, want 10", got)
	}

	// Concurrent read/write from separate goroutines must not lose counts.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = cc.Write(make([]byte, 100))
		}()
		go func() {
			defer wg.Done()
			_, _ = io.CopyN(io.Discard, b, 100)
		}()
	}
	wg.Wait()
	if got := cc.Bytes(); got != 410 {
		t.Fatalf("bytes after concurrent traffic: got %d, want 410", got)
	}
}

// recordingSink captures RecordTraffic calls for assertions.
type recordingSink struct {
	mu      sync.Mutex
	records map[string]int64
}

func (s *recordingSink) RecordTraffic(uuid string, bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[uuid] = bytes
}

func (s *recordingSink) get(uuid string) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.records[uuid]
	return b, ok
}

// TestRelayTrafficSinkReportsBytes runs a full relay session through the real
// TCP path and verifies the traffic sink receives the session UUID and the
// exact total number of bytes relayed in both directions.
func TestRelayTrafficSinkReportsBytes(t *testing.T) {
	cfg := config.DefaultConfig()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	cfg.RelayPort = port
	srv := New(cfg)
	sink := &recordingSink{records: make(map[string]int64)}
	srv.SetTrafficSink(sink)

	if err := srv.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Stop()

	uuid := "traffic-sink-uuid-1"
	authorizeTestRelayPair(t, uuid)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	dialAndRequest := func(id string) net.Conn {
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.Fatalf("dial %s: %v", id, err)
		}
		req := &pb.RendezvousMessage{
			Union: &pb.RendezvousMessage_RequestRelay{
				RequestRelay: &pb.RequestRelay{Uuid: uuid, Id: id},
			},
		}
		if err := codec.WriteRawProto(conn, req); err != nil {
			t.Fatalf("write %s: %v", id, err)
		}
		return conn
	}

	connA := dialAndRequest("PEER_A")
	connB := dialAndRequest("PEER_B")
	defer connA.Close()
	defer connB.Close()

	waitRelayPairing(t, srv)

	// A -> B: 12 bytes, fully flushed (peer reads them all).
	msg := []byte("hello-from-A")
	connA.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := connA.Write(msg); err != nil {
		t.Fatalf("write A->B: %v", err)
	}
	buf := make([]byte, 64)
	connB.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(connB, buf[:len(msg)]); err != nil {
		t.Fatalf("read A->B: %v", err)
	}

	// B -> A: 9 bytes, fully flushed.
	reply := []byte("hi-from-B")
	connB.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := connB.Write(reply); err != nil {
		t.Fatalf("write B->A: %v", err)
	}
	connA.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(connA, buf[:len(reply)]); err != nil {
		t.Fatalf("read B->A: %v", err)
	}

	// Closing both sides ends the session; each byte is counted once on the
	// read side of its source conn and once on the write side of its target
	// conn, so the reported total is 2 * (len(msg) + len(reply)).
	connA.Close()
	connB.Close()

	want := int64(2 * (len(msg) + len(reply)))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got, ok := sink.get(uuid); ok {
			if got != want {
				t.Fatalf("RecordTraffic bytes: got %d, want %d", got, want)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("traffic sink was not called for %q within 3s (records: %v)", uuid, sink.records)
}
