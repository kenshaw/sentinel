// Package sentinel provides a sentinel run group manager.
package sentinel

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// Logger is a logger interface.
//
// Allowed types:
//
// func(string, ...interface{})
// func(string, ...interface{}) error
// func(string)
// interface {
//   Printf(string, ...interface{})
// }
type Logger interface{}

// Sentinel is a sentinel run group manager.
type Sentinel struct {
	sigs     []os.Signal
	g        *errgroup.Group
	ctx      context.Context
	cancel   func()
	managers []Manager
	started  bool
	sync.Mutex
}

// WithContext creates a new sentinel run group manager.
func WithContext(ctx context.Context, sigs ...os.Signal) (*Sentinel, context.Context) {
	if sigs == nil {
		sigs = []os.Signal{os.Interrupt}
	}
	s := &Sentinel{
		sigs: sigs,
	}
	ctx, s.cancel = context.WithCancel(ctx)
	s.g, s.ctx = errgroup.WithContext(ctx)
	return s, ctx
}

// Manage creates and registers a manager to the sentinel for the provided
// start and shutdown funcs, adding any error ignores funcs to the run group.
func (s *Sentinel) Manage(start, shutdown interface{}, ignore ...func(error) bool) error {
	s.Lock()
	defer s.Unlock()
	if s.started {
		return ErrAlreadyStarted
	}
	manager, err := NewManager(start, shutdown, ignore...)
	if err != nil {
		return err
	}
	s.managers = append(s.managers, manager)
	return nil
}

// ManageHTTP creates and registers a manager for a HTTP server for the
// specified listener and handler, and registers the created HTTP server, its
// shutdown, and related ignore funcs (IgnoreServerClosed, IgnoreNetOpError)
// with the server sentinel group.
func (s *Sentinel) ManageHTTP(listener net.Listener, handler http.Handler, opts ...func(*http.Server) error) error {
	s.Lock()
	defer s.Unlock()
	if s.started {
		return ErrAlreadyStarted
	}
	manager, err := convhttp(listener, handler, opts)
	if err != nil {
		return err
	}
	s.managers = append(s.managers, manager)
	return nil
}

// Run runs the sentinel run group using the logger, and kill timeout.
func (s *Sentinel) Run(logger Logger, timeout time.Duration) error {
	logf, err := convlogf(logger)
	if err != nil {
		return err
	}
	s.Lock()
	if s.started {
		defer s.Unlock()
		return ErrAlreadyStarted
	}
	s.started = true
	s.Unlock()
	s.g.Go(func() func() error {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, s.sigs...)
		return func() error {
			logf("received signal: %v", <-sig)
			ctx, cancel := context.WithTimeout(s.ctx, timeout)
			defer cancel()
			return s.shutdown(ctx, logf)
		}
	}())
	for _, m := range s.managers {
		if m.start == nil {
			continue
		}
		s.g.Go(func(f func(context.Context) error) func() error {
			return func() error {
				return f(s.ctx)
			}
		}(m.start))
	}
	if err := s.g.Wait(); err != nil && !s.ignoreErr(err) {
		return err
	}
	return nil
}

// shutdown notifies all sentinel managers to shutdown.
func (s *Sentinel) shutdown(ctx context.Context, logf func(string, ...interface{})) error {
	defer s.cancel()
	var firstErr error
	for i, m := range s.managers {
		if m.shutdown == nil {
			continue
		}
		if err := m.shutdown(ctx); err != nil {
			logf("could not shutdown %d: %v", i, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// ignoreErr determines if an error should be ignored.
func (s *Sentinel) ignoreErr(err error) bool {
	if err == nil {
		return true
	}
	for _, m := range s.managers {
		for _, f := range m.ignore {
			if z := f(err); z {
				return true
			}
		}
	}
	return false
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

// Manager is a sentinel run manager holding the specific start and shutdown
// funcs for a sentinel.
type Manager struct {
	start    func(context.Context) error
	shutdown func(context.Context) error
	ignore   []func(error) bool
}

// NewManager creates a sentinel run manager for the provided start and
// shutdown funcs.
func NewManager(start, shutdown interface{}, ignore ...func(error) bool) (Manager, error) {
	var err error
	var st, sh func(context.Context) error
	if start != nil {
		st, err = convf(start)
		if err != nil {
			return Manager{}, err
		}
	}
	if shutdown != nil {
		sh, err = convf(shutdown)
		if err != nil {
			return Manager{}, err
		}
	}
	return Manager{
		start:    st,
		shutdown: sh,
		ignore:   ignore,
	}, nil
}

// Error is an error.
type Error string

// Error satisfies the error interface.
func (err Error) Error() string {
	return string(err)
}

const (
	// ErrAlreadyStarted is the already started error.
	ErrAlreadyStarted Error = "already started"

	// ErrUnknownType is the unknown type error
	ErrUnknownType Error = "unknown type"
)

// convf is a util func that wraps start and shutdown interfaces.
func convf(v interface{}) (func(context.Context) error, error) {
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
	return nil, ErrUnknownType
}

// convlogf is a util func that converts various loggers to standard logf func
// signature.
func convlogf(logger interface{}) (func(string, ...interface{}), error) {
	switch v := logger.(type) {
	case interface {
		Printf(string, ...interface{})
	}:
		return v.Printf, nil
	case func(string, ...interface{}):
		return v, nil
	case func(string, ...interface{}) (int, error):
		return func(s string, args ...interface{}) {
			v(s, args...)
		}, nil
	case func(string):
		return func(s string, args ...interface{}) {
			v(fmt.Sprintf(s, args...))
		}, nil
	}
	return nil, ErrUnknownType
}

// convhttp is a util func that converts http listener and handler to a manager.
func convhttp(l net.Listener, h http.Handler, opts []func(*http.Server) error) (Manager, error) {
	s := &http.Server{
		Handler: h,
	}
	for _, o := range opts {
		if err := o(s); err != nil {
			return Manager{}, err
		}
	}
	return Manager{
		start:    func(context.Context) error { return s.Serve(l) },
		shutdown: s.Shutdown,
		ignore:   []func(error) bool{IgnoreServerClosed, IgnoreNetOpError},
	}, nil
}
