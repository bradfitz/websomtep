// This was quick & sloppy demo code to have a little fun at the
// inaugural GoSF meetup (http://www.meetup.com/golangsf/).
//
// I release it under the public domain. It comes with no warranty whatsoever.
// Have fun.
//
// Author: Brad Fitzpatrick <brad@danga.com>

package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"html"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/mail"
	"strings"
	"sync"

	"code.google.com/p/go-smtpd/smtpd"
	"code.google.com/p/go.net/websocket"
)

var (
	webListen  = flag.String("listen", ":8081", "address to listen for HTTP/WebSockets on")
	smtpListen = flag.String("smtp", ":2500", "address to listen for SMTP on")
	domain     = flag.String("domain", "websomtep.danga.com", "required domain name in RCPT lines")
	wsAddr     = flag.String("ws", "websomtep.danga.com", "websocket host[:port], as seen by JavaScript")
	debug      = flag.Bool("debug", false, "enable debug features")
)

// Message implements smtpd.Envelope by streaming the message to all
// connected websocket clients.
type Message struct {
	// HTML-escaped fields sent to the client
	From, To string
	Subject  string
	Body     string // includes images (via data URLs)

	// internal state
	images []image
	bodies []string
	buf    bytes.Buffer // for accumulating email as it comes in
	msg    interface{}  // alternate message to send
}

// Stat is a JSON status message sent to clients when the number
// of connected WebSocket clients change.
type Stat struct {
	NumClients int
}

// SMTPStat is a JSON status message sent to clients when the number
// of connected SMTP clients change.
type SMTPStat struct {
	NumSenders int
}

type image struct {
	Type string
	Data []byte
}

func (m *Message) parse(r io.Reader) error {
	msg, err := mail.ReadMessage(r)
	if err != nil {
		return err
	}
	m.Subject = msg.Header.Get("Subject")
	m.To = msg.Header.Get("To")

	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		slurp, _ := ioutil.ReadAll(msg.Body)
		m.Body = string(slurp)
		return nil
	}
	if err := m.parseMultipart(msg.Body, params["boundary"]); err != nil {
		return err
	}
	// If we didn't find a text/plain body, pick the first body we did find.
	if m.Body == "" {
		for _, body := range m.bodies {
			if body != "" {
				m.Body = body
				break
			}
		}
	}
	return nil
}

// parseMultipart populates Body (preferring text/plain) and images,
// and may call itself recursively, walking through multipart/mixed
// and multipart/alternative innards.
func (m *Message) parseMultipart(r io.Reader, boundary string) error {
	mr := multipart.NewReader(r, boundary)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		partType, partParams, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if strings.HasPrefix(partType, "multipart/") {
			err = m.parseMultipart(part, partParams["boundary"])
			if err != nil {
				log.Printf("in boundary %q, returning error for multipart child %q: %v", boundary, partParams["boundary"], err)
				return err
			}
			continue
		}
		if strings.HasPrefix(partType, "image/") {
			if !strings.HasPrefix(part.Header.Get("Content-Disposition"), "attachment") {
				continue
			}
			if part.Header.Get("Content-Transfer-Encoding") != "base64" {
				continue
			}
			slurp, _ := ioutil.ReadAll(part)
			imdata, err := ioutil.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(removeNewlines(slurp))))
			if err != nil {
				log.Printf("image base64 decode error: %v", err)
				ioutil.WriteFile("/tmp/base64err", slurp, 0600)
				continue
			}
			m.images = append(m.images, image{
				Type: partType,
				Data: imdata,
			})
			continue
		}
		if !strings.HasPrefix(partType, "text/") {
			continue
		}
		slurp, _ := ioutil.ReadAll(part)
		if partType == "text/plain" {
			m.Body = string(slurp)
		} else {
			m.bodies = append(m.bodies, string(slurp))
		}
	}
	return nil
}

func removeNewlines(p []byte) []byte {
	return bytes.Map(func(r rune) rune {
		switch r {
		case '\n', '\r':
			return -1
		}
		return r
	}, p)
}

func (m *Message) AddRecipient(rcpt smtpd.MailAddress) error {
	m.To = strings.ToLower(rcpt.Email())
	if !strings.HasSuffix(m.To, "@"+*domain) {
		return errors.New("Invalid recipient domain")
	}
	return nil
}

func (m *Message) BeginData() error { return nil }

const maxMessageSize = 5 << 20

func (m *Message) Write(line []byte) error {
	m.buf.Write(line)
	if m.buf.Len() > maxMessageSize {
		return errors.New("too big, yo")
	}
	return nil
}

func (m *Message) Close() error {
	log.Printf("Got message: %q", m.buf.String())
	ioutil.WriteFile("/tmp/lastmsg", m.buf.Bytes(), 0600)
	if err := m.parse(&m.buf); err != nil {
		return err
	}

	// This is a lame place to do this, but this is a dumb hack,
	// so whatever.
	for _, field := range []*string{&m.From, &m.To, &m.Subject, &m.Body} {
		*field = html.EscapeString(*field)
	}
	m.Body = strings.Replace(m.Body, "\n", "<br>\n", -1)

	for _, im := range m.images {
		m.Body = m.Body + fmt.Sprintf("<p><img src='data:%s;base64,%s'></p>", im.Type, base64.StdEncoding.EncodeToString(im.Data))
	}
	broadcast(m)
	if *debug {
		backlog = append(backlog, m)
	}
	return nil
}

var backlog []*Message

func resend(w http.ResponseWriter, r *http.Request) {
	l := len(backlog)
	if l == 0 {
		return
	}
	m := backlog[l-1]
	for _, c := range clients() {
		c.Deliver(m)
	}
}

type Client chan *Message

func (c Client) Deliver(m *Message) {
	select {
	case c <- m:
	default:
		// Client is too backlogged. They don't get this message.
	}
}

var (
	mu        sync.Mutex // guards clientMap
	clientMap = map[Client]bool{}
)

func register(c Client) {
	mu.Lock()
	clientMap[c] = true
	n := len(clientMap)
	mu.Unlock()
	broadcast(&Message{msg: &Stat{NumClients: n}})
}

func unregister(c Client) {
	mu.Lock()
	delete(clientMap, c)
	n := len(clientMap)
	mu.Unlock()
	broadcast(&Message{msg: &Stat{NumClients: n}})
}

// clients returns all connected clients.
func clients() (cs []Client) {
	mu.Lock()
	defer mu.Unlock()
	for c := range clientMap {
		cs = append(cs, c)
	}
	return
}

func broadcast(m *Message) {
	for _, c := range clients() {
		c.Deliver(m)
	}
}

func streamMail(ws *websocket.Conn) {
	log.Printf("websocket connection from %v", ws.RemoteAddr())
	client := Client(make(chan *Message, 100))
	register(client)
	defer unregister(client)

	deadc := make(chan bool, 1)

	// Wait for incoming messages. Don't really care about them, but
	// use this to find out if client goes away.
	go func() {
		var msg Message
		for {
			err := websocket.JSON.Receive(ws, &msg)
			switch err {
			case nil:
				log.Printf("Unexpected message from %v: %+v", ws.RemoteAddr(), msg)
				continue
			case io.EOF:
			default:
				log.Printf("Receive error from %v: %v", ws.RemoteAddr(), err)
			}
			deadc <- true
		}
	}()

	for {
		select {
		case <-deadc:
			return
		case m := <-client:
			var err error
			if m.msg != nil {
				err = websocket.JSON.Send(ws, m.msg)
			} else {
				err = websocket.JSON.Send(ws, m)
			}
			if err != nil {
				return
			}
		}
	}
}

var uiTemplate = template.Must(template.ParseFiles("ui.html"))

type uiTemplateData struct {
	WSAddr string
	Domain string
}

func home(w http.ResponseWriter, r *http.Request) {
	var err error
	if *debug {
		uiTemplate, err = template.ParseFiles("ui.html")
		if err != nil {
			fmt.Fprint(w, err)
			return
		}
	}
	err = uiTemplate.Execute(w, uiTemplateData{
		WSAddr: *wsAddr,
		Domain: *domain,
	})
	if err != nil {
		log.Println(err)
	}
}

// countingListener tracks how many outstanding connections are open
// from l, running fn on changel
type countingListener struct {
	net.Listener
	fn func(int)
	mu sync.Mutex // guards n
	n  int
}

func (cl *countingListener) inc(delta int) {
	cl.mu.Lock()
	cl.n += delta
	defer cl.fn(cl.n)
	cl.mu.Unlock()
}

func (cl *countingListener) Accept() (c net.Conn, err error) {
	c, err = cl.Listener.Accept()
	if err == nil {
		cl.inc(1)
		c = &watchCloseConn{c, cl}
	}
	return
}

type watchCloseConn struct {
	net.Conn
	cl *countingListener
}

func (w *watchCloseConn) Close() error {
	if cl := w.cl; cl != nil {
		cl.inc(-1)
		w.cl = nil
	}
	return w.Conn.Close()
}

func main() {
	flag.Parse()

	http.HandleFunc("/", home)
	if *debug {
		http.HandleFunc("/resend", resend)
	}
	http.Handle("/stream", websocket.Handler(streamMail))

	sln, err := net.Listen("tcp", *smtpListen)
	if err != nil {
		log.Fatalf("error listening for SMTP: %v", err)
	}

	log.Printf("websomtep listening for HTTP on %q and SMTP on %q\n", *webListen, *smtpListen)
	go http.ListenAndServe(*webListen, nil)

	s := &smtpd.Server{
		OnNewMail: func(c smtpd.Connection, from smtpd.MailAddress) (smtpd.Envelope, error) {
			log.Printf("New message from %q", from)
			e := &Message{
				From: from.Email(),
			}
			return e, nil
		},
	}

	smtpCountListener := &countingListener{
		Listener: sln,
		fn: func(count int) {
			broadcast(&Message{msg: &SMTPStat{NumSenders: count}})
		},
	}

	err = s.Serve(smtpCountListener)
	if err != nil {
		log.Fatalf("ListenAndServe: %v", err)
	}
}
