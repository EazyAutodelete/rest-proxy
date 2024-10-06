package lib

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"

	"github.com/rabbitmq/amqp091-go"
)

type Request struct {
	Body          io.ReadCloser
	Method        string
	Header        http.Header
	ReplyTo       string
	CorrelationId string
	Message       amqp091.Delivery
	URL           *url.URL
	ctx           context.Context
}

type RabbitRequest struct {
	Body    map[string]interface{}
	Method  string
	Path    string
	Headers map[string]string
}

type Response struct {
	Request *Request
	Status  int
	Body    []byte
	Channel *amqp091.Channel
	header  http.Header
}

func (r *Response) Header() http.Header {
	return r.header
}

func NewRequest(rabbitMessage amqp091.Delivery) *Request {
	var body RabbitRequest

	err := json.Unmarshal(rabbitMessage.Body, &body)
	if err != nil {
		log.Fatalf("%s", err)
	}

	headers := make(http.Header)

	for key, value := range body.Headers {
		headers.Add(key, value)
	}

	parsedUrl, err := url.Parse(body.Path)

	jsonBody, err := json.Marshal(body.Body)
	buffer := bytes.NewBuffer(jsonBody)
	reader := io.NopCloser(buffer)

	if len(body.Body) == 0 {
		reader = nil
	}

	r := &Request{
		Body:          reader,
		Method:        body.Method,
		Header:        headers,
		CorrelationId: rabbitMessage.CorrelationId,
		ReplyTo:       rabbitMessage.ReplyTo,
		Message:       rabbitMessage,
		URL:           parsedUrl,
		ctx:           context.Background(),
	}

	return r
}

func (r *Request) Context() context.Context {
	return r.ctx
}

func (r *Request) Ack() {
	r.Message.Ack(false)
	r.Context()
}

func (r *Response) SetStatus(status int) {
	r.Status = status
}

func (r *Response) WriteBody(body []byte) {
	r.Body = body

	if r.Status == 204 && len(r.Body) > 0 {
		r.Status = 200
	}
}

func (r *Response) Send() {
	retBody := map[string]interface{}{
		"status": r.Status,
	}

	if r.Body != nil && len(r.Body) > 0 {
		retBody["body"] = string(r.Body)
	}

	bodyString, jErr := json.Marshal(retBody)
	if jErr != nil {
		log.Fatalf("Failed to marshal response: %s", jErr)
	}

	err := r.Channel.Publish("rest", r.Request.ReplyTo, false, false, amqp091.Publishing{
		ContentType:   "application/json",
		CorrelationId: r.Request.CorrelationId,
		Body:          bodyString,
	})
	if err != nil {
		log.Fatalf("Failed to publish a message: %s", err)
	} else {
		log.Println("Sent response")
		r.Request.Ack()
	}

}

func NewResponse(ch *amqp091.Channel, incoming *Request) *Response {
	return &Response{
		Request: incoming,
		Channel: ch,
		header:  make(http.Header),
	}
}
