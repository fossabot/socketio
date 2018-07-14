package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/url"
	"strings"

	"github.com/valyala/fasthttp"
)

type FastTransport interface {
	Name() string
	Dial(rawurl string) (conn Conn, err error)
	Accept(ctx *fasthttp.RequestCtx) (conn Conn, err error)
}

func getFastTransport(name string) FastTransport {
	switch name {
	case transportWebsocket:
		return nil
	case transportPolling:
		return FastPollingTransport
	}
	return nil
}

type fastPollingAcceptor struct{}

func (fastPollingAcceptor) Accept(ctx *fasthttp.RequestCtx) (conn Conn, err error) {
	return newPollingConn(8, string(ctx.Host()), ctx.RemoteAddr().String()), nil
}

type fastPollingTransport struct{ fastPollingAcceptor }

func (fastPollingTransport) Name() string {
	return transportPolling
}

func (fastPollingTransport) Dial(rawurl string) (Conn, error) {
	return nil, errors.New("not implemented")
}

// FastPollingTransport is a Transport instance for polling
var FastPollingTransport FastTransport = &fastPollingTransport{}

type fasthttpRequestHandler interface {
	HandleFastHTTP(ctx *fasthttp.RequestCtx)
}

// HandleFastHTTP implements fasthttp.RequestHandler
func (s *Socket) HandleFastHTTP(ctx *fasthttp.RequestCtx) {
	if handler, ok := s.Conn.(fasthttpRequestHandler); ok {
		handler.HandleFastHTTP(ctx)
	}
}

func (p *pollingConn) HandleFastHTTP(ctx *fasthttp.RequestCtx) {
	if p.isClosed() {
		ctx.Error(ErrPollingConnClosed.Error(), fasthttp.StatusBadRequest)
		return
	}
	switch string(ctx.Method()) {
	case "GET":
		pkt, err := p.ReadPacketOut(context.Background())
		if err != nil {
			ctx.Error(err.Error(), fasthttp.StatusNotFound)
			return
		}
		rURL, err := url.ParseRequestURI(string(ctx.RequestURI()))
		if err != nil {
			ctx.Error(err.Error(), fasthttp.StatusBadRequest)
			return
		}
		q := rURL.Query()
		b64 := q.Get(queryBase64)
		if jsonp := q.Get(queryJSONP); jsonp != "" {
			err = fastWriteJSONP(ctx, jsonp, pkt)
		} else if b64 == "1" {
			err = fastWriteXHR(ctx, pkt)
		} else {
			err = fastWriteXHR2(ctx, pkt.packet2())
		}
		if err != nil {
			log.Println("polling:", err.Error())
		}
	case "POST":
		var payload Payload
		mediatype, params, err := mime.ParseMediaType(string(ctx.Request.Header.Peek("Content-Type")))
		if err != nil {
			ctx.Error(err.Error(), fasthttp.StatusBadRequest)
			return
		}
		switch mediatype {
		case "application/octet-stream":
			payload.xhr2 = true
		case "text/plain":
			if strings.ToLower(params["charset"]) != "utf-8" {
				ctx.Error("invalid charset", fasthttp.StatusBadRequest)
				return
			}
		default:
			ctx.Error("invalid media type", fasthttp.StatusBadRequest)
			return
		}
		_, err = payload.ReadFrom(bytes.NewReader(ctx.Request.Body()))
		if err != nil {
			ctx.Error(err.Error(), fasthttp.StatusInternalServerError)
			return
		}
		for i := range payload.packets {
			select {
			case <-p.closed:
				ctx.Error("closed", fasthttp.StatusNotFound)
				return
			case p.in <- &payload.packets[i]:
			}
		}
		ctx.Error("OK", fasthttp.StatusOK)
	default:
		ctx.Error("error", fasthttp.StatusMethodNotAllowed)
	}
}

func fastWriteJSONP(ctx *fasthttp.RequestCtx, jsonp string, wt io.WriterTo) error {
	var buf bytes.Buffer
	ctx.Response.Header.Set("Content-Type", "text/javascript; charset=UTF-8")
	if _, err := wt.WriteTo(&buf); err != nil {
		return err
	}
	s := buf.String()
	buf.Reset()
	err := json.NewEncoder(&buf).Encode(s)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(ctx, `___eio[%s]("%s");`, jsonp, buf.String())
	ctx.SetStatusCode(fasthttp.StatusOK)
	return err
}

func fastWriteXHR(ctx *fasthttp.RequestCtx, wt io.WriterTo) error {
	ctx.Response.Header.Set("Content-Type", "text/plain; charset=UTF-8")
	if _, err := wt.WriteTo(ctx); err != nil {
		return err
	}
	ctx.SetStatusCode(fasthttp.StatusOK)
	return nil
}

func fastWriteXHR2(ctx *fasthttp.RequestCtx, wt io.WriterTo) error {
	ctx.Response.Header.Set("Content-Type", "application/octet-stream")
	if _, err := wt.WriteTo(ctx); err != nil {
		return err
	}
	ctx.SetStatusCode(fasthttp.StatusOK)
	return nil
}

func (s *Server) HandleFastHTTP(ctx *fasthttp.RequestCtx) {
	rURL, err := url.ParseRequestURI(string(ctx.RequestURI()))
	if err != nil {
		ctx.Error(err.Error(), fasthttp.StatusBadRequest)
		return
	}
	q := rURL.Query()

	if q.Get(queryEIO) != Version {
		ctx.Error("protocol version incompatible", fasthttp.StatusBadRequest)
		return
	}

	transport := getFastTransport(q.Get(queryTransport))
	if transport == nil {
		ctx.Error("invalid transport", fasthttp.StatusBadRequest)
		return
	}

	if sid := q.Get(querySession); sid == "" {
		conn, err := transport.Accept(ctx)
		if err != nil {
			ctx.Error(err.Error(), fasthttp.StatusInternalServerError)
			return
		}
		ß := s.NewSession(conn, s.pingTimeout+s.pingInterval, s.pingTimeout)
		ß.transportName = transport.Name()
		select {
		case <-s.done:
			return
		case s.ßchan <- ß:
		}
		ß.HandleFastHTTP(ctx)
	} else {
		ß, exists := s.sessionManager.Get(sid)
		if !exists {
			ctx.Error("invalid session", fasthttp.StatusBadRequest)
			return
		}
		if transportName := transport.Name(); ß.transportName != transportName {
			conn, err := transport.Accept(ctx)
			if err != nil {
				ctx.Error(err.Error(), fasthttp.StatusInternalServerError)
				return
			}
			s.upgrade(ß, transportName, conn)
		}
		ß.HandleFastHTTP(ctx)
	}
}
