package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUpdateManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release.json")
	t.Setenv("VEXORA_RELEASE_FILE", path)
	h := http.HandlerFunc(getUpdate)
	code, body := call(t, h, "GET", "/v1/updates/windows", "", nil)
	if code != 200 || body["available"] != false {
		t.Fatal(code, body)
	}
	if err := os.WriteFile(path, []byte(`{"payload":"abc","signature":"xyz"}`), 0600); err != nil {
		t.Fatal(err)
	}
	code, body = call(t, h, "GET", "/v1/updates/windows", "", nil)
	if code != 200 || body["signature"] != "xyz" {
		t.Fatal(code, body)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 65537)), 0600); err != nil {
		t.Fatal(err)
	}
	code, _ = call(t, h, "GET", "/v1/updates/windows", "", nil)
	if code != 503 {
		t.Fatal(code)
	}
}

func call(t *testing.T, h http.Handler, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(data))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	result := map[string]any{}
	if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return w.Code, result
}

func fixture(t *testing.T) (*server, http.Handler) {
	t.Helper()
	s, err := newServer(strings.Repeat("x", 32), filepath.Join(t.TempDir(), "content.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s, s.handler()
}

func joinTest(t *testing.T, h http.Handler, name, room string) string {
	t.Helper()
	code, body := call(t, h, "POST", "/v1/join", "", map[string]string{"name": name, "room": room})
	if code != 200 {
		t.Fatalf("join %d: %v", code, body)
	}
	return body["token"].(string)
}

func TestSharedRoomAndIsolation(t *testing.T) {
	_, h := fixture(t)
	a := joinTest(t, h, "Ana", "shared")
	b := joinTest(t, h, "David", "shared")
	c := joinTest(t, h, "Other", "private")
	state := update{X: 610, Y: 1190, Facing: 1, State: "jump", Color: "Cian", Interact: true}
	code, body := call(t, h, "POST", "/v1/sync", a, state)
	if code != 200 || len(body["players"].([]any)) != 2 {
		t.Fatalf("shared avatars: %d %v", code, body)
	}
	state.X = 130
	state.Y = 1414
	state.Interact = false
	_, body = call(t, h, "POST", "/v1/sync", b, state)
	if body["restored"].([]any)[0] != true {
		t.Fatal("second player did not receive shared core")
	}
	_, body = call(t, h, "POST", "/v1/sync", c, state)
	if len(body["players"].([]any)) != 1 || body["restored"].([]any)[0] != false {
		t.Fatal("room isolation failed")
	}
	// The public player array must never contain session credentials.
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), c) {
		t.Fatal("session token exposed")
	}
}

func TestPublicationAuthorizationAndPersistence(t *testing.T) {
	s, h := fixture(t)
	data := content{Items: []string{"Nueva zona"}, Text: "Colabora con vdavidc"}
	for _, token := range []string{"", "wrong", joinTest(t, h, "Player", "room")} {
		code, _ := call(t, h, "PUT", "/v1/content", token, data)
		if code != 401 {
			t.Fatalf("unauthorized publish returned %d", code)
		}
	}
	code, _ := call(t, h, "PUT", "/v1/content", s.adminToken, data)
	if code != 200 {
		t.Fatal(code)
	}
	// A second publication exercises replacement of an existing file on Windows.
	data.Text = "Actualizado"
	code, _ = call(t, h, "PUT", "/v1/content", s.adminToken, data)
	if code != 200 {
		t.Fatal(code)
	}
	restarted, err := newServer(s.adminToken, s.contentFile)
	if err != nil {
		t.Fatal(err)
	}
	_, body := call(t, restarted.handler(), "GET", "/v1/content", "", nil)
	if body["text"] != "Actualizado" || body["revision"] != float64(2) {
		t.Fatal(body)
	}
}

func TestCapacityExpirationAndValidation(t *testing.T) {
	s, h := fixture(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	first := joinTest(t, h, "First", "full")
	for i := 1; i < 8; i++ {
		joinTest(t, h, "Player", "full")
	}
	code, _ := call(t, h, "POST", "/v1/join", "", map[string]string{"name": "Ninth", "room": "full"})
	if code != 409 {
		t.Fatal(code)
	}
	state := update{X: 130, Y: 1414, Facing: 1, State: "idle", Color: "Cian", Interact: true}
	_, body := call(t, h, "POST", "/v1/sync", first, state)
	for _, v := range body["restored"].([]any) {
		if v != false {
			t.Fatal("remote interaction accepted")
		}
	}
	state.X = -1
	code, _ = call(t, h, "POST", "/v1/sync", first, state)
	if code != 400 {
		t.Fatal(code)
	}
	now = now.Add(31 * time.Second)
	state.X = 130
	code, _ = call(t, h, "POST", "/v1/sync", first, state)
	if code != 401 {
		t.Fatal(code)
	}
	joinTest(t, h, "Replacement", "full")
}

func TestCooperativeCompletionAndLeave(t *testing.T) {
	s, h := fixture(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	token := joinTest(t, h, "David", "co-op")
	for _, xy := range [][2]float64{{610, 1190}, {1390, 1095}, {2230, 1295}} {
		now = now.Add(time.Second)
		code, body := call(t, h, "POST", "/v1/sync", token, update{X: xy[0], Y: xy[1], Facing: 1, State: "idle", Color: "Cian", Interact: true})
		if code != 200 {
			t.Fatal(code)
		}
		if xy[0] == 2230 && body["completed"] != true {
			t.Fatal("third core did not complete mission")
		}
	}
	call(t, h, "POST", "/v1/leave", token, nil)
	if len(s.rooms) != 0 {
		t.Fatal("empty room leaked")
	}
}
