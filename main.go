package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
)

type Backend struct {
	port  int
	id    int
	proxy *httputil.ReverseProxy
}

type ServerPool struct {
	backends []*Backend
	current  uint64
}

func (s *ServerPool) NextIndex() int {
	return int(atomic.AddUint64(&s.current, uint64(1)) % uint64(len(s.backends)))
}

func (s *ServerPool) GetNextPeer() *Backend {
	next := s.NextIndex()
	l := len(s.backends) + next
	for i := next; i < l; i++ {
		idx := i % len(s.backends)
		if i != next {
			atomic.StoreUint64(&s.current, uint64(idx))
		}
		return s.backends[idx]
	}
	return nil
}

var serverPool = &ServerPool{}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	backendServersConfig := []Backend{
		{port: 8080, id: 1},
		{port: 8081, id: 2},
		{port: 8082, id: 3},
	}
	backendServers := make([]*http.Server, len(backendServersConfig))
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT)
	lbServer := createLoadBalancerServer()

	for i := range backendServersConfig {
		backendServerInstance := createBackendServer(backendServersConfig[i].id, backendServersConfig[i].port)
		go func() {
			backendServerInstance.ListenAndServe()
		}()
		u, _ := url.Parse(fmt.Sprintf("http://localhost:%d", backendServersConfig[i].port))
		backendServersConfig[i].proxy = httputil.NewSingleHostReverseProxy(u)
		serverPool.backends = append(serverPool.backends, &backendServersConfig[i])
		backendServers[i] = backendServerInstance

	}
	go func() {
		lbServer.ListenAndServe()
	}()

	defer func() {
		if err := lbServer.Shutdown(ctx); err != nil {
			fmt.Println("error when shutting down the load balancer server: ", err)
		}
		for _, backendServer := range backendServers {
			if err := backendServer.Shutdown(ctx); err != nil {
				fmt.Println("error when shutting down the backend server: ", err)
			}
		}
	}()
	sig := <-sigs
	fmt.Println(sig)

	cancel()
}

func writeResponseCommon(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, fmt.Sprintf("Received request from %s \n", r.Host))
	io.WriteString(w, fmt.Sprintf("%s  %s \n", r.Method, r.Proto))
	io.WriteString(w, fmt.Sprintf("Host: %s \n", r.Host))
	io.WriteString(w, fmt.Sprintf("User-Agent: %s \n", r.UserAgent()))
	io.WriteString(w, fmt.Sprintf("Accept: %s \n", r.Header.Get("Accept")))
}

func createBackendServer(serverId int, port int) *http.Server {
	backendServer := http.NewServeMux()
	backendServer.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {

		io.WriteString(w, "./be \n")
		writeResponseCommon(w, r)
		io.WriteString(w, fmt.Sprintf("Server ID: %d \n", serverId))
	})
	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: backendServer,
	}
	return server
}

func lb(w http.ResponseWriter, r *http.Request) {
	peer := serverPool.GetNextPeer()
	if peer != nil {
		peer.proxy.ServeHTTP(w, r)
		return
	}
	http.Error(w, "Service not available", http.StatusServiceUnavailable)

}

func createLoadBalancerServer() *http.Server {
	server := &http.Server{
		Addr:    ":80",
		Handler: http.HandlerFunc(lb),
	}
	return server
}
