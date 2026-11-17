// Command fakeidp serves fakes of the outside services the e2e run talks to: ssotest's OpenID Connect provider, a
// Twilio Messages API and Teams webhooks, which record what they are sent (GET /fake/sent lists it), and a Microsoft 365
// mailbox read through Graph (POST /fake/mail with a raw email puts it in the inbox, unread).
// Never deploy it: anyone can sign in as anyone.
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/servicelater/servicelater/backend/internal/sso/ssotest"
)

type message struct {
	Channel string `json:"channel"` // sms or teams
	To      string `json:"to"`      // the number, or the webhook's path
	Body    string `json:"body"`    // the text, or the card as JSON
}

func main() {
	addr := flag.String("addr", "127.0.0.1:5556", "address to listen on")
	issuer := flag.String("issuer", "http://127.0.0.1:5556", "the issuer: the address it is reached at")
	client := flag.String("client-id", "servicelater-e2e", "the client id")
	secret := flag.String("client-secret", "e2e-secret", "the client secret")
	flag.Parse()

	var mu sync.Mutex
	var sent []message
	record := func(m message) {
		mu.Lock()
		sent = append(sent, m)
		mu.Unlock()
	}
	mux := http.NewServeMux()
	mux.Handle("/", ssotest.New(*issuer, *client, *secret))
	// Twilio: POST /2010-04-01/Accounts/{sid}/Messages.json with To, Body.
	mux.HandleFunc("POST /2010-04-01/Accounts/{sid}/Messages.json", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		record(message{Channel: "sms", To: form.Get("To"), Body: form.Get("Body")})
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"sid":"SM00000000000000000000000000000000","status":"queued"}`))
	})
	// Teams: POST /teams/{name} with a card.
	mux.HandleFunc("POST /teams/{name}", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		record(message{Channel: "teams", To: "/teams/" + r.PathValue("name"), Body: string(raw)})
		w.WriteHeader(http.StatusAccepted)
	})
	// Microsoft 365: client-credentials sign-in, then Graph's unread list, raw message and mark-read, for any mailbox.
	type mail struct {
		raw  []byte
		read bool
	}
	var inbox []*mail
	mux.HandleFunc("POST /fake/mail", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		inbox = append(inbox, &mail{raw: raw})
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("POST /{tenant}/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("grant_type") != "client_credentials" || r.FormValue("client_secret") != "graph-secret" {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"graph-token","token_type":"Bearer","expires_in":3600}`))
	})
	graph := func(h func(w http.ResponseWriter, r *http.Request, m *mail)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer graph-token" {
				http.Error(w, `{"error":{"code":"InvalidAuthenticationToken","message":"no token"}}`, http.StatusUnauthorized)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			var m *mail
			if id := r.PathValue("id"); id != "" {
				n, err := strconv.Atoi(id)
				if err != nil || n < 0 || n >= len(inbox) {
					http.NotFound(w, r)
					return
				}
				m = inbox[n]
			}
			h(w, r, m)
		}
	}
	mux.HandleFunc("GET /v1.0/users/{mailbox}/mailFolders/inbox/messages", graph(func(w http.ResponseWriter, _ *http.Request, _ *mail) {
		type item struct {
			ID string `json:"id"`
		}
		out := []item{}
		for i, m := range inbox {
			if !m.read {
				out = append(out, item{strconv.Itoa(i)})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"value": out})
	}))
	mux.HandleFunc("GET /v1.0/users/{mailbox}/messages/{id}/{value}", graph(func(w http.ResponseWriter, _ *http.Request, m *mail) {
		_, _ = w.Write(m.raw)
	}))
	mux.HandleFunc("PATCH /v1.0/users/{mailbox}/messages/{id}", graph(func(w http.ResponseWriter, _ *http.Request, m *mail) {
		m.read = true
		w.WriteHeader(http.StatusOK)
	}))
	mux.HandleFunc("GET /fake/sent", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		out := []message{}
		for _, m := range sent {
			if c := r.URL.Query().Get("channel"); c == "" || c == m.Channel {
				if to := r.URL.Query().Get("to"); to == "" || strings.Contains(m.To, to) {
					out = append(out, m)
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("fake identity provider, Twilio and Teams at %s", *issuer)
	log.Fatal(srv.ListenAndServe())
}
