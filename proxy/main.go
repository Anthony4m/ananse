package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"
)

func main() {
	target, err := url.Parse("http://localhost:4199")

	if err != nil {
		log.Fatal(err)
	}

	// create a reverse proxy
	proxy := httputil.NewSingleHostReverseProxy(target)

	originalDirector := proxy.Director
	proxy.Director = func(request *http.Request) {
		originalDirector(request)

		//add custom headers
		request.Header.Set("X-Forwarded-Host", request.Host)
		request.Header.Set("X-Origin-Host", target.Host)
		request.Header.Set("X-Forwarded-For", request.RemoteAddr)
		request.Header.Set("X-Forwarded-Proto", "https")
		if request.Header.Get("X-Request-ID") == "" {
			request.Header.Set("X-Request-ID", strconv.FormatInt(time.Now().UnixNano(), 10))
		}
	}

	proxy.Transport = &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		MaxConnsPerHost:     50,
		IdleConnTimeout:     90 * time.Second,
	}

	proxy.ErrorHandler = func(writer http.ResponseWriter, request *http.Request, err error) {
		log.Printf("x Proxy error: %v", err)
		http.Error(writer, "Bad Gateway", http.StatusBadGateway)
	}

	proxy.ModifyResponse = func(response *http.Response) error {
		bodyBytes, err := io.ReadAll(response.Body)
		if err != nil {
			return err
		}
		response.Body.Close()

		var healthCheck struct {
			Status    string    `json:"status"`
			TimeStamp time.Time `json:"timeStamp"`
		}

		if err := json.Unmarshal(bodyBytes, &healthCheck); err != nil {
			response.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
			return nil
		}
		if healthCheck.Status != "healthy" {
			currResponse := map[string]interface{}{
				"status":          "healthy",
				"timestamp":       time.Now().UTC().Format(time.RFC3339),
				"original_status": healthCheck.Status,
			}
			currBody, err := json.Marshal(currResponse)
			if err != nil {
				return err
			}

			response.Body = io.NopCloser(bytes.NewReader(currBody))
			response.ContentLength = int64(len(currBody))
			response.Header.Set("Content-Length", strconv.Itoa(len(currBody)))
			response.Header.Set("X-Modified-By-Proxy", "true")
		} else {
			response.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}
		return nil
	}

	http.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		proxy.ServeHTTP(writer, request)
	})

	log.Println("Proxy server started on :8089")
	log.Fatal(http.ListenAndServe(":8089", nil))
}
