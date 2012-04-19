package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync"

	"code.google.com/p/go-smtpd/smtpd"
	"code.google.com/p/go.net/websocket"
)

var (
	wsAddr = flag.String("ws", "websomtep.danga.com", "websocket host[:port]")
)


func onNewMail(c smtpd.Connection, from smtpd.MailAddress) (smtpd.Envelope, error) {
	log.Printf("ajas: new mail from %q", from)
	for _, c := range clients() {
		select {
		case c <- &Message{From: from.Email()}:
		default:
		}
	}
	e := &Message{
		From: from.Email(),
	}
	return e, nil
}

type Message struct {
	From, To string
	Subject  string
	Body     string
}

func (m *Message) AddRecipient(rcpt smtpd.MailAddress) error {
	m.To = rcpt.Email()
	return nil
}

func (m *Message) BeginData() error { return nil }

func (m *Message) Write(line []byte) error {
	m.Body += string(line) + "\n"
	return nil
}

func (m *Message) Close() error {
	for _, c := range clients() {
		select {
		case c <- m:
		default:
		}
	}
	return nil
}

var (
	mu sync.Mutex // guards clients
	clientMap = map[chan *Message]bool{}
)

func register(c chan *Message) {
	mu.Lock()
	defer mu.Unlock()
	clientMap[c] = true
}

func unregister(c chan *Message) {
	mu.Lock()
	defer mu.Unlock()
	delete(clientMap, c)
}

func clients() (cs []chan *Message) {
	mu.Lock()
        defer mu.Unlock()
	for c := range clientMap {
		cs = append(cs, c)
	}
	return
}

func streamMail(ws *websocket.Conn) {
	log.Printf("websocket connection from %v", ws.RemoteAddr())
	msgc := make(chan *Message, 100)
	register(msgc)
	defer unregister(msgc)

	deadc := make(chan bool, 2)

	websocket.JSON.Send(ws, &Message{
		Subject: "mail will appear appear",
	})

	// Wait for incoming messages. Don't really care about them, but
	// use this to find out if client goes away.
	go func() {
		var msg Message
		for {
			err := websocket.JSON.Receive(ws, &msg)
			if err != nil {
				log.Printf("Receive error from %v: %v", ws.RemoteAddr(), err)
				deadc <- true
				return
			}
			log.Printf("Got message from %v: %+v", ws.RemoteAddr(), msg)
		}
	}()

	for {
		select {
		case <-deadc:
			return
		case m := <-msgc:
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
  div.innerText = "(mail goes here)";
  ws.onopen = function () {
     div.innerText = "opened\n" + div.innerText;
  };
  ws.onmessage = function (e) {
     div.innerText = "msg:" + e.data + "\n" + div.innerText;
   };
  ws.onclose = function (e) {
     div.innerText = "closed\n" + div.innerText;
  };
  div.innerText = "did init.\n" + div.innerText;
}
</script>
</head>
<body onLoad="init();">
<h1>websomtep -- websockets + SMTP</h1>
<p>This is an SMTP server written in <a href="http://golang.org/">Go</a> which streams incoming mail to your (and everybody else's) active WebSocket connection.</p>
<p>Test it! Email <b><i>whatever</i>@websomtep.danga.com</b>.</p>
<h2>Mail:</h2>
<div id='mail'></div>
</body>
			</html>`)
}

func main() {
	flag.Parse()

	http.HandleFunc("/", home)
	http.Handle("/stream", websocket.Handler(streamMail))
	go http.ListenAndServe(":8081", nil)

	s := &smtpd.Server{
		Addr:      ":2500",
		OnNewMail: onNewMail,
	}
	err := s.ListenAndServe()
	if err != nil {
		log.Fatalf("ListenAndServe: %v", err)
	}
}
