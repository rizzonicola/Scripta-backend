package handlers

import (
	"context"
	"testing"
	"time"
)

func TestUserLockSetSerializesSameUserOnly(t *testing.T) {
	var locks userLockSet
	ctx := context.Background()

	releaseA1, ok := locks.acquire(ctx, "tenant-a")
	if !ok {
		t.Fatal("prima acquisizione tenant-a fallita")
	}

	// Lo stesso utente non deve entrare finché il primo lock è detenuto.
	acquired := make(chan struct{})
	go func() {
		release, ok := locks.acquire(ctx, "tenant-a")
		if !ok {
			return
		}
		close(acquired)
		release()
	}()
	select {
	case <-acquired:
		t.Fatal("due richieste dello stesso utente hanno acquisito il lock insieme")
	case <-time.After(50 * time.Millisecond):
	}

	// Un altro tenant non deve invece aspettare il primo.
	releaseB, ok := locks.acquire(ctx, "tenant-b")
	if !ok {
		t.Fatal("acquisizione tenant-b bloccata da tenant-a")
	}
	releaseB()

	releaseA1()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("il waiter dello stesso utente non è stato riattivato dopo il rilascio")
	}
}

func TestUserLockSetAcquireHonorsContextCancellation(t *testing.T) {
	var locks userLockSet
	release, ok := locks.acquire(context.Background(), "tenant-a")
	if !ok {
		t.Fatal("prima acquisizione fallita")
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := locks.acquire(ctx, "tenant-a"); ok {
		t.Fatal("acquisizione riuscita con context già annullato")
	}
}
