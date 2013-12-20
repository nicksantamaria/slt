package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	vhost "github.com/inconshreveable/go-vhost"
	"io"
	"io/ioutil"
	"launchpad.net/goyaml"
	"log"
	"net"
	"os"
	"sync"
	"time"
)

const (
	muxTimeout            = 10 * time.Second
	defaultConnectTimeout = 10000 // milliseconds
)

type loadTLSConfigFn func(crtPath, keyPath string) (*tls.Config, error)

type Options struct {
	configPath string
}

type Backend struct {
	Addr           string `"yaml:addr"`
	ConnectTimeout int    `yaml:connect_timeout"`
}

type Frontend struct {
	Backends []Backend `yaml:"backends"`
	Strategy string    `yaml:"strategy"`
	TLSCrt   string    `yaml:"tls_crt"`
	mux *vhost.TLSMuxer
	TLSKey   string    `yaml:"tls_key"`

	strategy  BackendStrategy `yaml:"-"`
	tlsConfig *tls.Config     `yaml:"-"`
}

type Configuration struct {
	BindAddr  string               `yaml:"bind_addr"`
	Frontends map[string]*Frontend `yaml:"frontends"`
}

type Server struct {
	*log.Logger
	*Configuration
	wait sync.WaitGroup

	// these are for easier testing
	mux *vhost.TLSMuxer
	ready chan int
}

func (s *Server) Run() {
	// bind a port to handle TLS connections
	l, err := net.Listen("tcp", s.Configuration.BindAddr)
	if err != nil {
		panic(err)
	}
	s.Printf("Serving connections on %v", l.Addr())

	// start muxing on it
	s.mux, err = vhost.NewTLSMuxer(l, muxTimeout)
	if err != nil {
		panic(err)
	}

	// wait for all frontends to finish
	s.wait.Add(len(s.Frontends))

	// setup muxing for each frontend
	for name, front := range s.Frontends {
		fl, err := s.mux.Listen(name)
		if err != nil {
			panic(err)
		}
		go s.runFrontend(name, front, fl)
	}

	// use the default error handler
	go s.mux.HandleErrors()

	// we're ready, signal it for testing
	if s.ready != nil {
		close(s.ready)
	}

	s.wait.Wait()
}

func (s *Server) runFrontend(name string, front *Frontend, l net.Listener) {
	// mark finished when done so Run() can return
	defer s.wait.Done()

	// always round-robin strategy for now
	front.strategy = &RoundRobinStrategy{backends: front.Backends}

	s.Printf("Handling connections to %v", name)
	for {
		// accept next connection to this frontend
		conn, err := l.Accept()
		if err != nil {
			s.Printf("Failed to accept new connection for '%v': %v", conn.RemoteAddr())
			if e, ok := err.(net.Error); ok {
				if e.Temporary() {
					continue
				}
			}
			return
		}
		s.Printf("Accepted new connection for %v from %v", name, conn.RemoteAddr())

		// unwrap if tls cert/key was specified
		if front.tlsConfig != nil {
			conn = tls.Server(conn, front.tlsConfig)
		}

		// proxy the connection to an backend
		go s.proxyConnection(conn, front)
	}
}

func (s *Server) proxyConnection(c net.Conn, front *Frontend) (err error) {
	// pick the backend
	backend := front.strategy.NextBackend()

	// dial the backend
	upConn, err := net.DialTimeout("tcp", backend.Addr, time.Duration(backend.ConnectTimeout)*time.Millisecond)
	if err != nil {
		s.Printf("Failed to dial backend connection %v: %v", backend.Addr, err)
		c.Close()
		return
	}
	s.Printf("Initiated new connection to backend: %v %v", upConn.LocalAddr(), upConn.RemoteAddr())

	// join the connections
	s.joinConnections(c, upConn)
	return
}

func (s *Server) joinConnections(c1 net.Conn, c2 net.Conn) {
	var wg sync.WaitGroup
	halfJoin := func(dst net.Conn, src net.Conn) {
		defer wg.Done()
		defer dst.Close()
		defer src.Close()
		n, err := io.Copy(dst, src)
		s.Printf("Copy from %v to %v failed after %d bytes with error %v", src.RemoteAddr(), dst.RemoteAddr(), n, err)
	}

	s.Printf("Joining connections: %v %v", c1.RemoteAddr(), c2.RemoteAddr())
	wg.Add(2)
	go halfJoin(c1, c2)
	go halfJoin(c2, c1)
	wg.Wait()
}

type BackendStrategy interface {
	NextBackend() Backend
}

type RoundRobinStrategy struct {
	backends []Backend
	idx      int
}

func (s *RoundRobinStrategy) NextBackend() Backend {
	n := len(s.backends)

	if n == 1 {
		return s.backends[0]
	} else {
		s.idx = (s.idx + 1) % n
		return s.backends[s.idx]
	}
}

func parseArgs() (*Options, error) {
	flag.Parse()

	if len(flag.Args()) != 1 {
		return nil, fmt.Errorf("You must specify a single argument, the path to the configuration file.")
	}

	return &Options{
		configPath: flag.Arg(0),
	}, nil

}

func parseConfig(configBuf []byte, loadTLS loadTLSConfigFn) (config *Configuration, err error) {
	// deserialize/parse the config
	config = new(Configuration)
	if err = goyaml.Unmarshal(configBuf, &config); err != nil {
		err = fmt.Errorf("Error parsing configuration file: %v", err)
		return
	}

	// configuration validation / normalization
	if config.BindAddr == "" {
		err = fmt.Errorf("You must specify a bind_addr")
		return
	}

	if len(config.Frontends) == 0 {
		err = fmt.Errorf("You must specify at least one frontend")
		return
	}

	for name, front := range config.Frontends {
		if len(front.Backends) == 0 {
			err = fmt.Errorf("You must specify at least one backend for frontend '%v'", name)
			return
		}

		for _, back := range front.Backends {
			if back.ConnectTimeout == 0 {
				back.ConnectTimeout = defaultConnectTimeout
			}

			if back.Addr == "" {
				err = fmt.Errorf("You must specify an addr for each backend on frontend '%v'", name)
				return
			}
		}

		if front.TLSCrt != "" || front.TLSKey != "" {
			if front.tlsConfig, err = loadTLS(front.TLSCrt, front.TLSKey); err != nil {
				err = fmt.Errorf("Failed to load TLS configuration for frontend '%v': %v", name, err)
				return
			}
		}
	}

	return
}

func loadTLSConfig(crtPath, keyPath string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(crtPath, keyPath)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
	}, nil
}

func main() {
	// parse command line options
	opts, err := parseArgs()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	// read configuration file
	configBuf, err := ioutil.ReadFile(opts.configPath)
	if err != nil {
		fmt.Printf("Failed to read configuration file %s: %v", opts.configPath, err)
		os.Exit(1)
	}
	fmt.Printf("Read configuration file at: %s", opts.configPath)


	// parse configuration file
	config, err := parseConfig(configBuf, loadTLSConfig)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	// run server
	s := &Server{
		Configuration: config,
		Logger:        log.New(os.Stdout, "slt ", log.LstdFlags|log.Lshortfile),
	}

	s.Run()
}