// Package sentinel provides a sentinel server group.
package sentinel

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

var Logf = log.Printf
var Errorf = func(s string, v ...interface{}) { Logf("ERROR: "+s, v...) }

// Server is a server.
type Server struct {
	start    func(context.Context) error
	shutdown func(context.Context) error
}

// NewServer creates a new server.
func NewServer(start, shutdown interface{}) (Server, error) {
	var err error
	var st, sh func(context.Context) error
	if start != nil {
		st, err = convertContextFunc(start)
		if err != nil {
			return Server{}, err
		}
	}
	if shutdown != nil {
		sh, err = convertContextFunc(shutdown)
		if err != nil {
			return Server{}, err
		}
	}
	return Server{
		start:    st,
		shutdown: sh,
	}, nil
}

// Sentinel is a sentinel server group that manages servers and related ignore
// error handlers.
type Sentinel struct {
	servers []Server
	ignore  []func(error) bool
	started bool
	sync.Mutex
}

// New creates a new sentinel server group.
func New(opts ...Option) (*Sentinel, error) {
	s := new(Sentinel)
	for _, o := range opts {
		if err := o(s); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Run starts the sentinel server group.
func (s *Sentinel) Run(ctx context.Context, timeout time.Duration, sigs ...os.Signal) error {
	s.Lock()
	if s.started {
		defer s.Unlock()
		return ErrAlreadyStarted
	}
	s.started = true
	s.Unlock()

	eg, ctx := errgroup.WithContext(ctx)

	// add servers
	for _, server := range s.servers {
		if server.start == nil {
			continue
		}
		eg.Go(func(f func(context.Context) error) func() error {
			return func() error {
				return f(ctx)
			}
		}(server.start))
	}

	// add shutdown
	eg.Go(func() func() error {
		if sigs == nil {
			sigs = []os.Signal{os.Interrupt}
		}
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, sigs...)
		return func() error {
			Logf("received signal: %v", <-sig)
			return s.ShutdownWithTimeout(ctx, timeout)
		}
	}())

	if err := eg.Wait(); err != nil && !s.shutdownIgnore(err) {
		return err
	}

	return nil
}

// ShutdownWithTimeout calls all registered shutdown funcs.
func (s *Sentinel) ShutdownWithTimeout(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var firstErr error
	for i, server := range s.servers {
		if server.shutdown == nil {
			continue
		}
		if err := server.shutdown(ctx); err != nil {
			Errorf("could not shutdown %d: %v", i, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Shutdown calls all registered shutdown funcs.
func (s *Sentinel) Shutdown() error {
	return s.ShutdownWithTimeout(context.Background(), 10*time.Second)
}

// shutdownIgnore returns if any of the registered ignore error handlers
// reported true.
func (s *Sentinel) shutdownIgnore(err error) bool {
	if err == nil {
		return true
	}
	for _, f := range s.ignore {
		if z := f(err); z {
			return true
		}
	}
	return false
}

// Register registers a server to the sentinel group, a related (optional)
// shutdown func, and any ignore error handlers.
func (s *Sentinel) Register(start, shutdown interface{}, ignore ...func(error) bool) error {
	// add servers, shutdowns, ignores
	server, err := NewServer(start, shutdown)
	if err != nil {
		return err
	}
	s.servers = append(s.servers, server)
	s.ignore = append(s.ignore, ignore...)
	return nil
}

// HTTP creates a HTTP server for the specified listener and handler, and
// registers the created HTTP server, its shutdown, and related ignore funcs
// (IgnoreServerClosed, IgnoreNetOpError) with the server sentinel group.
func (s *Sentinel) HTTP(listener net.Listener, handler http.Handler, opts ...func(*http.Server) error) error {
	s.Lock()
	defer s.Unlock()
	if s.started {
		return ErrAlreadyStarted
	}

	var err error
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

// IgnoreError returns a func that returns true when passed errors match.
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

// Option is a sentinel server group option.
type Option = func(*Sentinel) error

// Register is a sentinel server group option to register a server and an
// associated shutdown handler.
//
// Both server and shutdown can have a type of `func()`, `func() error`, or
// `func(context.Context) error`.
func Register(servers ...Server) Option {
	return func(s *Sentinel) error {
		s.Lock()
		defer s.Unlock()
		if s.started {
			return ErrAlreadyStarted
		}
		s.servers = append(s.servers, servers...)
		return nil
	}
}

// RegisterServer is a sentinel server group option to add servers without any
// associated shutdown handlers.
//
// Any server can have a type of `func()`, `func() error`, or
// `func(context.Context) error`.
func RegisterServer(starts ...interface{}) Option {
	return func(s *Sentinel) error {
		var servers []Server
		for _, f := range starts {
			start, err := convertContextFunc(f)
			if err != nil {
				return err
			}
			servers = append(servers, Server{start: start})
		}
		return Register(servers...)(s)
	}
}

// RegisterShutdown is a sentinel server group option to add a shutdown
// handlers without any associated servers.
//
// Any shutdown listener can have a type of `func()`, `func() error`, or
// `func(context.Context) error`.
func RegisterShutdown(shutdowns ...interface{}) Option {
	return func(s *Sentinel) error {
		var servers []Server
		for _, f := range shutdowns {
			shutdown, err := convertContextFunc(f)
			if err != nil {
				return err
			}
			servers = append(servers, Server{shutdown: shutdown})
		}
		return Register(servers...)(s)
	}
}

// RegisterIgnore is a sentinel server group option to add ignore error handlers.
func RegisterIgnore(ignore ...func(error) bool) Option {
	return func(s *Sentinel) error {
		s.Lock()
		defer s.Unlock()
		if s.started {
			return ErrAlreadyStarted
		}
		s.ignore = append(s.ignore, ignore...)
		return nil
	}
}

// ErrAlreadyStarted is the already started error.
var ErrAlreadyStarted = errors.New("already started")

// convertContextFunc converts an interface{} to a func(context.Context) error if possible.
func convertContextFunc(v interface{}) (func(context.Context) error, error) {
	switch f := v.(type) {
	case func(context.Context) error:
		return f, nil
	case func():
		return func(context.Context) error {
			f()
			return nil
		}, nil
	case func() error:
		return func(context.Context) error {
			return f()
		}, nil
	}
	return nil, errors.New("invalid type")
}
