// VEXORA's single-instance cooperative alpha server. Only the Go standard
// library is required; Render terminates HTTPS in front of this HTTP service.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type content struct {
	Items    []string `json:"items"`
	Text     string   `json:"text"`
	Revision int64    `json:"revision"`
}

type avatar struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Facing int     `json:"facing"`
	State  string  `json:"state"`
	Color  string  `json:"color"`
}

type session struct {
	Avatar  avatar
	Room    string
	Seen    time.Time
	Updated time.Time
}

type room struct {
	Restored  [3]bool
	Completed bool
}

type server struct {
	mu          sync.Mutex
	sessions    map[string]*session
	rooms       map[string]*room
	content     content
	adminToken  string
	contentFile string
	now         func() time.Time
}

func newServer(token, file string) (*server, error) {
	s := &server{sessions: make(map[string]*session), rooms: make(map[string]*room),
		content:    content{Items: []string{"¡Bienvenido a VEXORA online!"}},
		adminToken: token, contentFile: file, now: time.Now}
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err == nil {
			if err = json.Unmarshal(data, &s.content); err != nil {
				return nil, err
			}
			if !validContent(s.content) {
				return nil, errors.New("invalid saved content")
			}
		}
	}
	return s, nil
}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("response failed: %v", err)
	}
}

func problem(w http.ResponseWriter, code int, message string) {
	reply(w, code, map[string]string{"error": message})
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 32768)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		problem(w, 400, "JSON no válido o demasiado grande")
		return false
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		problem(w, 400, "Se esperaba un único objeto JSON")
		return false
	}
	return true
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]string{"status": "ok", "game": "VEXORA", "version": "0.0.2"})
	})
	mux.HandleFunc("GET /v1/content", s.getContent)
	mux.HandleFunc("GET /v1/updates/windows", getUpdate)
	mux.HandleFunc("PUT /v1/content", s.publish)
	mux.HandleFunc("POST /v1/join", s.join)
	mux.HandleFunc("POST /v1/sync", s.syncPlayers)
	mux.HandleFunc("POST /v1/leave", s.leave)
	return mux
}

// getUpdate serves a publisher-signed manifest; clients verify its signature.
func getUpdate(w http.ResponseWriter, r *http.Request) {
	path := os.Getenv("VEXORA_RELEASE_FILE")
	if path == "" {
		path = "release.json"
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		reply(w, 200, map[string]bool{"available": false})
		return
	}
	if err != nil {
		problem(w, 503, "Actualización no disponible")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	var envelope map[string]string
	if err != nil || len(data) > 65536 || json.Unmarshal(data, &envelope) != nil || envelope["payload"] == "" || envelope["signature"] == "" {
		problem(w, 503, "Manifiesto no válido")
		return
	}
	reply(w, 200, envelope)
}

func (s *server) getContent(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reply(w, 200, s.content)
}

func validContent(c content) bool {
	if len(c.Items) > 20 || len(c.Text) > 8000 {
		return false
	}
	for _, item := range c.Items {
		if len(item) > 500 {
			return false
		}
	}
	return true
}

func (s *server) publish(w http.ResponseWriter, r *http.Request) {
	expected := sha256.Sum256([]byte(s.adminToken))
	received := sha256.Sum256([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
	if s.adminToken == "" || subtle.ConstantTimeCompare(expected[:], received[:]) != 1 {
		problem(w, 401, "Clave de administrador incorrecta")
		return
	}
	var c content
	if !decode(w, r, &c) {
		return
	}
	if !validContent(c) {
		problem(w, 400, "Máximo 20 anuncios de 500 bytes y 8000 bytes de colaboraciones")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c.Revision = s.content.Revision + 1
	if c.Items == nil {
		c.Items = []string{}
	}
	if s.contentFile != "" {
		data, err := json.Marshal(c)
		if err == nil {
			err = atomicWrite(s.contentFile, data)
		}
		if err != nil {
			log.Printf("content save failed: %v", err)
			problem(w, 500, "No se pudo guardar; vuelve a intentarlo")
			return
		}
	}
	s.content = c
	reply(w, 200, c)
}

func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".vexora-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

var roomPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,24}$`)

// expire removes abandoned sessions and empty rooms without an extra goroutine.
func (s *server) expire() {
	occupied := map[string]bool{}
	for token, p := range s.sessions {
		if s.now().Sub(p.Seen) > 30*time.Second {
			delete(s.sessions, token)
		} else {
			occupied[p.Room] = true
		}
	}
	for name := range s.rooms {
		if !occupied[name] {
			delete(s.rooms, name)
		}
	}
}

func (s *server) join(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name string `json:"name"`
		Room string `json:"room"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len([]rune(input.Name)) > 16 || !roomPattern.MatchString(input.Room) {
		problem(w, 400, "Nombre de 1 a 16 caracteres y sala de 1 a 24 letras, números o guiones")
		return
	}
	for _, c := range input.Name {
		if c < 32 {
			problem(w, 400, "Nombre no válido")
			return
		}
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		problem(w, 500, "No se pudo crear la sesión")
		return
	}
	token := hex.EncodeToString(tokenBytes)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
	count := 0
	for _, p := range s.sessions {
		if p.Room == input.Room {
			count++
		}
	}
	if count >= 8 || len(s.sessions) >= 256 {
		problem(w, 409, "Sala o servidor lleno")
		return
	}
	if _, exists := s.rooms[input.Room]; !exists {
		s.rooms[input.Room] = &room{}
	}
	id := token[:16]
	s.sessions[token] = &session{Avatar: avatar{ID: id, Name: input.Name, X: 130, Y: 1414, Facing: 1, State: "idle", Color: "Cian"}, Room: input.Room, Seen: s.now()}
	reply(w, 200, map[string]string{"token": token, "id": id})
}

func (s *server) authenticate(r *http.Request) *session {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return s.sessions[token]
}

type update struct {
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	Facing   int     `json:"facing"`
	State    string  `json:"state"`
	Color    string  `json:"color"`
	Interact bool    `json:"interact"`
}

func (s *server) syncPlayers(w http.ResponseWriter, r *http.Request) {
	var in update
	if !decode(w, r, &in) {
		return
	}
	if math.IsNaN(in.X) || math.IsNaN(in.Y) || math.IsInf(in.X, 0) || math.IsInf(in.Y, 0) || in.X < 0 || in.X > 2480 || in.Y < 0 || in.Y > 1640 || (in.Facing != 1 && in.Facing != -1) {
		problem(w, 400, "Posición no válida")
		return
	}
	switch in.State {
	case "idle", "run", "jump", "fall", "land", "dash", "hurt":
	default:
		problem(w, 400, "Animación no válida")
		return
	}
	switch in.Color {
	case "Cian", "Violeta", "Verde":
	default:
		problem(w, 400, "Color no válido")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
	p := s.authenticate(r)
	if p == nil {
		problem(w, 401, "Sesión caducada; vuelve a entrar")
		return
	}
	if s.now().Sub(p.Updated) < 40*time.Millisecond {
		problem(w, 429, "Demasiadas actualizaciones")
		return
	}
	p.Seen = s.now()
	p.Updated = p.Seen
	p.Avatar.X = in.X
	p.Avatar.Y = in.Y
	p.Avatar.Facing = in.Facing
	p.Avatar.State = in.State
	p.Avatar.Color = in.Color
	current := s.rooms[p.Room]
	// The alpha trusts client movement, but checks interaction distance centrally.
	// These sessions must never submit trusted competitive scores.
	if in.Interact {
		for i, pos := range [][2]float64{{630, 1221}, {1410, 1126}, {2250, 1326}} {
			if math.Abs(in.X+21-pos[0]) < 80 && math.Abs(in.Y+33-pos[1]) < 103 {
				current.Restored[i] = true
			}
		}
		if current.Restored == [3]bool{true, true, true} {
			current.Completed = true
		}
	}
	players := []avatar{}
	for _, other := range s.sessions {
		if other.Room == p.Room {
			players = append(players, other.Avatar)
		}
	}
	reply(w, 200, map[string]any{"players": players, "restored": current.Restored, "completed": current.Completed})
}

func (s *server) leave(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	s.expire()
	reply(w, 200, map[string]bool{"ok": true})
}

func main() {
	token := os.Getenv("VEXORA_ADMIN_TOKEN")
	if len(token) < 32 {
		log.Fatal("VEXORA_ADMIN_TOKEN must contain at least 32 characters")
	}
	file := os.Getenv("VEXORA_CONTENT_FILE")
	if file == "" {
		file = "data/content.json"
	}
	app, err := newServer(token, file)
	if err != nil {
		log.Fatal(err)
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "10000"
	}
	srv := &http.Server{Addr: ":" + port, Handler: app.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	fmt.Printf("VEXORA 0.0.2 listening on port %s\n", port)
	log.Fatal(srv.ListenAndServe())
}
