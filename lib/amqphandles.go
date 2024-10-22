package lib

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"

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
	Body    json.RawMessage   `json:"body"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
}

type BodyObject struct {
	Content string `json:"content"`
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
	var rabbitRequest RabbitRequest

	err := json.Unmarshal(rabbitMessage.Body, &rabbitRequest)
	if err != nil {
		log.Fatalf("%s", err)
	}

	headers := make(http.Header)

	for key, value := range rabbitRequest.Headers {
		headers.Add(key, value)
	}

	parsedUrl, err := url.Parse(rabbitRequest.Path)

	var body io.ReadCloser

	switch headers.Get("Content-Type") {
	case "application/json":
		// Handle body as JSON object
		var bodyObject BodyObject
		if err := json.Unmarshal(rabbitRequest.Body, &bodyObject); err == nil {
			bodyBytes, _ := json.Marshal(bodyObject)
			body = io.NopCloser(bytes.NewReader(bodyBytes))
			// fmt.Println("Body as JSON object:", string(bodyBytes))
		} else {
			fmt.Println("Error parsing JSON body:", err)

		}

	case "application/x-www-form-urlencoded":
		// Handle body as form-urlencoded string
		var bodyString string
		if err := json.Unmarshal(rabbitRequest.Body, &bodyString); err == nil {
			formData, err := url.ParseQuery(bodyString) // Parse the form-urlencoded string
			if err != nil {
				fmt.Println("Error parsing form-urlencoded body:", err)

			}
			body = io.NopCloser(strings.NewReader(formData.Encode()))
			// fmt.Println("Body as form-urlencoded:", formData)
		} else {
			fmt.Println("Error parsing body as form-urlencoded string:", err)

		}

	default:
		fmt.Println("Unsupported Content-Type:", headers.Get("Content-Type"))
	}

	r := &Request{
		Body:          body,
		Method:        rabbitRequest.Method,
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
	if r.Request.ReplyTo == "" || r.Request.CorrelationId == "" || len(r.Request.ReplyTo) < 2 || len(r.Request.CorrelationId) < 2 {
		r.Request.Ack()
		return
	}

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
