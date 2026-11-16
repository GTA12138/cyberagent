package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"testing"
	"time"

	"chengjian.local/chengjian/db"
	"golang.org/x/crypto/ssh"
)

func TestSSHSessionOpenHonorsCancellation(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	channelRequested := make(chan struct{})
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, e := ln.Accept()
		if e != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		sc, chans, reqs, e := ssh.NewServerConn(conn, cfg)
		if e != nil {
			return
		}
		defer sc.Close()
		go ssh.DiscardRequests(reqs)
		for range chans {
			close(channelRequested)
			_ = sc.Wait()
			return
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, e := sshRun(ctx, &db.CredentialRecord{CredType: db.CredPassword, Username: "test", Secret: "synthetic"}, ln.Addr().String(), "echo test", 5*time.Second, "")
		done <- e
	}()
	select {
	case <-channelRequested:
	case <-time.After(2 * time.Second):
		t.Fatal("session request not received")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("channel open ignored cancellation")
	}
	<-serverDone
}
