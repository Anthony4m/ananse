package main

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
)

func main() {
	target, err := url.Parse("http://localhost:4199")

	if err != nil {
		log.Fatal(err)
	}

	// create a reverse proxy
	proxy := httputil.NewSingleHostReverseProxy(target)

	http.HandleFunc("/echo", func(writer http.ResponseWriter, request *http.Request) {
		proxy.ServeHTTP(writer, request)
	})

	log.Println("Proxt server started on :8089")
	log.Fatal(http.ListenAndServe(":8089", nil))
}
