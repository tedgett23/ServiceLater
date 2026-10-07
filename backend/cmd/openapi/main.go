// Command openapi writes the API's OpenAPI document from the source of internal/api (see internal/openapi).
//
//	go run ./cmd/openapi            # rewrite internal/api/openapi.json
//	go run ./cmd/openapi -o -       # print it
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/servicelater/servicelater/backend/internal/openapi"
)

func main() {
	src := flag.String("dir", "internal/api", "the api package's directory")
	out := flag.String("o", "internal/api/openapi.json", "where to write the document, or - for stdout")
	flag.Parse()
	doc, err := openapi.Generate(*src)
	if err == nil {
		if *out == "-" {
			_, err = os.Stdout.Write(doc)
		} else {
			err = os.WriteFile(*out, doc, 0o644) //nolint:gosec // a source file in the repository, for anyone to read
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
