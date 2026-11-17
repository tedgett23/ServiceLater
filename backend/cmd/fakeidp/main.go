// Command fakeidp serves ssotest's OpenID Connect provider, for the e2e run (web/e2e/sso.spec.ts). Never deploy it:
// anyone can sign in as anyone.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/servicelater/servicelater/backend/internal/sso/ssotest"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:5556", "address to listen on")
	issuer := flag.String("issuer", "http://127.0.0.1:5556", "the issuer: the address it is reached at")
	client := flag.String("client-id", "servicelater-e2e", "the client id")
	secret := flag.String("client-secret", "e2e-secret", "the client secret")
	flag.Parse()
	srv := &http.Server{Addr: *addr, Handler: ssotest.New(*issuer, *client, *secret), ReadHeaderTimeout: 5 * time.Second}
	log.Printf("fake identity provider at %s", *issuer)
	log.Fatal(srv.ListenAndServe())
}
