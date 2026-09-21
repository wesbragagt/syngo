package compose_test

import (
	"path/filepath"
	"testing"

	"github.com/wesbragagt/syncthis/internal/compose"
)

func TestResolveDataDir_Precedence(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	env := t.TempDir()
	flag := t.TempDir()

	tests := []struct {
		name string
		env  string
		xdg  string
		flag string
		want string
	}{
		{"flag wins", env, xdg, flag, flag},
		{"env over xdg", env, xdg, "", env},
		{"xdg over home", "", xdg, "", filepath.Join(xdg, "syncthis")},
		{"home fallback", "", "", "", filepath.Join(home, ".local", "share", "syncthis")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", home)
			t.Setenv(compose.EnvDataDir, tt.env)
			t.Setenv("XDG_DATA_HOME", tt.xdg)

			got, err := compose.ResolveDataDir(tt.flag)
			if err != nil {
				t.Fatalf("ResolveDataDir error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("ResolveDataDir = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveDataDir_MakesPathAbsolute(t *testing.T) {
	got, err := compose.ResolveDataDir("relative/data")
	if err != nil {
		t.Fatalf("ResolveDataDir error: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("ResolveDataDir = %q, want an absolute path", got)
	}
}
