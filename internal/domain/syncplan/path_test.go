package syncplan_test

import (
	"testing"

	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/syncplan"
)

// TestPlan_SkipsPathsOutsideTheFolder proves the planner never asks for an
// entry a peer may not name, whatever handed it the manifest.
func TestPlan_SkipsPathsOutsideTheFolder(t *testing.T) {
	hostile := []string{
		"..", "../escape.txt", "../../escape.txt", "a/../../escape.txt",
		"/etc/passwd", ".", "./a", ".syncthis/tmp/x", ".syncthisignore", "a\x00b",
	}
	for _, p := range hostile {
		t.Run(p, func(t *testing.T) {
			remote := manifestOf(file(p, "v1", vec("A:1")))
			item := only(t, syncplan.Plan(manifestOf(), remote, testFolder()))
			if item.Action != syncplan.ActionNone || item.Reason != syncplan.ReasonInvalidPath {
				t.Fatalf("item = %s/%s, want none/%s", item.Action, item.Reason, syncplan.ReasonInvalidPath)
			}
		})
	}
}

func TestPlan_SkipsATombstoneOutsideTheFolder(t *testing.T) {
	remote := manifestOf(tombstone("../escape.txt", vec("A:1")))
	item := only(t, syncplan.Plan(manifestOf(), remote, testFolder()))
	if item.Action != syncplan.ActionNone || item.Reason != syncplan.ReasonInvalidPath {
		t.Fatalf("item = %s/%s, want none/%s", item.Action, item.Reason, syncplan.ReasonInvalidPath)
	}
}

func TestPlan_SkipsASymlinkLeavingTheFolder(t *testing.T) {
	for _, target := range []string{"/etc", "/tmp/outside", "../outside"} {
		t.Run(target, func(t *testing.T) {
			remote := manifestOf(manifest.Entry{
				Path: "link", Kind: manifest.KindSymlink, LinkTarget: target,
			})
			item := only(t, syncplan.Plan(manifestOf(), remote, testFolder()))
			if item.Action != syncplan.ActionNone || item.Reason != syncplan.ReasonInvalidPath {
				t.Fatalf("item = %s/%s, want none/%s", item.Action, item.Reason, syncplan.ReasonInvalidPath)
			}
		})
	}
}

func TestPlan_KeepsValidUnicodePaths(t *testing.T) {
	remote := manifestOf(file("documentos/café/résumé.txt", "v1", vec("A:1")))
	item := only(t, syncplan.Plan(manifestOf(), remote, testFolder()))
	if item.Action != syncplan.ActionPull {
		t.Fatalf("item = %s/%s, want pull", item.Action, item.Reason)
	}
}
