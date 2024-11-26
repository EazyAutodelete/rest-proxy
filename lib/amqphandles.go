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
	"time"

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

type Response struct {
	Request *Request
	Status  int
	Body    []byte
	Channel *amqp091.Channel
	header  http.Header
}

func removeUrlCredentials(rawURL string) string {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		fmt.Println("Invalid URL:", err)
		return rawURL
	}
	userInfo := parsedURL.User.Username()
	if userInfo != "" {
		userInfo += ":****@"
	}

	loggedURL := fmt.Sprintf("%s://%s%s%s", parsedURL.Scheme, userInfo, parsedURL.Host, parsedURL.RequestURI())
	loggedURL = strings.TrimRight(loggedURL, "/")

	return loggedURL
}

func ConnectRabbitMQ() (*amqp091.Connection, error) {
	var conn *amqp091.Connection
	var err error

	queueUser := EnvGet("QUEUE_USER", "guest")
	queuePass := EnvGet("QUEUE_PASSWORD", "guest")
	rawQueueHosts := EnvGet("QUEUE_HOSTS", "localhost:5672")
	queueHostStrings := strings.Split(rawQueueHosts, ",")

	queueHosts := make([]string, len(queueHostStrings))
	for i, host := range queueHostStrings {
		queueHosts[i] = fmt.Sprintf("amqp://%s:%s@%s", queueUser, queuePass, host)
	}

	for {
		for _, url := range queueHosts {
			log.Printf("Trying to connect to RabbitMQ at %s...", removeUrlCredentials(url))
			conn, err = amqp091.Dial(url)
			if err == nil {
				log.Printf("Connected to RabbitMQ at %s", removeUrlCredentials(url))
				return conn, nil
			}
			log.Printf("Failed to connect to RabbitMQ at %s: %s", removeUrlCredentials(url), err)
		}

		log.Printf("Failed to connect to RabbitMQ, retrying in 1 second")
		time.Sleep(1 * time.Second)
	}
}

func SetupRabbitMQConnection() *amqp091.Connection {
	for {
		conn, err := ConnectRabbitMQ()
		if err != nil {
			log.Printf("Failed to connect to RabbitMQ cluster: %s", err)
			time.Sleep(1 * time.Second)
			continue
		}

		go func(c *amqp091.Connection) {
			<-c.NotifyClose(make(chan *amqp091.Error))
			log.Println("RabbitMQ connection closed. Attempting to reconnect...")
			SetupRabbitMQConnection()
		}(conn)

		return conn
	}
}

func PrepareRabbitMQ(conn *amqp091.Connection) (*amqp091.Channel, <-chan amqp091.Delivery) {
	exchange := EnvGet("EXCHANGE", "rest")
	retryExchange := EnvGet("RETRY_EXCHANGE", "restRetry")
	requestQueue := EnvGet("REQUEST_QUEUE", "restRequestsQueue")
	retryQueue := EnvGet("RETRY_QUEUE", "restRetryQueue")
	queueArgs := amqp091.Table{
		"x-dead-letter-exchange": retryExchange,
	}
	retryArgs := amqp091.Table{
		"x-message-ttl":          int32(1000),
		"x-dead-letter-exchange": exchange,
	}

	ch, err := conn.Channel()
	if err != nil {
		log.Fatalf("Failed to open a channel: %s", err)
	}

	err = ch.Qos(500, 0, false)
	if err != nil {
		log.Fatalf("Failed to set QoS: %s", err)
	}

	err = ch.ExchangeDeclare(exchange, "direct", true, false, false, false, nil)
	if err != nil {
		log.Fatalf("Failed to declare exchange: %s", err)
	}

	err = ch.ExchangeDeclare(retryExchange, "direct", true, false, false, false, nil)
	if err != nil {
		log.Fatalf("Failed to declare Retry Exchange: %s", err)
	}

	q, err := ch.QueueDeclare(requestQueue, true, false, false, false, queueArgs)
	if err != nil {
		log.Fatalf("Failed to declare queue: %s", err)
	}

	err = ch.QueueBind(q.Name, requestQueue, exchange, false, nil)
	if err != nil {
		log.Fatalf("Failed to bind queue: %s", err)
	}

	_, err = ch.QueueDeclare(retryQueue, true, false, false, false, retryArgs)
	if err != nil {
		log.Fatalf("Failed to declare Retry Queue: %s", err)
	}

	if ch == nil {
		log.Fatalf("No channel")
		return ch, nil
	}

	msgs, err := ch.Consume(requestQueue, "", false, false, false, false, nil)
	if err != nil {
		log.Fatalf("Failed to register a consumer: %s", err)
	}

	return ch, msgs
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

	if headers.Get("User-Agent") == "" {
		headers.Add("User-Agent", "DiscordBot (https://github.com/EazyAutodelete/rest-proxy, v2.3.3)")
	}

	authHeader := headers.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bot ") && !strings.HasPrefix(authHeader, "Bearer ") {
		headers.Set("Authorization", "Bot "+authHeader)
	}

	if !strings.HasPrefix(rabbitRequest.Path, "/") {
		rabbitRequest.Path = "/" + rabbitRequest.Path
	}

	// if !strings.HasPrefix(rabbitRequest.Path, "/api") {
	// 	rabbitRequest.Path = "/api" + rabbitRequest.Path
	// }

	if len(rabbitRequest.Path) == 0 {
		rabbitRequest.Path = "/"
	}

	parsedUrl, err := url.Parse(rabbitRequest.Path)

	var body io.ReadCloser

	if rabbitRequest.Method == "" {
		rabbitRequest.Method = "GET"
	}

	if len(rabbitRequest.Body) > 0 && rabbitRequest.Method != "GET" {
		switch headers.Get("Content-Type") {
		case "application/json":
			var bodyObject map[string]interface{}
			if err := json.Unmarshal(rabbitRequest.Body, &bodyObject); err == nil {
				bodyBytes, _ := json.Marshal(bodyObject)
				body = io.NopCloser(bytes.NewReader(bodyBytes))

			} else {
				fmt.Println("Error parsing JSON body:", err, string(rabbitRequest.Body))
			}

		case "application/x-www-form-urlencoded":
			var bodyString string
			if err := json.Unmarshal(rabbitRequest.Body, &bodyString); err == nil {
				formData, err := url.ParseQuery(bodyString)
				if err != nil {
					fmt.Println("Error parsing form-urlencoded body:", err)
				}

				body = io.NopCloser(strings.NewReader(formData.Encode()))

			} else {
				fmt.Println("Error parsing body as form-urlencoded string:", err)

			}

		default:
			fmt.Println("Unsupported Content-Type:", headers.Get("Content-Type"))
		}
	} else {
		body = nil
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
