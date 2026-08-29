package controller

import (
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestSelectNode(t *testing.T) {
	nodes := []corev1.Node{*readyNode("node-c", ""), *readyNode("node-a", ""), *readyNode("node-b", "")}

	tests := []struct {
		name     string
		occupied map[string]bool
		want     func(picked string) bool
	}{
		{
			name:     "prefers unoccupied nodes",
			occupied: map[string]bool{"node-a": true, "node-c": true},
			want:     func(picked string) bool { return picked == "node-b" },
		},
		{
			name:     "falls back to occupied when all taken",
			occupied: map[string]bool{"node-a": true, "node-b": true, "node-c": true},
			want:     func(picked string) bool { return picked != "" },
		},
		{
			name:     "no occupancy",
			occupied: map[string]bool{},
			want:     func(picked string) bool { return picked != "" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			picked := selectNode("group-a", nodes, tt.occupied)
			if !tt.want(picked.Name) {
				t.Errorf("selectNode picked %q", picked.Name)
			}
			if again := selectNode("group-a", nodes, tt.occupied); again.Name != picked.Name {
				t.Errorf("selectNode is not deterministic: %q then %q", picked.Name, again.Name)
			}
		})
	}

	t.Run("does not mutate the input slice", func(t *testing.T) {
		selectNode("group-a", nodes, nil)
		got := []string{nodes[0].Name, nodes[1].Name, nodes[2].Name}
		want := []string{"node-c", "node-a", "node-b"}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("input slice reordered: %v, want %v", got, want)
			}
		}
	})

	t.Run("spreads groups across nodes", func(t *testing.T) {
		seen := map[string]bool{}
		occupied := map[string]bool{}
		for i := range 3 {
			picked := selectNode(fmt.Sprintf("group-%d", i), nodes, occupied)
			seen[picked.Name] = true
			occupied[picked.Name] = true
		}
		if len(seen) != 3 {
			t.Errorf("expected 3 groups on 3 distinct nodes, got %d", len(seen))
		}
	})
}

func TestNodeByName(t *testing.T) {
	nodes := []corev1.Node{*readyNode("node-a", ""), *readyNode("node-b", "")}

	tests := []struct {
		name   string
		lookup string
		want   string
	}{
		{name: "found", lookup: "node-b", want: "node-b"},
		{name: "absent", lookup: "node-x", want: ""},
		{name: "empty name", lookup: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nodeByName(nodes, tt.lookup); got.Name != tt.want {
				t.Errorf("nodeByName(%q).Name = %q, want %q", tt.lookup, got.Name, tt.want)
			}
		})
	}
}

func TestIsNodeReady(t *testing.T) {
	tests := []struct {
		name string
		node *corev1.Node
		want bool
	}{
		{name: "ready", node: readyNode("n", ""), want: true},
		{name: "not ready", node: notReadyNode("n"), want: false},
		{name: "no conditions", node: &corev1.Node{}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNodeReady(tt.node); got != tt.want {
				t.Errorf("isNodeReady = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNodePredicate(t *testing.T) {
	pred := nodePredicate()

	heartbeat := readyNode("n", "srv")
	heartbeat.Status.Conditions[0].LastHeartbeatTime = metav1.Now()

	annotated := readyNode("n", "other-srv")

	relabeled := readyNode("n", "srv")
	relabeled.Labels = map[string]string{"role": "ingress"}

	tests := []struct {
		name     string
		old, new *corev1.Node
		want     bool
	}{
		{name: "heartbeat only", old: readyNode("n", "srv"), new: heartbeat, want: false},
		{name: "readiness flip", old: readyNode("n", "srv"), new: notReadyNode("n"), want: true},
		{name: "annotation change", old: readyNode("n", "srv"), new: annotated, want: true},
		{name: "label change", old: readyNode("n", "srv"), new: relabeled, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pred.Update(event.UpdateEvent{ObjectOld: tt.old, ObjectNew: tt.new})
			if got != tt.want {
				t.Errorf("predicate = %v, want %v", got, tt.want)
			}
		})
	}
}
