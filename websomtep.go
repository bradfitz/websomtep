package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"

	"code.google.com/p/go-smtpd/smtpd"
	"code.google.com/p/go.net/websocket"
)

var (
	wsAddr = flag.String("ws", "websomtep.danga.com", "websocket host[:port]")
)

type env struct {
	*smtpd.BasicEnvelope
}

func (e *env) AddRecipient(rcpt smtpd.MailAddress) error {
	if strings.HasPrefix(rcpt.Email(), "bad@") {
		return errors.New("we don't send email to bad@")
	}
	return e.BasicEnvelope.AddRecipient(rcpt)
}

func onNewMail(c smtpd.Connection, from smtpd.MailAddress) (smtpd.Envelope, error) {
	log.Printf("ajas: new mail from %q", from)
	return &env{new(smtpd.BasicEnvelope)}, nil
}

func streamMail(ws *websocket.Conn) {
	log.Printf("websocket connection from %v", ws.RemoteAddr())
	type T struct {
		Foo string
		Bar string
	}
	websocket.JSON.Send(ws, &T{
		Foo: "123",
		Bar: "xyz",
	})
	for {
		var msg T
		err := websocket.JSON.Receive(ws, &msg)
		if err != nil {
			log.Printf("error reading from %v: %v", ws.RemoteAddr(), err)
			return
		}
		log.Printf("Got message from %v: %+v", ws.RemoteAddr(), msg)
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
