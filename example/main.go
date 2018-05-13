// example/main.go
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"

	"github.com/brankas/sentinel"
)

func main() {
	// create listeners
	l1, err := net.Listen("tcp", ":3000")
	if err != nil {
		log.Fatal(err)
	}
	l2, err := net.Listen("tcp", ":3001")
	if err != nil {
		log.Fatal(err)
	}

	// create server sentinel
	s, err := sentinel.New(
		sentinel.Logf(log.Printf),
	)
	if err != nil {
		log.Fatal(err)
	}

	// create servers
	err = s.HTTP(l1, http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
		fmt.Fprintln(res, "hello!")
	}))
	if err != nil {
		log.Fatal(err)
	}
	err = s.HTTP(l2, http.RedirectHandler("http://localhost:3000", http.StatusMovedPermanently))
	if err != nil {
		log.Fatal(err)
	}

	if err = s.Run(context.Background()); err != nil {
		log.Fatal(err)
	}
}
