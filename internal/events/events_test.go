package events

import (
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestEmitWritesOneJSONLine(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "events.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	got := make(chan Event, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var ev Event
		if err := json.NewDecoder(conn).Decode(&ev); err == nil {
			got <- ev
		}
	}()

	Emit(sock, Event{Event: VersionLanded, Repo: "app", Root: "/srv/repos/app", VersionID: "v1"})
	select {
	case ev := <-got:
		if ev.Event != VersionLanded || ev.Repo != "app" || ev.VersionID != "v1" || ev.V != Version {
			t.Fatalf("event = %+v", ev)
		}
		if ev.TS == 0 {
			t.Fatal("缺少 ts")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("未收到事件")
	}
}

func TestEmitMissingSocketIsNoop(t *testing.T) {
	Emit(filepath.Join(t.TempDir(), "missing.sock"), Event{Event: VersionLanded})
}
