package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const maxBody = 4 << 20

var repoName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

type login struct {
	Login string `json:"login"`
}

type issue struct {
	ID        uint64  `json:"id"`
	NodeID    string  `json:"node_id"`
	Number    int     `json:"number"`
	Title     string  `json:"title"`
	Body      string  `json:"body"`
	State     string  `json:"state"`
	Labels    []label `json:"labels"`
	Comments  int     `json:"comments"`
	User      login   `json:"user"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
	ClosedAt  *string `json:"closed_at"`
	cached    []byte
}

type repository struct {
	ID          uint64 `json:"id"`
	NodeID      string `json:"node_id"`
	Name        string `json:"name"`
	FullName    string `json:"full_name"`
	Owner       login  `json:"owner"`
	Private     bool   `json:"private"`
	Description string `json:"description"`
	Branch      string `json:"default_branch"`
	HasIssues   bool   `json:"has_issues"`
	issueRows   map[int]*issue
	nextNumber  int
	pulls       map[int]*pull
	labels      map[string]label
	comments    map[int][]note
	git         gitRepo
}

type forge struct {
	owner  atomic.Pointer[string]
	bearer string
	ids    atomic.Uint64
	mu     sync.RWMutex
	nextID uint64
	second int64
	repos  map[string]*repository
}

func nodeID(kind string, id uint64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(kind + ":" + strconv.FormatUint(id, 10)))
}

func encode(v any) []byte {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		panic(err)
	}
	return bytes.TrimSuffix(output.Bytes(), []byte{'\n'})
}

func send(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if len(body) != 0 {
		_, _ = w.Write(body)
	}
}

func problem(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	send(w, status, encode(struct {
		Message string `json:"message"`
	}{message}))
}

func requestBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		problem(w, http.StatusRequestEntityTooLarge, "Request body is too large")
		return nil, false
	}
	if err != nil {
		problem(w, http.StatusBadRequest, "Malformed JSON")
		return nil, false
	}
	return body, true
}

func decode(w http.ResponseWriter, r *http.Request, dest any) bool {
	body, ok := requestBody(w, r)
	if !ok {
		return false
	}
	if len(body) > 0 && json.Unmarshal(body, dest) != nil {
		problem(w, http.StatusBadRequest, "Malformed JSON")
		return false
	}
	return true
}

var ownerPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,38}$`)

func (f *forge) ownerName() string {
	if p := f.owner.Load(); p != nil {
		return *p
	}
	return "forge"
}

func (f *forge) owns(owner string) bool {
	if current := f.owner.Load(); current != nil {
		return strings.EqualFold(owner, *current)
	}
	if !ownerPattern.MatchString(owner) {
		return false
	}
	f.owner.CompareAndSwap(nil, &owner)
	return strings.EqualFold(owner, f.ownerName())
}

func main() {
	env := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	token := fallback(os.Getenv("MAKEFUN_SHOP_TOKEN"), os.Getenv("FORGE_TOKEN"))
	owner := os.Getenv("FORGE_OWNER")
	if token == "" {
		log.Fatal("FORGE_TOKEN or MAKEFUN_SHOP_TOKEN is required")
	}
	addr := "127.0.0.1:0"
	if env != "" {
		addr = "0.0.0.0:0"
	} else if owner == "" {
		owner = "forge"
	}
	f := &forge{bearer: "Bearer " + token, second: 1735689600, repos: make(map[string]*repository)}
	if owner != "" {
		if !ownerPattern.MatchString(owner) {
			log.Fatal("Invalid FORGE_OWNER")
		}
		f.owner.Store(&owner)
	}
	listener, err := net.Listen("tcp4", addr)
	if err != nil {
		log.Fatal(err)
	}
	server := &wireServer{handler: f}
	port := listener.Addr().(*net.TCPAddr).Port
	fmt.Printf("[forge] listening on port %d\n", port)
	if env != "" {
		control := make(chan error, 1)
		go func() { control <- runTrial(env, port); _ = listener.Close(); server.close() }()
		if err = server.Serve(listener); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Fatal(err)
		}
		if err = <-control; err != nil {
			log.Fatal(err)
		}
		return
	}
	if err = server.Serve(listener); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Fatal(err)
	}
}
