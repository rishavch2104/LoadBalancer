package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT)
	lbServer := createLoadBalancerServer()
	backendServer := createBackendServer()

	go func() {
		lbServer.ListenAndServe()
	}()

	go func() {
		backendServer.ListenAndServe()
	}()

	defer func() {
		if err := lbServer.Shutdown(ctx); err != nil {
			fmt.Println("error when shutting down the load balancer server: ", err)
		}
		if err := backendServer.Shutdown(ctx); err != nil {
			fmt.Println("error when shutting down the backend server: ", err)
		}
	}()
	sig := <-sigs
	fmt.Println(sig)

	cancel()

	//wg.Wait()
}

func writeResponseCommon(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, fmt.Sprintf("Received request from %s \n", r.Host))
	io.WriteString(w, fmt.Sprintf("%s  %s \n", r.Method, r.Proto))
	io.WriteString(w, fmt.Sprintf("Host: %s \n", r.Host))
	io.WriteString(w, fmt.Sprintf("User-Agent: %s \n", r.UserAgent()))
	io.WriteString(w, fmt.Sprintf("Accept: %s \n", r.Header.Get("Accept")))
}

func createBackendServer() *http.Server {
	backendServer := http.NewServeMux()
	backendServer.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {

		io.WriteString(w, "./be \n")
		writeResponseCommon(w, r)
	})
	server := &http.Server{
		Addr:    ":8080",
		Handler: backendServer,
	}
	return server
}

func createLoadBalancerServer() *http.Server {
	lbServer := http.NewServeMux()
	lbServer.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		resp, _ := http.Get("http://localhost:8080")
		io.Copy(w, resp.Body)
		io.WriteString(w, "Hello from Backend Server")
		resp.Body.Close()
	})
	server := &http.Server{
		Addr:    ":80",
		Handler: lbServer,
	}
	return server
}
