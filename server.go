/*
 * Copyright (c) 2016 Moriyoshi Koizumi
 * Copyright (c) 2012 Elazar Leibovich.
 * Copyright (c) 2012 The Go Authors.
 *
 * All rights reserved.
 *
 * Redistribution and use in source and binary forms, with or without
 * modification, are permitted provided that the following conditions are
 * met:
 *
 *    * Redistributions of source code must retain the above copyright
 * notice, this list of conditions and the following disclaimer.
 *    * Redistributions in binary form must reproduce the above
 * copyright notice, this list of conditions and the following disclaimer
 * in the documentation and/or other materials provided with the
 * distribution.
 *    * Neither the name of Elazar Leibovich. nor the names of its
 * contributors may be used to endorse or promote products derived from
 * this software without specific prior written permission.
 *
 * THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
 * "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
 * LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
 * A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
 * OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
 * SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
 * LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
 * DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
 * THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
 * (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
 * OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
 */
package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"github.com/Sirupsen/logrus"
	"io"
	"io/ioutil"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
	"unsafe"
)

type OurProxyHttpServer struct {
	Ctx              *DevProxy
	Logger           *logrus.Logger
	Tr               *http.Transport
	TLSConfigFactory TLSConfigFactory
	SessionSerial    int64
}

type OurProxyCtx struct {
	Proxy    *OurProxyHttpServer
	Logger   *logrus.Logger
	OrigReq  *http.Request
	Req      *http.Request
	OrigResp *http.Response
	Resp     *http.Response
	Tr       *http.Transport
	Error    error
	Session  int64
}

func makeHttp10Response(header string, body string) string {
	return header + fmt.Sprintf("Content-Length: %d\r\n", len(body)) + "Connection: close\r\n\r\n" + body
}

var contentTypeKey = http.CanonicalHeaderKey("Content-Type")

var http10BadGatewayBytes = []byte(makeHttp10Response("HTTP/1.0 502 Bad Gateway", "<html><body><h1>Bad Gateway</h1></body></html>"))
var http10OkBytes = []byte("HTTP/1.0 200 OK\r\n\r\n")

func (proxyCtx *OurProxyCtx) NewTLSConfig(r *http.Request) (*tls.Config, error) {
	return proxyCtx.Proxy.TLSConfigFactory(r.URL.Host, proxyCtx)
}

func (proxyCtx *OurProxyCtx) FilterRequest(r *http.Request) (req *http.Request, resp *http.Response) {
	for _, perHostConfig := range proxyCtx.Proxy.Ctx.Config.Hosts {
		if proxyCtx.Proxy.Ctx.checkIfRequestMatchesToUrl(perHostConfig.Host, false, r, proxyCtx) {
			return FilterRequest(perHostConfig, r, proxyCtx)
		}
	}
	return r, nil
}

func (proxyCtx *OurProxyCtx) FilterResponse(resp *http.Response) *http.Response {
	if proxyCtx.OrigReq != proxyCtx.Req {
		if resp == nil {
			return resp
		}
		contentType, ok := resp.Header[contentTypeKey]
		if !ok || len(contentType) == 0 {
			return resp
		}
		mimeType, params, err := mime.ParseMediaType(contentType[0])
		if err != nil {
			proxyCtx.Logger.Warnf("invalid Content-Type header: %s", contentType[0])
			return resp
		}
		applicable := false
		for _, v := range proxyCtx.Proxy.Ctx.HTMLMediaTypes {
			if v == mimeType {
				applicable = true
				break
			}
		}
		if !applicable {
			return resp
		}
		charset, ok := params["charset"]
		if !ok {
			charset = "UTF-8"
		}
		if charset == "UTF-16" {
			return resp
		}
		if unsafe.Sizeof(resp.ContentLength) != unsafe.Sizeof(int(0)) {
			if resp.ContentLength > math.MaxInt32 {
				proxyCtx.Logger.Errorf("failed to read response body (%d expected)", resp.ContentLength)
			}
		}
		contentLength := int(resp.ContentLength)
		body := []byte{}
		defer resp.Body.Close()
		if contentLength >= 0 {
			body = make([]byte, contentLength)
			n, err := io.ReadFull(resp.Body, body)
			if err != nil || (err == io.EOF && n < contentLength) {
				proxyCtx.Logger.Errorf("failed to read response body (%d bytes read, %d bytes expected): %s", n, contentLength, err.Error())
			}
		} else {
			err := error(nil)
			body, err = ioutil.ReadAll(resp.Body)
			if err != nil {
				proxyCtx.Logger.Errorf("failed to read response body: %s", err.Error())
			}
		}
		p := bytes.LastIndex(body, proxyCtx.Proxy.Ctx.InsertBefore)
		if p > 0 {
			newBody := make([]byte, 0, len(body)+len(proxyCtx.Proxy.Ctx.LabelHTML))
			newBody = append(newBody, body[0:p]...)
			newBody = append(newBody, proxyCtx.Proxy.Ctx.LabelHTML...)
			newBody = append(newBody, body[p:]...)
			body = newBody
		}
		newResp := new(http.Response)
		*newResp = *resp
		newResp.Body = ioutil.NopCloser(bytes.NewBuffer(body))
		return newResp
	} else {
		return resp
	}
}

func (proxy *OurProxyHttpServer) HandleNonProxyRequest(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "This is a proxy server. Does not respond to non-proxy requests.", 400)
}

/* --- BEGIN pasted from src/net/http/client.go -- */
// cancelTimerBody is an io.ReadCloser that wraps rc with two features:
// 1) on Read EOF or Close, the timer t is Stopped,
// 2) On Read failure, if reqWasCanceled is true, the error is wrapped and
//    marked as net.Error that hit its timeout.
type cancelTimerBody struct {
	t        *time.Timer
	rc       io.ReadCloser
	canceled *int32
}

func (b *cancelTimerBody) Read(p []byte) (n int, err error) {
	n, err = b.rc.Read(p)
	if err == io.EOF {
		b.t.Stop()
	} else if err != nil && atomic.LoadInt32(b.canceled) != 0 {
		return n, fmt.Errorf("%s: timeout exceeded", err.Error())
	}
	return
}

func (b *cancelTimerBody) Close() error {
	err := b.rc.Close()
	b.t.Stop()
	return err
}

/* --- END pasted from src/net/http/client.go -- */

func (proxyCtx *OurProxyCtx) DoRequest(req *http.Request, timeout time.Duration) (*http.Response, error) {
	defer func() {
		if req.Body != nil {
			req.Body.Close()
		}
	}()

	timer := (*time.Timer)(nil)
	canceled := int32(0)

	if timeout > 0 {
		timer = time.AfterFunc(timeout, func() {
			atomic.StoreInt32(&canceled, 1)
			proxyCtx.Tr.CancelRequest(req)
		})
	}

	resp, err := proxyCtx.Tr.RoundTrip(req)
	if err != nil {
		if atomic.LoadInt32(&canceled) != 0 {
			return nil, fmt.Errorf("%s: timeout exceeded", err.Error())
		}
		return nil, err
	}

	if timer != nil {
		resp.Body = &cancelTimerBody{
			t:        timer,
			rc:       resp.Body,
			canceled: &canceled,
		}
	}
	return resp, nil
}

func (proxyCtx *OurProxyCtx) HandleConnect(r *http.Request, proxyClient net.Conn) {
	connClosers := []func() error{proxyClient.Close}
	defer func() {
		i := len(connClosers)
		for {
			i--
			if i < 0 {
				break
			}
			connCloser := connClosers[i]
			err := connCloser()
			if err != nil {
				proxyCtx.Logger.Warnf("failed to close connection (%s)", err.Error())
			}
		}
	}()

	var targetHostPort string
	{
		pair := splitHostPort(r.URL.Host)
		if pair.Port == "" {
			pair.Port = "443" // this may be a bad assumption
		}
		targetHostPort = pair.String()
	}

	for _, perHostConfig := range proxyCtx.Proxy.Ctx.Config.Hosts {
		if proxyCtx.Proxy.Ctx.checkIfTunnelRequestMatchesToUrl(perHostConfig.Host, r, proxyCtx) {
			tlsConfig, err := proxyCtx.NewTLSConfig(r)
			if err != nil {
				proxyCtx.Logger.Errorf("failed to create tls.Config (%s)", err.Error())
				if _, err := proxyClient.Write(http10BadGatewayBytes); err != nil {
					proxyCtx.Logger.Errorf("failed to send response to client (%s)", err.Error())
				}
				return
			}

			_, err = proxyClient.Write(http10OkBytes)
			if err != nil {
				proxyCtx.Logger.Errorf("failed to send response to client (%s)", err.Error())
				return
			}
			clientTlsConn := tls.Server(proxyClient, tlsConfig)
			err = clientTlsConn.Handshake()
			if err != nil {
				proxyCtx.Logger.Errorf("TLS handshake with the client failed (%s)", err.Error())
				return
			}
			connClosers[0] = clientTlsConn.Close

			clientBufReader := bufio.NewReader(clientTlsConn)
			req, err := http.ReadRequest(clientBufReader)
			if err != nil {
				proxyCtx.Logger.Errorf("failed to read request from the target (%s)", err.Error())
				if _, err := clientTlsConn.Write(http10BadGatewayBytes); err != nil {
					proxyCtx.Logger.Errorf("failed to send response to client (%s)", err.Error())
				}
				return
			}

			nestedProxyCtx := new(OurProxyCtx)
			*nestedProxyCtx = *proxyCtx
			nestedProxyCtx.OrigReq = req
			nestedProxyCtx.Req = req

			req.URL.Scheme = "https"
			req.URL.Host = targetHostPort
			req.RequestURI = req.URL.String()
			req, resp := FilterRequest(perHostConfig, req, nestedProxyCtx)
			nestedProxyCtx.Req = req

			if resp == nil {
				removeProxyHeaders(req)
				resp, err = nestedProxyCtx.DoRequest(req, 0)
				if err != nil {
					nestedProxyCtx.Logger.Errorf("failed to read response from the target (%s)", err.Error())
					if _, err := clientTlsConn.Write(http10BadGatewayBytes); err != nil {
						nestedProxyCtx.Logger.Errorf("failed to send response to client (%s)", err.Error())
					}
					return
				}
			}
			nestedProxyCtx.OrigResp = resp
			resp = nestedProxyCtx.FilterResponse(resp)
			resp.ContentLength = -1
			if resp.Header != nil {
				resp.Header.Del("Content-Length")
			}
			err = resp.Write(clientTlsConn)
			if err != nil {
				nestedProxyCtx.Logger.Errorf("failed to send response to the client (%s)", err.Error())
			}
			return
		}
	}

	proxyCtx.Logger.Debugf("Connecting to %s", targetHostPort)
	targetConn, err := proxyCtx.Proxy.ConnectDial("tcp", targetHostPort)
	if err != nil {
		proxyCtx.Logger.Errorf("failed to connect to %s (%s)", targetHostPort, err.Error())
		if _, err := proxyClient.Write(http10BadGatewayBytes); err != nil {
			proxyCtx.Logger.Errorf("failed to send response to client (%s)", err.Error())
		}
		return
	}
	connClosers = append(connClosers, targetConn.Close)
	_, err = proxyClient.Write(http10OkBytes)
	if err != nil {
		proxyCtx.Logger.Errorf("failed to send response to client (%s)", err.Error())
		return
	}
	proxyCtx.Proxy.Ctx.bidiTunnel(proxyClient, targetConn)
}

func (proxyCtx *OurProxyCtx) SendToClient(w http.ResponseWriter, resp *http.Response) {
	dest := w.Header()
	for headerName, headerValues := range resp.Header {
		dest[headerName] = headerValues
	}
	w.WriteHeader(resp.StatusCode)
	nr, err := io.Copy(w, resp.Body)
	if err := resp.Body.Close(); err != nil {
		proxyCtx.Logger.Warnf("Can't close response body %v", err)
	}
	proxyCtx.Logger.Debugf("Copied %v bytes to client error=%v", nr, err)
}

func (proxy *OurProxyHttpServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	proxyCtx := &OurProxyCtx{
		Proxy:    proxy,
		Logger:   proxy.Logger,
		OrigReq:  r,
		Req:      r,
		OrigResp: nil,
		Resp:     nil,
		Session:  atomic.AddInt64(&proxy.SessionSerial, 1),
		Tr:       proxy.Tr,
	}
	if r.Method == "CONNECT" {
		hij, ok := w.(http.Hijacker)
		if !ok {
			proxy.Logger.Error("Connection hijacking is not supported")
			http.Error(w, "CONNECT not supported", 400)
			return
		}
		proxyClient, _, err := hij.Hijack()
		if err != nil {
			proxy.Logger.Errorf("Failed to hijack the connection (%s)", err.Error())
			http.Error(w, "CONNECT not supported", 400)
		}
		proxyCtx.HandleConnect(r, proxyClient)
	} else {
		var err error
		proxy.Logger.Debugf("Got request %v %v %v %v", r.URL.Path, r.Host, r.Method, r.URL.String())
		if !r.URL.IsAbs() {
			proxy.HandleNonProxyRequest(w, r)
			return
		}
		r, resp := proxyCtx.FilterRequest(r)
		proxyCtx.Req = r
		if resp == nil {
			removeProxyHeaders(r)
			resp, err = proxyCtx.DoRequest(r, 0)
			if err != nil {
				proxyCtx.Error = err
				resp = proxyCtx.FilterResponse(nil)
				if resp == nil {
					proxyCtx.Logger.Errorf("error read response %v %v:", r.URL.Host, err.Error())
					http.Error(w, err.Error(), 500)
					return
				}
			}
			proxyCtx.Logger.Debugf("Received response %v", resp.Status)
		}
		proxyCtx.OrigResp = resp
		resp = proxyCtx.FilterResponse(resp)
		resp.ContentLength = -1
		if resp.Header != nil {
			resp.Header.Del("Content-Length")
		}
		proxyCtx.Logger.Debugf("Copying response to client %v [%d]", resp.Status, resp.StatusCode)
		proxyCtx.SendToClient(w, resp)
	}
}

/* copy and pasted from github.com/elazarl/goproxy */
func (proxy *OurProxyHttpServer) ConnectDial(network, addr string) (net.Conn, error) {
	if proxy.Tr.Dial != nil {
		return proxy.Tr.Dial(network, addr)
	}
	return net.Dial(network, addr)
}

func CloneHeader(orig http.Header) http.Header {
	newHeader := make(http.Header)
	for headerName, headerValues := range orig {
		header := make([]string, len(headerValues))
		for i, headerValue := range headerValues {
			header[i] = headerValue
		}
		newHeader[headerName] = header
	}
	return newHeader
}

func CloneRequest(r *http.Request) *http.Request {
	newRequest := new(http.Request)
	*newRequest = *r
	newRequest.URL = new(url.URL)
	*newRequest.URL = *r.URL
	if r.Header != nil {
		newRequest.Header = CloneHeader(r.Header)
	}
	if r.Trailer != nil {
		newRequest.Trailer = CloneHeader(r.Trailer)
	}
	return newRequest
}

func FilterRequest(perHostConfig *PerHostConfig, r *http.Request, proxyCtx *OurProxyCtx) (*http.Request, *http.Response) {
	newUrlString := ""
	for _, pattern := range perHostConfig.Patterns {
		submatchIndexes := pattern.Pattern.FindStringSubmatchIndex(r.URL.Path)
		if submatchIndexes != nil {
			proxyCtx.Logger.Debugf("%s matched to pattern %s", r.URL.Path, pattern.Pattern.String())
			newUrlString = string(pattern.Pattern.ExpandString(
				make([]byte, 0, len(pattern.Substitution)+len(r.URL.Path)),
				pattern.Substitution,
				r.URL.Path,
				submatchIndexes,
			))
			break
		}
	}
	if newUrlString != "" {
		newUrl, err := url.Parse(newUrlString)
		if err != nil {
			proxyCtx.Logger.Error(err)
		}
		newUrl.User = r.URL.User
		newRequest := CloneRequest(r)
		newRequest.URL = newUrl
		proxyCtx.Logger.Infof("%s %s => %s", r.Method, r.RequestURI, newRequest.URL.String())
		return newRequest, nil
	} else {
		proxyCtx.Logger.Infof("%s %s", r.Method, r.RequestURI)
		return r, nil
	}
}

func removeProxyHeaders(r *http.Request) {
	r.RequestURI = ""
	r.Header.Del("Accept-Encoding")
	r.Header.Del("Proxy-Connection")
	r.Header.Del("Proxy-Authenticate")
	r.Header.Del("Proxy-Authorization")
	r.Header.Del("Connection")
}
