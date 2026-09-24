package folders_test

import (
	"context"
	"errors"
	"testing"

	"github.com/wesbragagt/syngo/internal/app/fakes"
	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/domain/device"
)

// serviceDev is a device the authorization tests pair with.
const serviceDev = device.ID("ETI3RRZAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")

// pairDevice makes a device known to the store without authorizing it for
// any folder.
func pairDevice(t *testing.T, store *fakes.Store) {
	t.Helper()
	ctx := context.Background()
	err := store.SaveDevice(ctx, device.Device{ID: serviceDev, Name: "nas"})
	if err != nil {
		t.Fatalf("save device: %v", err)
	}
}

func TestGrant_MakesAFolderReachableAndRevokeTakesItBack(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newService(t)
	if _, err := svc.Add(ctx, docsFolder("docs", "/docs")); err != nil {
		t.Fatalf("add folder: %v", err)
	}
	pairDevice(t, store)

	// A folder starts authorized for nobody.
	access, err := svc.Access(ctx, "docs")
	if err != nil {
		t.Fatalf("Access: %v", err)
	}
	if !access.Empty() {
		t.Fatalf("a new folder allows %v, want nobody", access.Devices())
	}

	if err := svc.Grant(ctx, "docs", serviceDev); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	access, err = svc.Access(ctx, "docs")
	if err != nil {
		t.Fatalf("Access after the grant: %v", err)
	}
	if !access.Allows(serviceDev) {
		t.Fatalf("the granted device is not allowed: %v", access.Devices())
	}

	if err := svc.Revoke(ctx, "docs", serviceDev); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	access, err = svc.Access(ctx, "docs")
	if err != nil {
		t.Fatalf("Access after the revocation: %v", err)
	}
	if access.Allows(serviceDev) {
		t.Fatalf("the revoked device is still allowed: %v", access.Devices())
	}
}

func TestGrantRevokeAccess_ReportNotFound(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newService(t)
	if _, err := svc.Add(ctx, docsFolder("docs", "/docs")); err != nil {
		t.Fatalf("add folder: %v", err)
	}
	pairDevice(t, store)

	cases := []struct {
		name string
		call func() error
	}{
		{"grant on an unknown folder", func() error { return svc.Grant(ctx, "nope", serviceDev) }},
		{"grant to an unpaired device", func() error {
			return svc.Grant(ctx, "docs", "ETI3RRZBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB")
		}},
		{"grant with a malformed device id", func() error { return svc.Grant(ctx, "docs", "nope") }},
		{"revoke what was never granted", func() error { return svc.Revoke(ctx, "docs", serviceDev) }},
		{"revoke with a malformed device id", func() error { return svc.Revoke(ctx, "docs", "nope") }},
		{"access of an unknown folder", func() error {
			_, err := svc.Access(ctx, "nope")
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, ports.ErrNotFound) {
				t.Fatalf("error = %v, want %v", err, ports.ErrNotFound)
			}
		})
	}
}

// TestAccess_OfAConfiguredFolderIsNeverAnErrorWhenItIsEmpty separates "no such
// folder" from "nobody is authorized": the CLI must be able to say which.
func TestAccess_OfAConfiguredFolderIsNeverAnErrorWhenItIsEmpty(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newService(t)
	if _, err := svc.Add(ctx, docsFolder("docs", "/docs")); err != nil {
		t.Fatalf("add folder: %v", err)
	}

	access, err := svc.Access(ctx, "docs")
	if err != nil {
		t.Fatalf("Access: %v", err)
	}
	if !access.Empty() {
		t.Fatalf("access = %v, want nobody", access.Devices())
	}
}
