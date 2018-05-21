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

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not create listener: %v", err)
	}

	h := &http.Server{
		Handler: http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
			fmt.Fprint(res, "foobar")
		}),
	}

	s, err := New(
		Server(func() error {
			return h.Serve(l)
		}),
		Shutdown(h.Shutdown),
		Ignore(IgnoreServerClosed, IgnoreNetOpError),
	)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	go func() {
		res, err := grab(t, "http://"+l.Addr().String())
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if res != "foobar" {
			t.Errorf("expected body %q, got: %q", "foobar", res)
		}
		s.sig <- syscall.SIGINT
	}()

	ctxt, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

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

func TestNewAndHTTP(t *testing.T) {
	t.Parallel()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not create listener: %v", err)
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

	go func() {
		res, err := grab(t, "http://"+l.Addr().String())
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if res != "foobar" {
			t.Errorf("expected body %q, got: %q", "foobar", res)
		}
		s.sig <- syscall.SIGINT
	}()

	ctxt, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

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

func TestMultiHTTP(t *testing.T) {
	t.Parallel()

	s, err := New()
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// create 100 servers
	servers := make([]net.Listener, 100)
	for i := 0; i < 100; i++ {
		servers[i], err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("could not create listener: %v", err)
		}
		err = s.HTTP(servers[i], func(i int) http.Handler {
			return http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
				fmt.Fprintf(res, "foo%d", i)
			})
		}(i))
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	}

	ctxt, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for i, server := range servers {
		go func(i int, l net.Listener) {
			res, err := grab(t, "http://"+l.Addr().String())
			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
			if exp := fmt.Sprintf("foo%d", i); res != exp {
				t.Errorf("expected body %q, got: %q", exp, res)
			}
		}(i, server)
	}

	go func() {
		<-time.After(5 * time.Second)
		s.sig <- syscall.SIGINT
	}()

	if err = s.Run(ctxt); err != nil {
		t.Errorf("expected no error, got: %v", err)
	}

	// check that the server has been shutdown
	for _, server := range servers {
		_, err = grab(t, "http://"+server.Addr().String())
		if err == nil {
			t.Errorf("expected error")
		} else {
			t.Logf("received error %v", err)
		}
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
