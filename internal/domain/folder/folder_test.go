package folder_test

import (
	"errors"
	"testing"

	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

func valid() folder.Folder {
	return folder.Folder{
		ID:            "docs",
		Path:          "/home/u/docs",
		Mode:          folder.ModeSendReceive,
		DeletePolicy:  folder.DeletePropagate,
		Symlinks:      folder.SymlinkSkip,
		KeepEmptyDirs: true,
	}
}

func TestValidate_RejectsRelativePath(t *testing.T) {
	tests := []struct {
		name string
		path string
		ok   bool
	}{
		{"absolute", "/home/u/docs", true},
		{"relative", "docs", false},
		{"dot relative", "./docs", false},
		{"empty", "", false},
		{"uncleaned", "/home/u/../u/docs", false},
		{"trailing slash", "/home/u/docs/", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := valid()
			f.Path = tt.path
			err := f.Validate()
			if tt.ok && err != nil {
				t.Fatalf("Validate() error = %v, want nil", err)
			}
			if !tt.ok && err == nil {
				t.Fatalf("Validate() = nil, want error for path %q", tt.path)
			}
		})
	}
}

func TestValidate_RejectsBadID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		ok   bool
	}{
		{"simple", "docs", true},
		{"digits and punctuation", "a0._-", true},
		{"empty", "", false},
		{"uppercase", "Docs", false},
		{"leading dash", "-docs", false},
		{"space", "my docs", false},
		{"too long", string(make([]byte, 65)), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := valid()
			f.ID = folder.ID(tt.id)
			err := f.Validate()
			if tt.ok && err != nil {
				t.Fatalf("Validate() error = %v, want nil", err)
			}
			if !tt.ok && !errors.Is(err, folder.ErrInvalidID) {
				t.Fatalf("Validate() error = %v, want ErrInvalidID", err)
			}
		})
	}
}

func TestCanReceive_CanSend_ByMode(t *testing.T) {
	tests := []struct {
		mode        folder.Mode
		canReceive  bool
		canSend     bool
		validConfig bool
	}{
		{folder.ModeSendReceive, true, true, true},
		{folder.ModeSendOnly, false, true, true},
		{folder.ModeReceiveOnly, true, false, true},
		{folder.Mode("bogus"), true, true, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			f := valid()
			f.Mode = tt.mode
			if got := f.CanReceive(); got != tt.canReceive {
				t.Errorf("CanReceive() = %v, want %v", got, tt.canReceive)
			}
			if got := f.CanSend(); got != tt.canSend {
				t.Errorf("CanSend() = %v, want %v", got, tt.canSend)
			}
			if got := f.Validate() == nil; got != tt.validConfig {
				t.Errorf("Validate() == nil is %v, want %v", got, tt.validConfig)
			}
		})
	}
}
