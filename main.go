package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/germanoeich/nirn-proxy/lib"
	"github.com/hashicorp/memberlist"
	_ "github.com/joho/godotenv/autoload"
	"github.com/rabbitmq/amqp091-go"
	"github.com/sirupsen/logrus"
)

var logger = logrus.New()

// token : queue map
var bufferSize = 50

func setupLogger() {
	logLevel := lib.EnvGet("LOG_LEVEL", "info")
	lvl, err := logrus.ParseLevel(logLevel)

	if err != nil {
		panic("Failed to parse log level")
	}

	logger.SetLevel(lvl)
	lib.SetLogger(logger)
}

func initCluster(proxyPort string, manager *lib.QueueManager) *memberlist.Memberlist {
	port := lib.EnvGetInt("CLUSTER_PORT", 7946)

	memberEnv := os.Getenv("CLUSTER_MEMBERS")
	dns := os.Getenv("CLUSTER_DNS")

	if memberEnv == "" && dns == "" {
		logger.Info("Running in stand-alone mode")
		return nil
	}

	logger.Info("Attempting to create/join cluster")
	var members []string
	if memberEnv != "" {
		members = strings.Split(memberEnv, ",")
	} else {
		ips, err := net.LookupIP(dns)
		if err != nil {
			logger.Panic(err)
		}

		if len(ips) == 0 {
			logger.Panic("no ips returned by dns")
		}

		for _, ip := range ips {
			members = append(members, ip.String())
		}
	}

	return lib.InitMemberList(members, port, proxyPort, manager)
}

func main() {
	outboundIp := os.Getenv("OUTBOUND_IP")

	timeout := lib.EnvGetInt("REQUEST_TIMEOUT", 5000)

	disableHttp2 := lib.EnvGetBool("DISABLE_HTTP_2", true)

	globalOverrides := lib.EnvGet("BOT_RATELIMIT_OVERRIDES", "")

	disableGlobalRatelimitDetection := lib.EnvGetBool("DISABLE_GLOBAL_RATELIMIT_DETECTION", false)

	lib.ConfigureDiscordHTTPClient(outboundIp, time.Duration(timeout)*time.Millisecond, disableHttp2, globalOverrides, disableGlobalRatelimitDetection)

	port := lib.EnvGet("PORT", "8080")
	bindIp := lib.EnvGet("BIND_IP", "0.0.0.0")

	setupLogger()

	bufferSize = lib.EnvGetInt("BUFFER_SIZE", 50)
	maxBearerLruSize := lib.EnvGetInt("MAX_BEARER_COUNT", 1024)

	manager := lib.NewQueueManager(bufferSize, maxBearerLruSize)

	connectionString := lib.EnvGet("CONNECTION_STRING", "amqp://localhost")
	exchange := lib.EnvGet("EXCHANGE", "rest")
	retryExchange := lib.EnvGet("RETRY_EXCHANGE", "restRetry")
	requestQueue := lib.EnvGet("REQUEST_QUEUE", "restRequestsQueue")
	retryQueue := lib.EnvGet("RETRY_QUEUE", "restRetryQueue")
	queueArgs := amqp091.Table{
		"x-dead-letter-exchange": retryExchange,
	}
	retryArgs := amqp091.Table{
		"x-message-ttl":          int32(1000),
		"x-dead-letter-exchange": exchange,
	}

	conn, err := amqp091.Dial(connectionString)
	if err != nil {
		log.Fatalf("Failed to connect to RabbitMQ: %s", err)
	}

	ch, err := conn.Channel()
	if err != nil {
		log.Fatalf("Failed to open a channel: %s", err)
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

	rQ, err := ch.QueueDeclare(retryQueue, true, false, false, false, retryArgs)
	if err != nil {
		log.Fatalf("Failed to declare Retry Queue: %s", err)
	}

	log.Printf("Queue %s & %s was declared", q.Name, rQ.Name)

	if ch == nil {
		log.Fatalf("No channel")
		return
	}

	msgs, err := ch.Consume(requestQueue, "", false, false, false, false, nil)
	if err != nil {
		log.Fatalf("Failed to register a consumer: %s", err)
	}

	go func() {
		for msg := range msgs {
			request := lib.NewRequest(msg)
			response := lib.NewResponse(ch, request)

			manager.DiscordRequestHandler(request, response)
		}
	}()

	mux := manager.CreateMux()

	s := &http.Server{
		Addr:              bindIp + ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       10 * time.Second,
		WriteTimeout:      1 * time.Hour,
		MaxHeaderBytes:    1 << 20,
	}

	if os.Getenv("ENABLE_PPROF") == "true" {
		go lib.StartProfileServer()
	}

	if os.Getenv("ENABLE_METRICS") != "false" {
		port := lib.EnvGet("METRICS_PORT", "9000")
		go lib.StartMetrics(bindIp + ":" + port)
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.WithFields(logrus.Fields{"function": "http.ListenAndServe"}).Panic(err)
		}
	}()

	logger.Info("Started proxy on " + bindIp + ":" + port)

	// Wait for the http server to ready before joining the cluster
	<-time.After(1 * time.Second)
	initCluster(port, manager)

	<-done
	logger.Info("Server received shutdown signal")

	logger.Info("Broadcasting leave message to cluster, if in cluster mode")
	manager.Shutdown()

	logger.Info("Gracefully shutting down HTTP server")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := s.Shutdown(ctx); err != nil {
		logger.WithFields(logrus.Fields{"function": "http.Shutdown"}).Error(err)
	}

	logger.Info("Bye bye")
}
