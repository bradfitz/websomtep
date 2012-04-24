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
	"io"
	"io/ioutil"
	"log"
	"mime"
	"mime/multipart"
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
	buf    bytes.Buffer // for accumulating email as it comes in
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

	log.Printf("Parsing message with subject %q", m.Subject)

	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || (mediaType != "multipart/alternative" && mediaType != "multipart/mixed") {
		slurp, _ := ioutil.ReadAll(msg.Body)
		m.Body = string(slurp)
		return nil
	}
	// boundary
	mr := multipart.NewReader(msg.Body, params["boundary"])
	lastBody := ""
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		partType, partParams, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		log.Printf("MIME part type %q, params: %#v", partType, partParams)
		if strings.HasPrefix(partType, "image/") && strings.HasPrefix(part.Header.Get("Content-Disposition"), "attachment") &&
			part.Header.Get("Content-Transfer-Encoding") == "base64" {
			slurp, _ := ioutil.ReadAll(part)
			slurp = bytes.Map(func(r rune) rune {
				switch r {
				case '\n', '\r':
					return -1
				}
				return r
			}, slurp)
			imdata, err := ioutil.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(slurp)))
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
		slurp, _ := ioutil.ReadAll(part)
		if partType == "text/plain" {
			m.Body = string(slurp)
		} else {
			lastBody = string(slurp)
		}

	}
	// If we didn't find a text/plain alternative section, just use whatever we last saw.
	if m.Body == "" {
		m.Body = lastBody
	}

	return nil
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
	for _, im := range m.images {
		m.Body = m.Body + fmt.Sprintf("<p><img src='data:%s;base64,%s'></p>", im.Type, base64.StdEncoding.EncodeToString(im.Data))
	}
	for _, c := range clients() {
		c.Deliver(m)
	}
	return nil
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
	defer mu.Unlock()
	clientMap[c] = true
}

func unregister(c Client) {
	mu.Lock()
	defer mu.Unlock()
	delete(clientMap, c)
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
			err := websocket.JSON.Send(ws, m)
			if err != nil {
				return
			}
		}
	}
}

func home(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, `<html>
<head>
<script type="text/javascript">
var ws;
function init() {
  console.log("init.");
  if (ws != null) {
     ws.close();
     ws = null;
  }
  ws = new WebSocket("ws://`+*wsAddr+`/stream");
  var div = document.getElementById("mail");
  div.innerText = "(mail goes here; connecting to websocket server...)";
  ws.onopen = function () {
     div.innerText = "(connected; waiting for email...)\n";
  };
  ws.onmessage = function (e) {
     var m = JSON.parse(e.data);
     var md = document.createElement("div");
     md.innerHTML = "<table style='margin-bottom: 2em'>" +
		"<tr><td align=right><b>From:</b></td><td>" + m.From + "</td></tr>" +
		"<tr><td align=right><b>To:</b></td><td>" + m.To + "</td></tr>" +
		"<tr><td align=right><b>Subject:</b></td><td>" + m.Subject + "</td></tr>" +
		"<tr valign=top><td align=right><b>Body:</b></td><td>" + m.Body + "</td></tr>" +
                "</table>";
     div.insertBefore(md, div.firstChild)
   };
  ws.onclose = function (e) {
     div.innerHTML = "<div>(connection closed)</div>" + div.innerHTML;
  };
}
</script>
</head>
<body onLoad="init();">
<h1>websomtep -- websockets + SMTP</h1>
<p>This is an SMTP server written in <a href="http://golang.org/">Go</a> which streams incoming mail to your (and everybody else's) active WebSocket connection.</p>
<p>Test it! Email <a href="mailto:whatever@websomtep.danga.com"><b><i>whatever</i>@websomtep.danga.com</b></a>.</p>
<p>Source code is here: <a href="https://github.com/bradfitz/websomtep">https://github.com/bradfitz/websomtep</a>.</p>
<h2>Mail:</h2>
<div id='mail'></div>
</body>
			</html>`)
}

func main() {
	flag.Parse()

	http.HandleFunc("/", home)
	http.Handle("/stream", websocket.Handler(streamMail))
	log.Printf("websomtep listening for HTTP on %q and SMTP on %q\n", *webListen, *smtpListen)
	go http.ListenAndServe(*webListen, nil)

	s := &smtpd.Server{
		Addr: *smtpListen,
		OnNewMail: func(c smtpd.Connection, from smtpd.MailAddress) (smtpd.Envelope, error) {
			log.Printf("New message from %q", from)
			e := &Message{
				From: from.Email(),
			}
			return e, nil
		},
	}
	err := s.ListenAndServe()
	if err != nil {
		log.Fatalf("ListenAndServe: %v", err)
	}
}
