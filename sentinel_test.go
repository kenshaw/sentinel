package sentinel

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"
)

func TestNewAndRun(t *testing.T) {
	t.Parallel()

	h := &http.Server{
		Handler: http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
			fmt.Fprint(res, "foobar")
		}),
	}

	// listener address
	addr := new(string)
	s, err := New(
		Server(func() error {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return err
			}
			*addr = l.Addr().String()
			return h.Serve(l)
		}),
		Shutdown(h.Shutdown),
		Ignore(IgnoreServerClosed, IgnoreNetOpError),
	)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	ctxt, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go func() {
		<-time.After(1 * time.Second)
		res, err := grab(t, "http://"+*addr)
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if res != "foobar" {
			t.Errorf("expected body %q, got: %q", "foobar", res)
		}
		syscall.Kill(syscall.Getpid(), syscall.SIGINT)
	}()

	if err = s.Run(ctxt); err != nil {
		t.Errorf("expected no error, got: %v", err)
	}

	// check that the server has been shutdown
	_, err = grab(t, "http://"+*addr)
	if err == nil {
		t.Errorf("expected error")
	} else {
		t.Logf("received error %v", err)
	}

	err = s.Run(ctxt)
	if err != ErrAlreadyStarted {
		t.Errorf("expected already started error, got: %v", err)
	}

}
func TestNewAndHTTP(t *testing.T) {
	t.Parallel()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not create a listener: %v", err)
	}

	s, err := New()
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	err = s.HTTP(l, http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
		fmt.Fprint(res, "foobar")
	}))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	ctxt, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go func() {
		<-time.After(1 * time.Second)
		res, err := grab(t, "http://"+l.Addr().String())
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if res != "foobar" {
			t.Errorf("expected body %q, got: %q", "foobar", res)
		}
		syscall.Kill(syscall.Getpid(), syscall.SIGINT)
	}()

	if err = s.Run(ctxt); err != nil {
		t.Errorf("expected no error, got: %v", err)
	}

	// check that the server has been shutdown
	_, err = grab(t, "http://"+l.Addr().String())
	if err == nil {
		t.Errorf("expected error")
	} else {
		t.Logf("received error %v", err)
	}

	err = s.Run(ctxt)
	if err != ErrAlreadyStarted {
		t.Errorf("expected already started error, got: %v", err)
	}
}

// grab retrieves the body from the specified URL.
func grab(t *testing.T, urlstr string) (string, error) {
	t.Logf("retrieving %s", urlstr)

	req, err := http.NewRequest("GET", urlstr, nil)
	if err != nil {
		return "", err
	}

	cl := &http.Client{}
	res, err := cl.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	buf, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return "", err
	}

	t.Logf("body %s: %s", urlstr, string(buf))
	return string(buf), nil
}
