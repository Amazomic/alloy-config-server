package main

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	collectorv1 "github.com/grafana/alloy-remote-config/api/gen/proto/go/collector/v1"
)

func TestCollectorInactiveRetention(t *testing.T) {
	for _, tc := range []struct {
		name          string
		lastSeenAgo   time.Duration
		disconnected  bool
		disconnectAgo time.Duration
		visible       bool
		active        bool
	}{
		{"active", time.Minute, false, 0, true, true},
		{"silent inactive", 6 * time.Minute, false, 0, true, false},
		{"silent expired", 10 * time.Minute, false, 0, false, false},
		{"disconnected", time.Minute, true, time.Minute, true, false},
		{"disconnect expired", 6 * time.Minute, true, 5 * time.Minute, false, false},
		{"late disconnect does not extend retention", 11 * time.Minute, true, time.Minute, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := configTestServer(t)
			now := time.Now()
			server.collectors["node-1"] = &Collector{
				ID: "node-1", LastSeen: now.Add(-tc.lastSeenAgo),
				disconnected: tc.disconnected, disconnectedAt: now.Add(-tc.disconnectAgo),
			}
			cols, _ := server.snapshot()
			if (len(cols) == 1) != tc.visible {
				t.Fatalf("visible = %v, want %v", len(cols) == 1, tc.visible)
			}
			if tc.visible && cols[0].Active != tc.active {
				t.Fatalf("active = %v, want %v", cols[0].Active, tc.active)
			}
		})
	}
}

func TestCollectorDisconnectAndReconnect(t *testing.T) {
	server := configTestServer(t)
	pollConfig(t, server, "", collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_UNSET, nil)
	unregister := func() {
		t.Helper()
		if _, err := server.UnregisterCollector(context.Background(), connect.NewRequest(&collectorv1.UnregisterCollectorRequest{Id: "node-1"})); err != nil {
			t.Fatal(err)
		}
	}
	unregister()
	cols, _ := server.snapshot()
	if len(cols) != 1 || cols[0].Active {
		t.Fatal("disconnected collector must remain visible and inactive")
	}
	c := server.collectors["node-1"]
	c.disconnectedAt = time.Now().Add(-collectorInactiveRetention)
	unregister()
	cols, _ = server.snapshot()
	if len(cols) != 0 {
		t.Fatal("repeated unregister must not extend retention")
	}
	for _, register := range []bool{false, true} {
		unregister()
		if register {
			if _, err := server.RegisterCollector(context.Background(), connect.NewRequest(&collectorv1.RegisterCollectorRequest{Id: "node-1"})); err != nil {
				t.Fatal(err)
			}
		} else {
			pollConfig(t, server, "", collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_UNSET, nil)
		}
		cols, _ = server.snapshot()
		if len(cols) != 1 || !cols[0].Active || !c.disconnectedAt.IsZero() {
			t.Fatal("reconnected collector must become visible and active")
		}
	}
}
