package websocket_test

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/websocket"
)

func ExampleAccept() {
	handler := func(ctx *fasthttp.RequestCtx) {
		err := websocket.Accept(ctx, nil, func(c *websocket.Conn) {
			for {
				typ, msg, err := c.Read(c.Context())
				if err != nil {
					return
				}
				if err := c.Write(c.Context(), typ, msg); err != nil {
					return
				}
			}
		})
		if err != nil {
			log.Printf("websocket: %v", err)
		}
	}
	log.Fatal(fasthttp.ListenAndServe(":8080", handler))
}

func ExampleDial() {
	ctx := context.Background()
	opts := &websocket.DialOptions{ReadTimeout: time.Minute, WriteTimeout: 10 * time.Second}
	c, err := websocket.Dial(ctx, "wss://example.com/echo", opts)
	if err != nil {
		log.Println(err)
		return
	}
	defer c.Close(ctx, websocket.StatusNormalClosure, "")
	if err := c.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
		log.Println(err)
		return
	}
	_, msg, err := c.Read(ctx)
	if err != nil {
		log.Println(err)
		return
	}
	fmt.Printf("%s\n", msg)
}
