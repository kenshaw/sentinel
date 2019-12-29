package sentinel

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/brankas/netmux"
	"golang.org/x/sync/errgroup"
)

const (
	// DefaultShutdownDuration is the default shutdown timeout duration.
	DefaultShutdownDuration = 10 * time.Second
)

// Sentinel manages servers, shutdown listeners, and related errors.
type Sentinel struct {
	serverFuncs      []func(context.Context) error
	shutdownFuncs    []func(context.Context) error
	shutdownDuration time.Duration
	shutdownSigs     []os.Signal
	ignoreErrors     []func(error) bool

	logf func(string, ...interface{})
	errf func(string, ...interface{})

	sync.Mutex
	started bool
	sig     chan os.Signal
}

// New creates a new sentinel server group.
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
func (s *Sentinel) Run(ctx context.Context) error {
	s.Lock()
	if s.started {
		defer s.Unlock()
		return ErrAlreadyStarted
	}
	s.started = true
	s.Unlock()

	eg, ctx := errgroup.WithContext(ctx)

	// add servers
	for _, f := range s.serverFuncs {
		eg.Go(func(f func(context.Context) error) func() error {
			return func() error {
				return f(ctx)
			}
		}(f))
	}

	// add shutdown
	eg.Go(func() func() error {
		s.sig = make(chan os.Signal, 1)
		signal.Notify(s.sig, s.shutdownSigs...)
		return func() error {
			s.logf("received signal: %v", <-s.sig)
			return s.Shutdown()
		}
	}())

	if err := eg.Wait(); err != nil && !s.ShutdownIgnore(err) {
		return err
	}

	return nil
}

// Shutdown calls all registered shutdown funcs.
func (s *Sentinel) Shutdown() error {
	var firstErr error
	for i, f := range s.shutdownFuncs {
		ctx, cancel := context.WithTimeout(context.Background(), s.shutdownDuration)
		defer cancel()
		if err := f(ctx); err != nil {
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
	// add server and shutdown funcs
	var err error
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

// Mux creates a new network connection muxer and registers its server,
// shutdown, and ignore error funcs.
func (s *Sentinel) Mux(listener net.Listener, opts ...netmux.Option) (*netmux.Netmux, error) {
	s.Lock()
	defer s.Unlock()

	if s.started {
		return nil, ErrAlreadyStarted
	}

	// create connection mux
	mux, err := netmux.New(listener, opts...)
	if err != nil {
		return nil, err
	}

	// register server + shutdown
	if err = s.Register(mux, mux, IgnoreListenerClosed, IgnoreNetOpError); err != nil {
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

// IgnoreListenerClosed returns true when the passed error is the
// netmux.ErrListenerClosed error.
func IgnoreListenerClosed(err error) bool {
	return err == netmux.ErrListenerClosed
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

// Option is a sentinel option.
type Option = func(*Sentinel) error

// Register is a sentinel option to register a server, its shutdown listener,
// and ignore error handlers.
//
// Both server and shutdown can have a type of `func()`, `func() error`, or
// `func(context.Context) error`.
func Register(server, shutdown interface{}, ignore ...func(error) bool) Option {
	return func(s *Sentinel) error {
		s.Lock()
		defer s.Unlock()

		if s.started {
			return ErrAlreadyStarted
		}

		return s.Register(server, shutdown, ignore...)
	}
}

// Server is a sentinel option to add server funcs.
//
// Any server can have a type of `func()`, `func() error`, or
// `func(context.Context) error`.
func Server(serverFuncs ...interface{}) Option {
	return func(s *Sentinel) error {
		s.Lock()
		defer s.Unlock()

		if s.started {
			return ErrAlreadyStarted
		}

		var err error
		s.serverFuncs, err = convertAndAppendContextFuncs(s.serverFuncs, serverFuncs...)
		return err
	}
}

// Shutdown is a sentinel option to add shutdown listeners.
//
// Any shutdown listener can have a type of `func()`, `func() error`, or
// `func(context.Context) error`.
func Shutdown(shutdownFuncs ...interface{}) Option {
	return func(s *Sentinel) error {
		s.Lock()
		defer s.Unlock()

		if s.started {
			return ErrAlreadyStarted
		}

		var err error
		s.shutdownFuncs, err = convertAndAppendContextFuncs(s.shutdownFuncs, shutdownFuncs...)
		return err
	}
}

// Ignore is a sentinel option to add ignore error handlers.
func Ignore(ignore ...func(error) bool) Option {
	return func(s *Sentinel) error {
		s.Lock()
		defer s.Unlock()

		if s.started {
			return ErrAlreadyStarted
		}

		s.ignoreErrors = append(s.ignoreErrors, ignore...)
		return nil
	}
}

// Sigs is a sentinel option to set the specified signals for shutdown.
func Sigs(sigs ...os.Signal) Option {
	return func(s *Sentinel) error {
		s.Lock()
		defer s.Unlock()

		if s.started {
			return ErrAlreadyStarted
		}

		s.shutdownSigs = sigs
		return nil
	}
}

// Logf is a sentinel option to set a logger.
func Logf(f func(string, ...interface{})) Option {
	return func(s *Sentinel) error {
		s.logf = f
		return nil
	}
}

// Errorf is a sentinel option to set a error logger.
func Errorf(f func(string, ...interface{})) Option {
	return func(s *Sentinel) error {
		s.errf = f
		return nil
	}
}

// ServerOption is a HTTP server option.
type ServerOption = func(*http.Server) error
