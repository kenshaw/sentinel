package sentinel

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/brankas/connmux"
	"golang.org/x/sync/errgroup"
)

const (
	// DefaultShutdownDuration is the default shutdown timeout duration.
	DefaultShutdownDuration = 10 * time.Second
)

// Sentinel manages servers and associated shutdown listeners.
type Sentinel struct {
	serverFuncs      []func(context.Context) error
	shutdownFuncs    []func(context.Context) error
	shutdownDuration time.Duration
	shutdownSigs     []os.Signal
	ignoreErrors     []func(error) bool
	logf             func(string, ...interface{})
	errf             func(string, ...interface{})

	sync.Mutex
	started bool
}

// New creates a new sentinal server group.
func New(opts ...Option) (*Sentinel, error) {
	s := &Sentinel{
		shutdownDuration: DefaultShutdownDuration,
		logf:             func(string, ...interface{}) {},
	}

	var err error

	// apply options
	for _, o := range opts {
		if err = o(s); err != nil {
			return nil, err
		}
	}

	// ensure sigs set
	if s.shutdownSigs == nil {
		s.shutdownSigs = []os.Signal{os.Interrupt}
	}

	// ensure errf set
	if s.errf == nil {
		s.errf = func(str string, v ...interface{}) {
			s.logf("ERROR: "+str, v...)
		}
	}

	return s, nil
}

// Run starts the server group, returning the first encountered error upon
// shutdown.
func (s *Sentinel) Run(ctxt context.Context) error {
	s.Lock()
	if s.started {
		return ErrAlreadyStarted
	}
	s.started = true
	s.Unlock()

	eg, ctxt := errgroup.WithContext(ctxt)

	// add servers
	for _, f := range s.serverFuncs {
		eg.Go(func(f func(context.Context) error) func() error {
			return func() error {
				return f(ctxt)
			}
		}(f))
	}

	// add shutdown
	eg.Go(func() func() error {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, s.shutdownSigs...)
		return func() error {
			s.logf("received signal: %v", <-sig)
			return s.Shutdown()
		}
	}())

	if err := eg.Wait(); !s.ShutdownIgnore(err) {
		return err
	}

	return nil
}

// Shutdown calls all of the registered shutdown funcs.
func (s *Sentinel) Shutdown() error {
	var firstErr error
	for i, f := range s.shutdownFuncs {
		ctxt, cancel := context.WithTimeout(context.Background(), s.shutdownDuration)
		defer cancel()
		if err := f(ctxt); err != nil {
			s.errf("could not shutdown %d: %v", i, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// ShutdownIgnore returns if any of the registered ignore funcs reported true.
func (s *Sentinel) ShutdownIgnore(err error) bool {
	if err == nil {
		return true
	}
	for _, f := range s.ignoreErrors {
		if z := f(err); z {
			return true
		}
	}
	return false
}

// Register registers a server, its shutdown func, and ignore error funcs.
func (s *Sentinel) Register(server, shutdown interface{}, ignore ...func(error) bool) error {
	var err error

	// add server and shutdown funcs
	s.serverFuncs, err = convertAndAppendContextFuncs(s.serverFuncs, server)
	if err != nil {
		return err
	}
	s.shutdownFuncs, err = convertAndAppendContextFuncs(s.shutdownFuncs, shutdown)
	if err != nil {
		return err

	}
	s.ignoreErrors = append(s.ignoreErrors, ignore...)
	return nil
}

// ConnMux creates a new connection muxer and registers it.
func (s *Sentinel) ConnMux(listener net.Listener, opts ...connmux.Option) (*connmux.ConnMux, error) {
	s.Lock()
	defer s.Unlock()

	if s.started {
		return nil, ErrAlreadyStarted
	}

	// create connection mux
	mux, err := connmux.New(listener, opts...)
	if err != nil {
		return nil, err
	}

	// register server + shutdown
	if err = s.Register(mux, mux, IgnoreError(connmux.ErrListenerClosed), IgnoreNetOpError); err != nil {
		return nil, err
	}

	return mux, nil
}

// HTTP creates a HTTP server and registers it with the sentinel.
func (s *Sentinel) HTTP(listener net.Listener, handler http.Handler, opts ...ServerOption) error {
	s.Lock()
	defer s.Unlock()

	if s.started {
		return ErrAlreadyStarted
	}

	var err error

	// create server and apply options
	server := &http.Server{
		Handler: handler,
	}
	for _, o := range opts {
		if err = o(server); err != nil {
			return err
		}
	}

	// register server
	return s.Register(func() error {
		return server.Serve(listener)
	}, server.Shutdown, IgnoreServerClosed, IgnoreNetOpError)
}

// IgnoreError returns a func that will return true when the passed errors
// match.
func IgnoreError(err error) func(error) bool {
	return func(e error) bool {
		return err == e
	}
}

// IgnoreServerClosed returns true when the passed error is the
// http.ErrServerClosed error.
func IgnoreServerClosed(err error) bool {
	return err == http.ErrServerClosed
}

// IgnoreNetOpError returns true when the passed error is a net.OpError with
// error "use of closed network connection".
func IgnoreNetOpError(err error) bool {
	if opErr, ok := err.(*net.OpError); ok {
		return opErr.Err.Error() == "use of closed network connection"
	}
	return false
}
