package opencode

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/IngTian/claude-witness/internal/store"
)

func TestCaptureWritesOpenCodeEventOnce(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	event := []byte(`{
		"type":"message.updated",
		"properties":{
			"sessionID":"ses_plugin",
			"info":{"id":"msg_user","role":"user"},
			"part":{"type":"text","text":"hello from plugin"}
		}
	}`)

	wrote, err := Capture(st, event, time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Fatal("first capture should write")
	}
	wrote, err = Capture(st, event, time.Unix(11, 0))
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Fatal("duplicate capture should be idempotent")
	}
	raw, err := st.ReadRaw(SessionPrefix + "ses_plugin")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 1 || raw[0].Role != "user" || raw[0].Text != "hello from plugin" {
		t.Fatalf("unexpected raw: %+v", raw)
	}
}

func TestCaptureSkipsIncompleteAssistant(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	event := []byte(`{
		"type":"message.updated",
		"properties":{
			"sessionID":"ses_plugin",
			"info":{"id":"msg_a","role":"assistant","time":{"created":1}},
			"part":{"type":"text","text":"partial"}
		}
	}`)
	wrote, err := Capture(st, event, time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Fatal("incomplete assistant message should not be captured")
	}
}

func TestCaptureSkipsMessageAlreadyImportedFromDB(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	event := []byte(`{
		"type":"message.updated",
		"properties":{
			"sessionID":"ses_plugin",
			"info":{"id":"msg_user","role":"user"},
			"part":{"type":"text","text":"hello from plugin"}
		}
	}`)
	key := messageKey("msg_user", "user", "hello from plugin")
	if err := st.SetMetaString(importKeysMetaPrefix+SessionPrefix+"ses_plugin", `["`+key+`"]`); err != nil {
		t.Fatal(err)
	}
	wrote, err := Capture(st, event, time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Fatal("plugin capture should skip messages already imported from DB")
	}
}
