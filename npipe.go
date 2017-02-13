package main

import (
	"context"
	"encoding/binary"
	"flag"
	"io"
	"log"
	"os"
	"os/exec"

	ninchat "github.com/ninchat/ninchat-go"
)

const (
	messageType = "github.com/tsavola/npipe"
	messageTTL  = 0.1
	maxPartSize = 32 * 1024
	maxSize     = 32 * maxPartSize
)

var (
	payloadHello = []ninchat.Frame{[]byte{}}
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		myUserId   string
		myUserAuth string
		peerUserId string
		channelId  string
		executable string
	)

	flag.StringVar(&myUserId, "user", myUserId, "login with existing user id (needs -auth)")
	flag.StringVar(&myUserAuth, "auth", myUserAuth, "user authentication token")
	flag.StringVar(&peerUserId, "peer", peerUserId, "peer user id (if dialogue peer is already known, or for filtering channel messages")
	flag.StringVar(&channelId, "channel", channelId, "use channel instead of dialogue (needs -user)")
	flag.StringVar(&executable, "exec", executable, "executable to run")
	flag.Parse()

	var (
		r io.Reader
		w io.Writer
	)

	if executable != "" {
		r, w = execute(ctx, cancel, executable, flag.Args())
	} else {
		r = os.Stdin
		w = os.Stdout
	}

	events := make(chan *ninchat.Event, 100)
	closed := make(chan struct{})

	session := &ninchat.Session{
		OnSessionEvent: func(event *ninchat.Event) {
			events <- event
		},

		OnEvent: func(event *ninchat.Event) {
			events <- event
		},

		OnClose: func() {
			close(closed)
		},

		OnLog: func(x ...interface{}) {
			log.Print(append([]interface{}{"client: "}, x...)...)
		},
	}

	params := map[string]interface{}{
		"message_types": []string{
			messageType,
		},
	}

	if myUserId != "" {
		params["user_id"] = myUserId
		params["user_auth"] = myUserAuth
	}

	session.SetParams(params)

	session.Open()

	defer func() {
		session.Close()

		for {
			select {
			case event := <-events:
				if event.String() == "error" {
					return
				}

			case <-closed:
				return
			}
		}
	}()

	defer cancel()

	var (
		sessionCreated bool
	)

	for {
		select {
		case event := <-events:
			switch event.String() {
			case "error":
				log.Printf("error event: %s", event.Params["error_type"])
				close(closed)
				return

			case "session_created":
				if sessionCreated {
					log.Print("session lost")
					return
				}

				sessionCreated = true
				myUserId, _ = event.Str("user_id")

				if channelId != "" {
					if myUserAuth != "" {
						go sendLoop(ctx, cancel, r, session, "", channelId)
					} else {
						follow(session, channelId)
					}
				} else if peerUserId != "" {
					send(session, peerUserId, "", false, 60, payloadHello)
					go sendLoop(ctx, cancel, r, session, peerUserId, "")
				} else {
					log.Printf("my user id: %s", myUserId)
				}

			case "message_received":
				if x, ok := event.Str("message_user_id"); ok && x == myUserId {
					// reply
					break
				}

				var forMe bool

				if channelId != "" {
					if x, _ := event.Str("channel_id"); x == channelId {
						if peerUserId == "" {
							forMe = true
						} else {
							x, _ := event.Str("message_user_id")
							forMe = (x == peerUserId)
						}
					}
				} else {
					if x, ok := event.Str("user_id"); ok {
						if peerUserId == "" {
							peerUserId = x
							go sendLoop(ctx, cancel, r, session, peerUserId, "")
						}

						forMe = (x == peerUserId)
					}
				}

				if forMe {
					for _, data := range event.Payload {
						if _, err := w.Write(data); err != nil {
							log.Printf("output: %v", err)
							return
						}
					}
				}
			}

		case <-ctx.Done():
			log.Print(ctx.Err())
			return
		}
	}
}

func sendLoop(ctx context.Context, cancel context.CancelFunc, r io.Reader, session *ninchat.Session, peerUserId, channelId string) {
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		header := make([]byte, 4)
		if _, err := io.ReadFull(r, header); err != nil {
			if err != io.EOF {
				log.Printf("input: %v", err)
			}
			return
		}

		size := binary.LittleEndian.Uint32(header)
		if size < 4 || size > maxSize {
			log.Printf("input: message size out of bounds: %d", size)
			return
		}

		buf := make([]byte, size)
		copy(buf, header)
		if _, err := io.ReadFull(r, buf[4:]); err != nil {
			log.Printf("input: %v", err)
			return
		}

		payload := make([]ninchat.Frame, (size+maxPartSize-1)/maxPartSize)
		for i := 0; i < len(payload); i++ {
			begin := i * maxPartSize
			end := begin + maxPartSize
			if end > len(buf) {
				end = len(buf)
			}
			payload[i] = buf[begin:end]
		}

		send(session, peerUserId, channelId, true, messageTTL, payload)
	}
}

func follow(session *ninchat.Session, channelId string) {
	session.Send(&ninchat.Action{
		Params: map[string]interface{}{
			"action":     "follow_channel",
			"channel_id": channelId,
		},
	})
}

func send(session *ninchat.Session, peerUserId, channelId string, fold bool, ttl interface{}, payload []ninchat.Frame) {
	params := map[string]interface{}{
		"action":       "send_message",
		"action_id":    nil,
		"message_type": messageType,
		"message_fold": fold,
		"message_ttl":  ttl,
	}

	if channelId != "" {
		params["channel_id"] = channelId
	} else {
		params["user_id"] = peerUserId
	}

	session.Send(&ninchat.Action{
		Params:  params,
		Payload: payload,
	})
}

func execute(ctx context.Context, cancel context.CancelFunc, executable string, args []string) (r io.Reader, w io.Writer) {
	r, send, err := os.Pipe()
	if err != nil {
		panic(err)
	}

	recv, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}

	env := os.Environ()
	env = append(env, "RECV_FD=3")
	env = append(env, "SEND_FD=4")

	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{recv, send}

	if err := cmd.Start(); err != nil {
		log.Fatalf("exec: %v", err)
	}

	recv.Close()
	send.Close()

	go func() {
		defer cancel()

		if err := cmd.Wait(); err != nil {
			log.Printf("exec: %v", err)
		}
	}()

	return
}
