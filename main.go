package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	Attempts int = iota
	Retry
)

type Backend struct {
	port    int
	id      int
	url     *url.URL
	isAlive bool
	mux     sync.RWMutex
	proxy   *httputil.ReverseProxy
}

func (b *Backend) SetAlive(alive bool) {
	b.mux.Lock()
	b.isAlive = alive
	b.mux.Unlock()
}

// IsAlive returns true when backend is alive
func (b *Backend) IsAlive() (alive bool) {
	b.mux.RLock()
	alive = b.isAlive
	b.mux.RUnlock()
	return
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
		if s.backends[idx].isAlive {
			if i != next {
				atomic.StoreUint64(&s.current, uint64(idx))
			}
			return s.backends[idx]
		}
	}
	return nil
}

func (s *ServerPool) MarkBackendStatus(beUrl *url.URL, alive bool) {
	for _, b := range s.backends {
		if b.url.String() == beUrl.String() {
			b.isAlive = alive
			return
		}
	}
}
func (s *ServerPool) HealthCheck() {
	for _, b := range s.backends {
		status := "up"
		alive := isBackendAlive(b.url)
		b.SetAlive(alive)
		if !alive {
			status = "down"
		}
		log.Printf("%s [%s]\n", b.url, status)
	}
}

func GetAttemptsFromContext(r *http.Request) int {
	if r.Context().Value(Attempts) == nil {
		return 1
	}
	return r.Context().Value(Attempts).(int)
}

func GetRetryFromContext(r *http.Request) int {
	if r.Context().Value(Retry) == nil {
		return 0
	}
	return r.Context().Value(Retry).(int)
}

var serverPool = &ServerPool{}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	backendServersConfig := []Backend{
		{port: 8080, id: 1, isAlive: true},
		{port: 8081, id: 2, isAlive: true},
		{port: 8082, id: 3, isAlive: true},
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
		beUrl := fmt.Sprintf("http://localhost:%d", backendServersConfig[i].port)
		u, _ := url.Parse(beUrl)
		proxy := httputil.NewSingleHostReverseProxy(u)
		backendServersConfig[i].proxy = proxy
		backendServersConfig[i].url = u
		proxy.ErrorHandler = func(writer http.ResponseWriter, request *http.Request, e error) {
			retries := GetRetryFromContext(request)
			if retries < 3 {
				select {
				case <-time.After(10 * time.Millisecond):
					ctx := context.WithValue(request.Context(), Retry, retries+1)
					proxy.ServeHTTP(writer, request.WithContext(ctx))
				}
				return
			}

			serverPool.MarkBackendStatus(backendServersConfig[i].url, false)

			attempts := GetAttemptsFromContext(request)
			log.Printf("%s(%s) Attempting retry %d\n", request.RemoteAddr, request.URL.Path, attempts)
			ctx := context.WithValue(request.Context(), Attempts, attempts+1)
			lb(writer, request.WithContext(ctx))
		}

		serverPool.backends = append(serverPool.backends, &backendServersConfig[i])
		backendServers[i] = backendServerInstance

	}
	go func() {
		lbServer.ListenAndServe()
	}()
	go healthCheck()

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
func isBackendAlive(u *url.URL) bool {
	timeout := 2 * time.Second
	conn, err := net.DialTimeout("tcp", u.Host, timeout)
	if err != nil {
		log.Println("Site unreachable, error: ", err)
		return false
	}
	defer conn.Close()
	return true
}

func healthCheck() {
	t := time.NewTicker(time.Minute * 2)
	for {
		select {
		case <-t.C:
			log.Println("Starting health check...")
			serverPool.HealthCheck()
			log.Println("Health check completed")
		}
	}
}

func lb(w http.ResponseWriter, r *http.Request) {
	attempts := GetAttemptsFromContext(r)
	if attempts > 3 {
		log.Printf("%s(%s) Max attempts reached, terminating\n", r.RemoteAddr, r.URL.Path)
		http.Error(w, "Service not available", http.StatusServiceUnavailable)
		return
	}

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
