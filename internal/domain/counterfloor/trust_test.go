package counterfloor_test

import (
	"sync"
	"testing"

	"github.com/wesbragagt/syncthis/internal/domain/counterfloor"
)

func TestTrust_StartsUnverified(t *testing.T) {
	trust := counterfloor.New()
	if trust.Verified() {
		t.Fatal("a fresh trust reads as verified, want every process start to doubt the floor")
	}
	trust.Verify()
	if !trust.Verified() {
		t.Fatal("the trust still reads as unverified after a peer confirmed the floor")
	}
	// A second contact says nothing new.
	trust.Verify()
	if !trust.Verified() {
		t.Fatal("verifying twice unset the trust")
	}
}

// TestTrust_NilReadsAsVerified keeps a caller that does not track the question
// behaving exactly as it did before the floor was ever doubted.
func TestTrust_NilReadsAsVerified(t *testing.T) {
	var trust *counterfloor.Trust
	if !trust.Verified() {
		t.Fatal("a nil trust reads as unverified, want it to read as verified")
	}
	trust.Verify() // must not panic
}

// TestTrust_ConcurrentUse mirrors how it is used: one puller writes it on
// first contact while the scanners of every folder read it.
func TestTrust_ConcurrentUse(t *testing.T) {
	trust := counterfloor.New()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_ = trust.Verified()
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			trust.Verify()
		}()
	}
	wg.Wait()
	if !trust.Verified() {
		t.Fatal("the trust is unverified after every writer ran")
	}
}
