package main

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"path"
	"reflect"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/filecoin-project/go-jsonrpc"

	"github.com/gorilla/websocket"
	"golang.org/x/xerrors"
)

type ParamEncoder func(reflect.Value) (reflect.Value, error)

type backoff struct {
	minDelay time.Duration
	maxDelay time.Duration
}

type Config struct {
	reconnectBackoff backoff
	pingInterval     time.Duration
	timeout          time.Duration

	paramEncoders map[reflect.Type]ParamEncoder

	noReconnect      bool
	proxyConnFactory func(func() (*websocket.Conn, error)) func() (*websocket.Conn, error) // for testing
}

type Option func(c *Config)

func WithParamEncoder(t interface{}, encoder ParamEncoder) func(c *Config) {
	return func(c *Config) {
		c.paramEncoders[reflect.TypeOf(t).Elem()] = encoder
	}
}

type resType int

type ReaderStream struct {
	Type StreamType
	Info string
}
type readRes struct {
	rt   resType
	meta string
}

const (
	resStart    resType = iota // send on first read after HEAD
	resRedirect                // send on redirect before first read after HEAD
	resError
	// done/closed = close res channel
)

type RpcReader struct {
	postBody     io.ReadCloser   // nil on initial head request
	next         chan *RpcReader // on head will get us the postBody after sending resStart
	mustRedirect bool
	eof          bool

	res       chan readRes
	beginOnce *sync.Once
	closeOnce sync.Once
}

type StreamType string

const (
	Null       StreamType = "null"
	PushStream StreamType = "push"
	// TODO: Data transfer handoff to workers?
)

func (w *RpcReader) beginPost() {
	if w.mustRedirect {
		w.res <- readRes{
			rt: resError,
		}
		close(w.res)
		return
	}

	if w.postBody == nil {
		w.res <- readRes{
			rt: resStart,
		}

		nr := <-w.next

		w.postBody = nr.postBody
		w.res = nr.res
		w.beginOnce = nr.beginOnce
	}
}

var ErrMustRedirect = errors.New("reader can't be read directly; marked as MustRedirect")

func (w *RpcReader) Read(p []byte) (int, error) {
	w.beginOnce.Do(func() {
		w.beginPost()
	})

	if w.eof {
		return 0, io.EOF
	}

	if w.mustRedirect {
		return 0, ErrMustRedirect
	}

	if w.postBody == nil {
		return 0, xerrors.Errorf("reader already closed or redirected")
	}

	n, err := w.postBody.Read(p)
	if err != nil {
		if err == io.EOF {
			w.eof = true
		}
		w.closeOnce.Do(func() {
			close(w.res)
		})
	}
	return n, err
}

func (w *RpcReader) Close() error {
	w.beginOnce.Do(func() {})
	w.closeOnce.Do(func() {
		close(w.res)
	})
	if w.postBody == nil {
		return nil
	}
	return w.postBody.Close()
}

func (w *RpcReader) redirect(to string) bool {
	if w.postBody != nil {
		return false
	}

	done := false

	w.beginOnce.Do(func() {
		w.closeOnce.Do(func() {
			w.res <- readRes{
				rt:   resRedirect,
				meta: to,
			}

			done = true
			close(w.res)
		})
	})

	return done
}

var client = func() *http.Client {
	c := *http.DefaultClient
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &c
}()

type Reader interface {
	Read(p []byte) (n int, err error)
}

type LimitedReader struct {
	R Reader // underlying reader
	N int64  // max bytes remaining
}

type NullReader struct {
	*io.LimitedReader
}

func ReaderParamEncoder(addr string) jsonrpc.Option {
	// Client side parameter encoder. Runs on the rpc client side. io.Reader -> ReaderStream{}
	return jsonrpc.WithParamEncoder(new(io.Reader), func(value reflect.Value) (reflect.Value, error) {
		r := value.Interface().(io.Reader)

		if r, ok := r.(*NullReader); ok {
			return reflect.ValueOf(ReaderStream{Type: Null, Info: fmt.Sprint(r.N)}), nil
		}

		reqID := uuid.New()
		u, err := url.Parse(addr)
		if err != nil {
			return reflect.Value{}, xerrors.Errorf("parsing push address: %w", err)
		}
		u.Path = path.Join(u.Path, reqID.String())

		rpcReader, redir := r.(*RpcReader)
		if redir {
			// if we have an rpc stream, redirect instead of proxying all the data
			redir = rpcReader.redirect(u.String())
		}

		if !redir {
			go func() {
				// TODO: figure out errors here
				for {
					req, err := http.NewRequest("HEAD", u.String(), nil)
					if err != nil {
						fmt.Println("sending HEAD request for the reder param: %+v", err)
						return
					}
					req.Header.Set("Content-Type", "application/octet-stream")
					resp, err := client.Do(req)
					if err != nil {
						fmt.Println("sending reader param: %+v", err)
						return
					}
					// todo do we need to close the body for a head request?

					if resp.StatusCode == http.StatusFound {
						nextStr := resp.Header.Get("Location")
						u, err = url.Parse(nextStr)
						if err != nil {
							fmt.Println("sending HEAD request for the reder param, parsing next url (%s): %+v", nextStr, err)
							return
						}

						continue
					}

					if resp.StatusCode == http.StatusNoContent { // reader closed before reading anything
						// todo just return??
						return
					}

					if resp.StatusCode != http.StatusOK {
						b, _ := ioutil.ReadAll(resp.Body)
						fmt.Println("sending reader param (%s): non-200 status: %s, msg: '%s'", u.String(), resp.Status, string(b))
						return
					}

					break
				}

				// now actually send the data
				req, err := http.NewRequest("POST", u.String(), r)
				if err != nil {
					fmt.Println("sending reader param: %+v", err)
					return
				}
				req.Header.Set("Content-Type", "application/octet-stream")
				resp, err := client.Do(req)
				if err != nil {
					fmt.Println("sending reader param: %+v", err)
					return
				}

				defer resp.Body.Close() //nolint

				if resp.StatusCode != http.StatusOK {
					b, _ := ioutil.ReadAll(resp.Body)
					fmt.Println("sending reader param (%s): non-200 status: %s, msg: '%s'", u.String(), resp.Status, string(b))
					return
				}
			}()
		}

		return reflect.ValueOf(ReaderStream{Type: PushStream, Info: reqID.String()}), nil
	})

}

func getPushUrl(addr string) (string, error) {
	pushUrl, err := url.Parse(addr)
	if err != nil {
		return "", err
	}
	switch pushUrl.Scheme {
	case "ws":
		pushUrl.Scheme = "http"
	case "wss":
		pushUrl.Scheme = "https"
	}
	///rpc/v0 -> /rpc/streams/v0/push

	pushUrl.Path = path.Join(pushUrl.Path, "../streams/v0/push")
	return pushUrl.String(), nil
}
